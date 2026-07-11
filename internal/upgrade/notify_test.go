// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"sync/atomic"
	"testing"
)

func TestSideEffectsOnVersionChangeCallsListChanged(t *testing.T) {
	t.Parallel()
	tr := NewNoticeTracker()
	var calls atomic.Int32
	var gotFrom, gotTo string
	fx := &SideEffects{
		Notices: tr,
		ListChanged: func(from, to string) {
			calls.Add(1)
			gotFrom, gotTo = from, to
		},
	}
	fx.OnVersionChange([]string{"s1", "s2"}, "0.61.0", "0.62.0")
	if calls.Load() != 1 {
		t.Fatalf("list_changed calls %d", calls.Load())
	}
	if gotFrom != "0.61.0" || gotTo != "0.62.0" {
		t.Fatalf("from=%q to=%q", gotFrom, gotTo)
	}
	if msg, ok := tr.Consume("s1"); !ok || msg == "" {
		t.Fatal("expected notice for s1")
	}
}

func TestSideEffectsNilListChangedIsNoOp(t *testing.T) {
	t.Parallel()
	fx := &SideEffects{Notices: NewNoticeTracker()}
	fx.OnVersionChange([]string{"s"}, "1", "2") // must not panic
	if _, ok := fx.Notices.Consume("s"); !ok {
		t.Fatal("notice still applied")
	}
}

func TestListChangedHolderSetAndSend(t *testing.T) {
	t.Parallel()
	var h ListChangedHolder
	h.Send("a", "b") // no sender yet
	var n atomic.Int32
	h.Set(func(from, to string) {
		if from != "0.1.0" || to != "0.2.0" {
			t.Errorf("from=%q to=%q", from, to)
		}
		n.Add(1)
	})
	h.Send("0.1.0", "0.2.0")
	if n.Load() != 1 {
		t.Fatalf("n=%d", n.Load())
	}
	h.Set(nil)
	h.Send("x", "y") // cleared
	if n.Load() != 1 {
		t.Fatalf("after clear n=%d", n.Load())
	}
}

// fakeBroadcaster records SendNotificationToAllClients calls so we
// exercise BroadcastListChanged on the real method constant.
type fakeBroadcaster struct {
	method string
	params map[string]any
	calls  int
}

func (f *fakeBroadcaster) SendNotificationToAllClients(method string, params map[string]any) {
	f.calls++
	f.method = method
	f.params = params
}

func TestBroadcastListChangedUsesMCPMethod(t *testing.T) {
	t.Parallel()
	BroadcastListChanged(nil, "a", "b") // no-op
	var fb fakeBroadcaster
	BroadcastListChanged(&fb, "0.61.0", "0.62.0")
	if fb.calls != 1 {
		t.Fatalf("calls %d", fb.calls)
	}
	if fb.method != ToolsListChangedMethod {
		t.Fatalf("method %q want %q", fb.method, ToolsListChangedMethod)
	}
	if ToolsListChangedMethod != "notifications/tools/list_changed" {
		t.Fatalf("constant drift: %q", ToolsListChangedMethod)
	}
	if fb.params["reason"] != "mnemo_upgrade" {
		t.Fatalf("params %+v", fb.params)
	}
	if fb.params["from"] != "0.61.0" || fb.params["to"] != "0.62.0" {
		t.Fatalf("params %+v", fb.params)
	}
}

func TestSideEffectsWiresBroadcastListChanged(t *testing.T) {
	t.Parallel()
	var fb fakeBroadcaster
	fx := &SideEffects{
		Notices: NewNoticeTracker(),
		ListChanged: func(from, to string) {
			BroadcastListChanged(&fb, from, to)
		},
	}
	fx.OnVersionChange([]string{"s"}, "1.0.0", "1.1.0")
	if fb.method != ToolsListChangedMethod {
		t.Fatalf("method %q", fb.method)
	}
}
