// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultQueryTimeout is the statement-execution budget for agent-reachable
// ad-hoc reads (Store.Query / mnemo_query). Distinct from PRAGMA busy_timeout,
// which only bounds lock-acquisition waits (🎯T74).
const DefaultQueryTimeout = 30 * time.Second

// ErrQueryTimeout is returned (wrapped) when an ad-hoc query exceeds its
// execution budget. Agents should narrow the query rather than retry verbatim.
var ErrQueryTimeout = errors.New("query exceeded the execution budget")

// SetQueryTimeoutForTest overrides DefaultQueryTimeout for this store.
// Pass 0 to restore the default. Not for production configuration.
func (s *Store) SetQueryTimeoutForTest(d time.Duration) {
	s.queryTimeoutOverride = d
}

func (s *Store) effectiveQueryTimeout() time.Duration {
	if s != nil && s.queryTimeoutOverride > 0 {
		return s.queryTimeoutOverride
	}
	return DefaultQueryTimeout
}

// mapQueryTimeout rewrites context deadline / SQLite interrupt errors into
// a clear ErrQueryTimeout with the configured budget in the message.
func mapQueryTimeout(err error, ctx context.Context, budget time.Duration) error {
	if err == nil {
		return nil
	}
	if isQueryTimeoutErr(err, ctx) {
		secs := int(budget / time.Second)
		if secs < 1 {
			secs = 1
		}
		return fmt.Errorf("%w: query exceeded the %d-second budget — narrow the query or add a filter",
			ErrQueryTimeout, secs)
	}
	return err
}

func isQueryTimeoutErr(err error, ctx context.Context) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	if ctx != nil && (errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled)) {
		// Driver may wrap interrupt without preserving context.DeadlineExceeded.
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "interrupted") || strings.Contains(msg, "deadline exceeded") {
		return true
	}
	return false
}

// IsQueryTimeout reports whether err is (or wraps) ErrQueryTimeout.
func IsQueryTimeout(err error) bool {
	return errors.Is(err, ErrQueryTimeout)
}
