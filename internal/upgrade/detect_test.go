// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDetectorDisabledNeverCallsFetch(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	d := NewDetector(&DetectorArgs{
		CurrentVersion: "0.61.0",
		Disabled:       true,
		Fetch: func(ctx context.Context) (string, error) {
			calls.Add(1)
			return "v0.99.0", nil
		},
	})
	cr := d.Check(context.Background())
	if cr.CalledNetwork {
		t.Fatal("expected no network when disabled")
	}
	if cr.UpgradeAvailable {
		t.Fatal("expected no upgrade flag when disabled")
	}
	if calls.Load() != 0 || d.FetchCount() != 0 {
		t.Fatalf("fetch invoked %d times", calls.Load())
	}
}

func TestDetectorReportsUpgradeWhenNewer(t *testing.T) {
	t.Parallel()
	d := NewDetector(&DetectorArgs{
		CurrentVersion: "0.61.0",
		Fetch: func(ctx context.Context) (string, error) {
			return "v0.62.0", nil
		},
		MinInterval: time.Hour,
	})
	cr := d.Check(context.Background())
	if !cr.CalledNetwork {
		t.Fatal("expected network call")
	}
	if !cr.UpgradeAvailable {
		t.Fatalf("expected upgrade: %+v", cr)
	}
	if cr.Latest != "v0.62.0" {
		t.Fatalf("latest %q", cr.Latest)
	}
	// Second check within interval uses cache — no second fetch.
	cr2 := d.Check(context.Background())
	if cr2.CalledNetwork {
		t.Fatal("expected cache hit")
	}
	if !cr2.UpgradeAvailable {
		t.Fatal("still available")
	}
	if d.FetchCount() != 1 {
		t.Fatalf("fetch count %d", d.FetchCount())
	}
}

func TestDetectorUpToDate(t *testing.T) {
	t.Parallel()
	d := NewDetector(&DetectorArgs{
		CurrentVersion: "0.62.0",
		Fetch: func(ctx context.Context) (string, error) {
			return "v0.62.0", nil
		},
	})
	cr := d.Check(context.Background())
	if cr.UpgradeAvailable {
		t.Fatal("should be up to date")
	}
}

func TestDetectorFetchError(t *testing.T) {
	t.Parallel()
	d := NewDetector(&DetectorArgs{
		CurrentVersion: "0.61.0",
		Fetch: func(ctx context.Context) (string, error) {
			return "", errors.New("gh missing")
		},
	})
	cr := d.Check(context.Background())
	if cr.Err == nil {
		t.Fatal("expected error")
	}
	if !cr.CalledNetwork {
		t.Fatal("expected attempted fetch")
	}
}
