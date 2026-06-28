package agentclient

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Client speaks the stream-json protocol over an already-connected pair of
// streams: w is claude's stdin (we write user turns), r is claude's stdout (we
// read events). It is deliberately transport-agnostic — Phase 0 spawns claude
// directly (see Spawn), Phase 1 hands it the ends of pipes that cross the
// sandbox re-execs — so the same reader/writer logic serves both.
type Client struct {
	mu     sync.Mutex // serializes writes to w and guards reqSeq
	w      io.Writer
	events chan Event
	reqSeq int // monotonic id for control requests
}

// New starts reading events from r in a background goroutine and returns a
// Client that writes turns to w. The Events channel is closed when r reaches
// EOF (claude exited / stdout closed). If w is an io.Closer, CloseStdin closes
// it to signal end-of-input (claude exits when its stdin closes).
func New(w io.Writer, r io.Reader) *Client {
	c := &Client{w: w, events: make(chan Event, 64)}
	go c.readLoop(r)
	return c
}

// CloseStdin closes claude's stdin if the write side supports it, signalling
// no more turns; claude then finishes any in-flight turn and exits. No-op if w
// is not a closer.
func (c *Client) CloseStdin() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wc, ok := c.w.(io.Closer); ok {
		return wc.Close()
	}
	return nil
}

// Events yields decoded events until claude's stdout closes, at which point the
// channel is closed.
func (c *Client) Events() <-chan Event { return c.events }

func (c *Client) readLoop(r io.Reader) {
	defer close(c.events)
	sc := bufio.NewScanner(r)
	// Tool results and large assistant turns can produce very long lines;
	// raise the scanner cap well above the 64 KiB default.
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		// Scanner reuses its buffer; copy before handing the line on.
		raw := make([]byte, len(line))
		copy(raw, line)
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			// Non-JSON on stdout shouldn't happen in stream-json mode; skip it
			// rather than killing the stream.
			continue
		}
		c.events <- Event{
			Type:      env.Type,
			Subtype:   env.Subtype,
			SessionID: env.SessionID,
			Raw:       raw,
		}
	}
}

// SendPrompt writes one user turn to claude's stdin as a stream-json line.
// Safe for concurrent callers; writes are serialized.
func (c *Client) SendPrompt(text string) error {
	var m userMessage
	m.Type = "user"
	m.Message.Role = "user"
	m.Message.Content = text
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(b)
	return err
}

// controlRequest is a bidirectional control-channel message written to claude's
// stdin. Only the "interrupt" subtype is used (to stop a running turn); the rest
// of the control subprotocol is intentionally out of scope.
type controlRequest struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype string `json:"subtype"`
	} `json:"request"`
}

// Interrupt asks claude to stop the in-flight turn. claude acknowledges with a
// control_response event on the stream and ends the turn with a result whose
// subtype reports the interruption. Fire-and-forget: we don't block on the ack.
func (c *Client) Interrupt() error {
	c.mu.Lock()
	c.reqSeq++
	var req controlRequest
	req.Type = "control_request"
	req.RequestID = fmt.Sprintf("req_int_%d", c.reqSeq)
	req.Request.Subtype = "interrupt"
	b, err := json.Marshal(req)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	b = append(b, '\n')
	_, err = c.w.Write(b)
	c.mu.Unlock()
	return err
}
