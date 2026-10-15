// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"runtime"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// imageThreadCap is the stated OS-thread budget for the stress test:
// threads created during 20,000 appends across 500 sessions, where each
// scan pins its thread as a blocking cgo call (OCR) does. Unbounded
// spawning created one thread per in-flight scan.
const imageThreadCap = 64

func TestSessionImagesBoundedUnderAppendStorm(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	s.AwaitStartup()
	var running, peak, scans atomic.Int32
	scanned := sync.Map{}
	imageScanHook = func(sid string) {
		runtime.LockOSThread() // a blocking cgo call holds its thread
		defer runtime.UnlockOSThread()
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(200 * time.Microsecond)
		scanned.Store(sid, true)
		scans.Add(1)
		running.Add(-1)
	}
	defer func() { imageScanHook = nil }()

	threads := pprof.Lookup("threadcreate")
	threadsBefore := threads.Count()
	gBefore := runtime.NumGoroutine()

	const appenders, perAppender, sessions = 200, 100, 500
	var wg sync.WaitGroup
	var gPeak atomic.Int64
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if g := int64(runtime.NumGoroutine()); g > gPeak.Load() {
				gPeak.Store(g)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	start := make(chan struct{})
	for a := 0; a < appenders; a++ {
		wg.Add(1)
		go func(a int) {
			defer wg.Done()
			<-start
			for i := 0; i < perAppender; i++ {
				s.enqueueSessionImages(fmt.Sprintf("sess-storm-%03d", (a*perAppender+i)%sessions))
			}
		}(a)
	}
	close(start)
	wg.Wait()
	deadline := time.Now().Add(30 * time.Second)
	for s.imagesBusy() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	if s.imagesBusy() {
		t.Fatal("image queue did not drain")
	}
	threadsCreated := threads.Count() - threadsBefore
	t.Logf("appends=%d sessions=%d scans=%d peak concurrent scans=%d peak workers=%d",
		appenders*perAppender, sessions, scans.Load(), peak.Load(), s.imgQ.peak.Load())
	t.Logf("goroutines before=%d peak=%d (incl. %d appenders) after=%d; OS threads created=%d (cap %d)",
		gBefore, gPeak.Load(), appenders, runtime.NumGoroutine(), threadsCreated, imageThreadCap)

	if p := peak.Load(); p > int32(imageWorkers) {
		t.Errorf("peak concurrent scans = %d, want <= %d", p, imageWorkers)
	}
	if p := s.imgQ.peak.Load(); p > int32(imageWorkers) {
		t.Errorf("peak image workers = %d, want <= %d", p, imageWorkers)
	}
	if threadsCreated > imageThreadCap {
		t.Errorf("OS threads created = %d, want <= %d", threadsCreated, imageThreadCap)
	}
	if g := gPeak.Load() - int64(gBefore) - appenders; g > int64(imageWorkers)+8 {
		t.Errorf("goroutines beyond the appenders peaked at %d, want about %d", g, imageWorkers)
	}
	n := 0
	scanned.Range(func(_, _ any) bool { n++; return true })
	if n != sessions {
		t.Errorf("sessions scanned = %d, want all %d", n, sessions)
	}
	if sc := scans.Load(); sc >= appenders*perAppender/2 {
		t.Errorf("scans = %d for %d appends: per-session coalescing is not working", sc, appenders*perAppender)
	}
}

// An append that lands while its session is being scanned must cause
// one more scan, so images in that append are not missed.
func TestSessionImagesRerunAfterAppendDuringScan(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	s.AwaitStartup()
	release := make(chan struct{})
	var scans atomic.Int32
	imageScanHook = func(string) {
		if scans.Add(1) == 1 {
			<-release
		}
	}
	defer func() { imageScanHook = nil }()
	s.enqueueSessionImages("sess-rerun-a")
	for scans.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 50; i++ {
		s.enqueueSessionImages("sess-rerun-a")
	}
	close(release)
	for s.imagesBusy() {
		time.Sleep(time.Millisecond)
	}
	if n := scans.Load(); n != 2 {
		t.Fatalf("scans = %d, want 2 (the first plus one coalesced rerun)", n)
	}
}
