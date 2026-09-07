// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/marcelocantos/mnemo/internal/store"
)

// cmdDedupeEntries is the offline twin of mnemo_ops op=dedupe_entries
// (🎯T170).
//
// It exists because the pass has to be runnable against a store whose
// daemon is stopped: the survey is a window function over every keyed row
// and the deletes touch millions of rows on a large database, which is
// work to do with the writer to yourself rather than alongside live
// ingest. Stop the daemon, run this, start it again.
func cmdDedupeEntries(args []string) {
	fs := flag.NewFlagSet("dedupe-entries", flag.ExitOnError)
	apply := fs.Bool("apply", false, "delete the surplus rows (default: report only)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: mnemo dedupe-entries [--apply]

Remove surplus copies of entries that share a (session_id, uuid) key,
together with the messages hanging off them, and fill the uuid_m that a
swallowed trigger left unset on a survivor.

Duplicates could be written while a row's packing had removed it from the
partial unique index on the generated uuid column; the insert path no
longer permits it, and this clears what was already written.

Reports and changes nothing unless --apply is given. Idempotent. Space
returns to the filesystem only after a VACUUM.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve home: %v\n", err)
		os.Exit(1)
	}
	s, err := store.New(
		filepath.Join(home, ".mnemo", "mnemo.db"),
		filepath.Join(home, ".claude", "projects"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		os.Exit(1)
	}
	defer s.Close()

	res, err := s.DedupeEntries(context.Background(), !*apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dedupe-entries: %v\n", err)
		os.Exit(1)
	}

	switch {
	case res.DuplicateGroups == 0:
		fmt.Println("No duplicate entries: every (session_id, uuid) key holds one row.")
	case !*apply:
		fmt.Printf("Would remove %d surplus entries in %d duplicate groups, "+
			"with %d messages hanging off them.\nNothing was changed; re-run with --apply.\n",
			res.EntriesRemoved, res.DuplicateGroups, res.MessagesRemoved)
	default:
		fmt.Printf("Removed %d surplus entries in %d duplicate groups, and %d messages.\n"+
			"Repaired %d keys the collision had left unset.\n"+
			"Space returns to the filesystem only after a VACUUM.\n",
			res.EntriesRemoved, res.DuplicateGroups, res.MessagesRemoved, res.KeysRepaired)
	}
}
