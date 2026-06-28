package sessionhub

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/hanwen/runclaude/agentclient"
)

// newTestHub wires a Hub to an agentclient reading from a pipe we feed by hand,
// standing in for claude's stdout. The returned write func emits one event
// line; closing the pipe ends the stream.
func newTestHub(t *testing.T) (*Hub, func(string), *io.PipeWriter) {
	t.Helper()
	pr, pw := io.Pipe()
	client := agentclient.New(&bytes.Buffer{}, pr)
	h := New(client)
	go h.Run()
	emit := func(line string) {
		if _, err := io.WriteString(pw, line+"\n"); err != nil {
			t.Errorf("emit: %v", err)
		}
	}
	return h, emit, pw
}

func recv(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case b, ok := <-ch:
		if !ok {
			t.Fatal("channel closed, wanted an event")
		}
		return b
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		return nil
	}
}

func TestHubFanoutAndHistory(t *testing.T) {
	h, emit, pw := newTestHub(t)

	// Early subscriber: empty history, sees live events.
	early := h.Subscribe()
	defer early.Close()
	if len(early.History) != 0 {
		t.Fatalf("early history = %d, want 0", len(early.History))
	}

	emit(`{"type":"system","subtype":"init","session_id":"s1"}`)
	if got := recv(t, early.C); !bytes.Contains(got, []byte(`"s1"`)) {
		t.Errorf("early event = %s", got)
	}
	emit(`{"type":"assistant","session_id":"s1"}`)
	recv(t, early.C)

	// Late subscriber: gets both prior events as history, then live ones.
	late := h.Subscribe()
	defer late.Close()
	if len(late.History) != 2 {
		t.Fatalf("late history = %d, want 2", len(late.History))
	}
	emit(`{"type":"result","subtype":"success","session_id":"s1"}`)
	if got := recv(t, late.C); !bytes.Contains(got, []byte(`"result"`)) {
		t.Errorf("late live event = %s", got)
	}
	// The early subscriber sees the same live event (independent fan-out).
	if got := recv(t, early.C); !bytes.Contains(got, []byte(`"result"`)) {
		t.Errorf("early live event = %s", got)
	}

	// Ending the stream closes subscriber channels.
	pw.Close()
	for {
		if _, ok := <-early.C; !ok {
			break
		}
	}
}

func TestControlStealAndSubmit(t *testing.T) {
	pr, pw := io.Pipe()
	client := agentclient.New(&bytes.Buffer{}, pr)
	h := New(client)
	go h.Run()
	_ = pw // keep the stream open so Run blocks rather than ending

	sub := h.Subscribe()
	defer sub.Close()

	// Nobody controls yet: a non-controller submit is rejected.
	if err := h.Submit("alice", "alice", "alice", "hi"); err != ErrNotController {
		t.Fatalf("submit with no controller = %v, want ErrNotController", err)
	}

	// Alice takes control; a control event is broadcast.
	h.TakeControl("alice", "Alice", "alice@x")
	if got := recv(t, sub.C); !bytes.Contains(got, []byte(`"controller":"alice"`)) {
		t.Errorf("control event = %s", got)
	}
	if h.Controller() != "alice" {
		t.Errorf("controller = %q, want alice", h.Controller())
	}
	if err := h.Submit("alice", "Alice", "alice@x", "do thing"); err != nil {
		t.Errorf("alice submit = %v, want nil", err)
	}

	// Bob steals; the prior controller (alice) is displaced.
	prev := h.TakeControl("bob", "Bob", "bob@x")
	if prev != "Alice" {
		t.Errorf("displaced = %q, want Alice", prev)
	}
	recv(t, sub.C) // the bob control event
	if err := h.Submit("alice", "Alice", "alice@x", "still me?"); err != ErrNotController {
		t.Errorf("displaced alice submit = %v, want ErrNotController", err)
	}

	// Interrupt is gated on the same token: alice (displaced) cannot, bob can.
	if err := h.Interrupt("alice"); err != ErrNotController {
		t.Errorf("displaced alice interrupt = %v, want ErrNotController", err)
	}
	if err := h.Interrupt("bob"); err != nil {
		t.Errorf("controller bob interrupt = %v, want nil", err)
	}

	// Release by a non-holder is a no-op; by the holder clears the token.
	h.ReleaseControl("alice")
	if h.Controller() != "bob" {
		t.Errorf("controller after non-holder release = %q, want bob", h.Controller())
	}
	h.ReleaseControl("bob")
	if h.Controller() != "" {
		t.Errorf("controller after release = %q, want empty", h.Controller())
	}
}

func TestSubscribeAfterEnd(t *testing.T) {
	h, emit, pw := newTestHub(t)
	emit(`{"type":"system","subtype":"init","session_id":"s2"}`)
	// Give Run time to ingest, then end the stream.
	time.Sleep(50 * time.Millisecond)
	pw.Close()
	time.Sleep(50 * time.Millisecond)

	sub := h.Subscribe()
	defer sub.Close()
	if len(sub.History) != 1 {
		t.Errorf("history after end = %d, want 1", len(sub.History))
	}
	if _, ok := <-sub.C; ok {
		t.Error("channel should be closed for a session that already ended")
	}
}
