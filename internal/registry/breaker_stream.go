// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"time"

	"github.com/marcelocantos/mnemo/internal/breaker"
	"github.com/marcelocantos/mnemo/internal/store"
)

// wrapBreakerRecord records success/failure on the circuit breaker after
// each Reconcile without changing stream isolation (🎯T145).
func wrapBreakerRecord(sr store.StreamReconciler, b *breaker.Breaker) store.StreamReconciler {
	return breakerStream{inner: sr, b: b}
}

type breakerStream struct {
	inner store.StreamReconciler
	b     *breaker.Breaker
}

func (w breakerStream) Name() string                 { return w.inner.Name() }
func (w breakerStream) Interval() time.Duration      { return w.inner.Interval() }
func (w breakerStream) PassTimeout() time.Duration   { return store.StreamPassTimeout(w.inner) }
func (w breakerStream) Reconcile(ctx context.Context, now time.Time) (int, error) {
	n, err := w.inner.Reconcile(ctx, now)
	if w.b != nil {
		if err != nil {
			w.b.Record(time.Now(), false, err.Error())
		} else {
			w.b.Record(time.Now(), true, "")
		}
	}
	return n, err
}
