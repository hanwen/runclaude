// Package sessionhub is the single source of truth for one shared claude
// session. It owns the agentclient, keeps the canonical ordered transcript so
// late joiners get full history, and fans every event out to all subscribers
// (the web viewers). Prompt submission flows back through the hub so it can be
// gated and attributed in later phases.
package sessionhub

import (
	"sync"

	"github.com/hanwen/runclaude/agentclient"
)

// subBuffer is the per-subscriber queue depth. A viewer that can't keep up past
// this is dropped (its channel closed); the browser reconnects and replays the
// full transcript, so no events are lost — the slow path just costs a reload
// rather than stalling the whole hub.
const subBuffer = 512

// Hub owns the session's agentclient and broadcasts its events.
type Hub struct {
	client *agentclient.Client

	mu         sync.Mutex
	transcript [][]byte // canonical ordered raw event lines
	subs       map[int]chan []byte
	nextID     int
	done       bool
}

// New returns a Hub over client. Call Run (once) to start consuming events.
func New(client *agentclient.Client) *Hub {
	return &Hub{client: client, subs: map[int]chan []byte{}}
}

// Run consumes the client's event stream until it closes (claude exited),
// appending each event to the transcript and fanning it out. It blocks; run it
// in its own goroutine. When the stream ends it closes all subscriber channels.
func (h *Hub) Run() {
	for ev := range h.client.Events() {
		h.broadcast(ev.Raw)
	}
	h.mu.Lock()
	h.done = true
	for id, ch := range h.subs {
		close(ch)
		delete(h.subs, id)
	}
	h.mu.Unlock()
}

func (h *Hub) broadcast(raw []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.transcript = append(h.transcript, raw)
	for id, ch := range h.subs {
		select {
		case ch <- raw:
		default:
			// Subscriber is too far behind; drop it. It reconnects and replays.
			close(ch)
			delete(h.subs, id)
		}
	}
}

// Subscription is a live view onto the session: History is a snapshot of the
// transcript at subscribe time, C streams events after it. Always Close it.
type Subscription struct {
	History [][]byte
	C       <-chan []byte

	hub *Hub
	id  int
	ch  chan []byte
}

// Subscribe registers a new viewer and returns the transcript so far plus a
// channel of subsequent events. If the session has already ended, C is an
// already-closed channel and History holds the complete transcript.
func (h *Hub) Subscribe() *Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	hist := make([][]byte, len(h.transcript))
	copy(hist, h.transcript)
	ch := make(chan []byte, subBuffer)
	id := h.nextID
	h.nextID++
	if h.done {
		close(ch)
		return &Subscription{History: hist, C: ch, hub: h, id: id, ch: ch}
	}
	h.subs[id] = ch
	return &Subscription{History: hist, C: ch, hub: h, id: id, ch: ch}
}

// Close removes the subscription. Safe to call more than once and safe to race
// with a hub-side drop/shutdown (both serialize on the hub mutex).
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if ch, ok := s.hub.subs[s.id]; ok && ch == s.ch {
		close(ch)
		delete(s.hub.subs, s.id)
	}
}

// SendPrompt forwards a prompt turn to claude. Phase 3 adds the controller gate
// and identity attribution on top of this.
func (h *Hub) SendPrompt(text string) error {
	return h.client.SendPrompt(text)
}
