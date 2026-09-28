package gateway

import "sync"

// sseFrame is one SSE frame: ID is the persisted entry seq (used as the
// SSE `id:` field, so clients reconnect with Last-Event-ID), Data is the
// AgentEvent JSON.
type sseFrame struct {
	ID   string
	Data string
}

// hub fans one session's events out to all live SSE subscribers.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan sseFrame]struct{}
}

func newHub() *hub {
	return &hub{subs: make(map[string]map[chan sseFrame]struct{})}
}

// subscribe registers a live subscriber for one session.
func (h *hub) subscribe(sessionID string) (<-chan sseFrame, func()) {
	ch := make(chan sseFrame, 256)
	h.mu.Lock()
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = make(map[chan sseFrame]struct{})
	}
	h.subs[sessionID][ch] = struct{}{}
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if m := h.subs[sessionID]; m != nil {
			delete(m, ch)
			if len(m) == 0 {
				delete(h.subs, sessionID)
			}
		}
	}
	return ch, cancel
}

// broadcast pushes a frame to every subscriber of a session. A slow
// subscriber's buffer overflows and frames are dropped for it — it
// catches up exactly on reconnect via Last-Event-ID replay (DESIGN §6.5),
// so dropping is safe.
func (h *hub) broadcast(sessionID string, f sseFrame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[sessionID] {
		select {
		case ch <- f:
		default:
		}
	}
}
