package cjkevi

import (
	"sync"
	"testing"
)

func TestHistoryConcurrentBounded(t *testing.T) {
	h := &History{}
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			for j := 0; j < HistoryCapacity; j++ {
				h.Add(HistoryEvent{Reason: "no_candidate", Surface: SurfaceChat})
				if got := h.Snapshot(); len(got) > HistoryCapacity {
					t.Errorf("capacity exceeded: %d", len(got))
				}
			}
		}(i)
	}
	workers.Wait()
	if got := h.Snapshot(); len(got) != HistoryCapacity {
		t.Fatalf("want %d events, got %d", HistoryCapacity, len(got))
	}
	h.Add(HistoryEvent{Reason: "newest"})
	if got := h.Snapshot(); got[0].Reason != "newest" || len(got) != HistoryCapacity {
		t.Fatalf("ring ordering/capacity: %+v", got[0])
	}
}
