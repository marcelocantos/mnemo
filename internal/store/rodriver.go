// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"strings"
	"sync/atomic"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// readOnlyDriverName is the SQLite driver mnemo opens its read pool with.
// It is the stock "sqlite3" driver plus a per-connection authorizer
// (readOnlyAuthorizer) that hardens the read-only boundary of
// mnemo_query, which runs arbitrary client SQL (including from mTLS
// federation peers).
//
// PRAGMA query_only=1 alone is not a sufficient boundary:
//   - a client can submit "PRAGMA query_only=0; <write>" to turn the
//     guard back off within one submission and execute writes — a full
//     DB wipe under the append-only/pruned-JSONL policy (🎯T103); and
//   - query_only never gates ATTACH DATABASE, so a client can read
//     arbitrary local SQLite files or create files at chosen paths
//     (🎯T106).
//
// The authorizer closes both holes at the connection level, where
// client SQL cannot undo it.
const readOnlyDriverName = "sqlite3_mnemo_ro"

func init() {
	sql.Register(readOnlyDriverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			conn.RegisterAuthorizer(readOnlyAuthorizer)
			return registerTextFunc(conn)
		},
	})
}

// readOnlyAuthorizer is the SQLite authorizer installed on every read-pool
// connection. Comprehensive write-blocking stays delegated to
// PRAGMA query_only=1 (set per query in Store.Query); this authorizer only
// guarantees that guard cannot be bypassed, by denying the two vectors
// query_only itself does not cover:
//
//   - ATTACH/DETACH — reads of arbitrary local SQLite files and file
//     creation at chosen paths (🎯T106); mnemo never legitimately
//     attaches a second database on the read path.
//   - turning query_only off — the client-resettable PRAGMA that makes
//     "PRAGMA query_only=0; <write>" execute writes (🎯T103).
//
// It fails closed: any query_only SET other than the exact "=1" mnemo
// itself issues is denied, while a bare "PRAGMA query_only" read and all
// other pragmas (journal_mode, synchronous, … set by openDB) pass.
func readOnlyAuthorizer(op int, arg1, arg2, arg3 string) int {
	switch op {
	case sqlite3.SQLITE_SELECT:
		readSelectCount.Add(1)
	case sqlite3.SQLITE_ATTACH, sqlite3.SQLITE_DETACH:
		return sqlite3.SQLITE_DENY
	case sqlite3.SQLITE_PRAGMA:
		if strings.EqualFold(arg1, "query_only") && arg2 != "" && arg2 != "1" {
			return sqlite3.SQLITE_DENY
		}
	}
	return sqlite3.SQLITE_OK
}

// readSelectCount counts SELECT authorisations on the read pool. SQLite
// consults the authorizer once per SELECT clause at prepare time (nested
// subselects included), and database/sql prepares afresh on every
// Query/QueryRow, so the count tracks statements issued rather than rows
// read. It exists for the perf ratchet: a search that issues one query
// per hit shows up here as hundreds of selects, and the ratchet pins the
// number so an N+1 cannot creep back in silently.
var readSelectCount atomic.Int64

// ReadSelectCount returns the process-wide number of SELECT authorisations
// on the read pool so far. Take a delta around the call under test; the
// absolute value is meaningless.
func ReadSelectCount() int64 { return readSelectCount.Load() }
