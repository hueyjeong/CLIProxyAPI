package cjkevi

import (
	"sync"
	"time"
)

const HistoryCapacity = 256

// HistoryEvent contains only bounded, enumerated decision metadata; never request text.
type HistoryEvent struct {
	Time             time.Time `json:"time"`
	Outcome          string    `json:"outcome"`
	Reason           string    `json:"reason"`
	Surface          string    `json:"surface"`
	JevCalled        bool      `json:"jev_called"`
	LLMCalled        bool      `json:"llm_called"`
	IntentionalScore *float64  `json:"intentional_score,omitempty"`
	IntrusionScore   *float64  `json:"intrusion_score,omitempty"`
}

// History is a fixed-size, concurrency-safe ring of terminal correction decisions.
type History struct {
	mu     sync.Mutex
	events [HistoryCapacity]HistoryEvent
	next   int
	count  int
}

func (h *History) Add(event HistoryEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events[h.next] = event
	h.next = (h.next + 1) % HistoryCapacity
	if h.count < HistoryCapacity {
		h.count++
	}
}

func (h *History) Snapshot() []HistoryEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]HistoryEvent, h.count)
	for i := range out {
		out[i] = h.events[(h.next-1-i+HistoryCapacity)%HistoryCapacity]
	}
	return out
}
