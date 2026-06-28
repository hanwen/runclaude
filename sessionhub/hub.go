// Package sessionhub is the single source of truth for one shared claude
// session. It owns the agentclient, keeps the canonical ordered transcript so
// late joiners get full history, and fans every event out to all subscribers
// (the web viewers). Prompt submission flows back through the hub so it can be
// gated and attributed in later phases.
package sessionhub

import (
	"encoding/json"
	"errors"
	"log"
	"sync"

	"github.com/hanwen/runclaude/agentclient"
)

// ErrNotController is returned by Submit when a non-controller tries to send a
// prompt — the single-writer token is held by someone else (or nobody).
var ErrNotController = errors.New("not the controller")

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

	// Single-writer control token. ctrlID is the participant id currently
	// allowed to submit prompts ("" = nobody); ctrlName is its display name for
	// the UI. Take-control steals the token atomically (no pending/grant state).
	ctrlMu   sync.Mutex
	ctrlID   string
	ctrlName string
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

// SendPrompt forwards a prompt turn to claude with no control gate. It is the
// local terminal operator's path — the operator physically owns the session, so
// it is always allowed. Web prompts go through Submit instead.
func (h *Hub) SendPrompt(text string) error {
	return h.client.SendPrompt(text)
}

// Controller returns the participant id currently holding the writer token
// ("" if nobody).
func (h *Hub) Controller() string {
	h.ctrlMu.Lock()
	defer h.ctrlMu.Unlock()
	return h.ctrlID
}

// TakeControl atomically transfers the single writer token to participant id
// (display name name, authenticated login for the audit log). Any previous
// controller is displaced — clients compare the broadcast holder to their own
// id and the loser drops to viewer. Eligibility (may this login write at all)
// is enforced by the caller before calling this. Returns the displaced
// controller's name, if any.
func (h *Hub) TakeControl(id, name, login string) string {
	h.ctrlMu.Lock()
	prev := h.ctrlName
	h.ctrlID = id
	h.ctrlName = name
	h.ctrlMu.Unlock()
	log.Printf("audit: control taken by id=%q name=%q login=%q (displaced %q)", id, name, login, prev)
	h.emit(map[string]any{"type": "control", "controller": id, "controllerName": name})
	return prev
}

// ReleaseControl drops the token if id currently holds it (a controller
// voluntarily stepping down). No-op otherwise.
func (h *Hub) ReleaseControl(id string) {
	h.ctrlMu.Lock()
	if h.ctrlID != id {
		h.ctrlMu.Unlock()
		return
	}
	h.ctrlID = ""
	h.ctrlName = ""
	h.ctrlMu.Unlock()
	log.Printf("audit: control released by id=%q", id)
	h.emit(map[string]any{"type": "control", "controller": "", "controllerName": ""})
}

// Submit sends a prompt turn on behalf of participant id, enforcing the
// single-writer token: only the current controller may submit. login/name are
// recorded for the audit trail. Returns ErrNotController if id does not hold
// the token.
func (h *Hub) Submit(id, name, login, text string) error {
	if id == "" || id != h.Controller() {
		return ErrNotController
	}
	log.Printf("audit: prompt from id=%q name=%q login=%q: %q", id, name, login, truncate(text, 200))
	return h.client.SendPrompt(text)
}

// Interrupt stops the in-flight turn. Only the current controller may interrupt
// (it is a control action over the same single-writer token as prompting).
func (h *Hub) Interrupt(id string) error {
	if id == "" || id != h.Controller() {
		return ErrNotController
	}
	log.Printf("audit: interrupt by id=%q", id)
	return h.client.Interrupt()
}

// emit broadcasts a hub-synthesized event (e.g. a control change) into the same
// transcript/fan-out stream as claude's events, so it is ordered, replayed to
// late joiners, and part of the audit-visible record.
func (h *Hub) emit(ev map[string]any) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	h.broadcast(raw)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
