package serve

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/hanwen/runclaude/agentclient"
	"github.com/hanwen/runclaude/sessionhub"
	"github.com/hanwen/runclaude/terminal"
)

// fakeTerminal is an in-memory Terminal: Attach hands out a per-login output
// channel (seeded with a recognizable scrollback marker), and tests push output
// and inspect recorded input/resizes.
type fakeTerminal struct {
	mu      sync.Mutex
	chans   map[string]chan terminal.Event
	writes  map[string][]byte
	resizes map[string][2]uint16
}

func newFakeTerminal() *fakeTerminal {
	return &fakeTerminal{chans: map[string]chan terminal.Event{}, writes: map[string][]byte{}, resizes: map[string][2]uint16{}}
}

func (f *fakeTerminal) Attach(login string) (terminal.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan terminal.Event, 32)
	f.chans[login] = ch
	return terminal.Attachment{
		Out:        ch,
		Scrollback: []byte("SCROLL:" + login + ";"),
		Cols:       80,
		Rows:       24,
		Close:      func() {},
	}, nil
}

func (f *fakeTerminal) push(login string, b []byte) {
	f.mu.Lock()
	ch := f.chans[login]
	f.mu.Unlock()
	if ch != nil {
		ch <- terminal.Event{Data: b}
	}
}

func (f *fakeTerminal) Write(login string, p []byte) error {
	f.mu.Lock()
	f.writes[login] = append(f.writes[login], p...)
	f.mu.Unlock()
	return nil
}

func (f *fakeTerminal) Resize(login string, cols, rows uint16) error {
	f.mu.Lock()
	f.resizes[login] = [2]uint16{cols, rows}
	f.mu.Unlock()
	return nil
}

func (f *fakeTerminal) recordedWrite(login string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.writes[login])
}

func wsURL(httpURL, path string) string {
	return strings.Replace(httpURL, "http", "ws", 1) + path
}

func newTermServer(t *testing.T, hub *sessionhub.Hub, ft *fakeTerminal) *httptest.Server {
	t.Helper()
	return httptest.NewServer(New(Config{
		Hub:      hub,
		Identity: DevIdentifier{Base: Identity{Login: "local", Name: "operator"}},
		Policy:   AllowlistPolicy{Logins: map[string]bool{"alice": true}},
		Term:     ft,
	}))
}

// readUntil accumulates WebSocket frame payloads until sub is seen.
func readUntil(t *testing.T, ctx context.Context, c *websocket.Conn, sub string) {
	t.Helper()
	var buf []byte
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read before seeing %q: %v; got: %q", sub, err, buf)
		}
		buf = append(buf, data...)
		if bytes.Contains(buf, []byte(sub)) {
			return
		}
	}
}

func TestTerminalGatesOnWriteEligibility(t *testing.T) {
	hub := sessionhub.New(agentclient.New(&bytes.Buffer{}, strings.NewReader("")))
	ft := newFakeTerminal()
	srv := newTermServer(t, hub, ft)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// mallory is not in the allowlist -> the /terminal upgrade is rejected 403.
	_, resp, err := websocket.Dial(ctx, wsURL(srv.URL, "/terminal?as=mallory"), nil)
	if err == nil {
		t.Fatal("expected dial to fail for ineligible user")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got resp %v, want 403", resp)
	}
}

func TestTerminalOwnerIO(t *testing.T) {
	hub := sessionhub.New(agentclient.New(&bytes.Buffer{}, strings.NewReader("")))
	ft := newFakeTerminal()
	srv := newTermServer(t, hub, ft)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL(srv.URL, "/terminal?as=alice"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// Scrollback arrives first, then live output.
	readUntil(t, ctx, c, "SCROLL:alice;")
	ft.push("alice", []byte("LIVE_OUT_1"))
	readUntil(t, ctx, c, "LIVE_OUT_1")

	// Input (binary) and resize (text) reach the broker.
	if err := c.Write(ctx, websocket.MessageBinary, []byte("ls -la\n")); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"resize":{"cols":120,"rows":40}}`)); err != nil {
		t.Fatal(err)
	}

	// Input and resize are separate WS messages processed in order; poll until
	// both have landed rather than racing the server's read loop.
	deadline := time.After(3 * time.Second)
	for {
		ft.mu.Lock()
		gotWrite := string(ft.writes["alice"])
		gotResize := ft.resizes["alice"]
		ft.mu.Unlock()
		if gotWrite == "ls -la\n" && gotResize == [2]uint16{120, 40} {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("input/resize not recorded; write=%q resize=%v", gotWrite, gotResize)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestMyShellPresentingBadge(t *testing.T) {
	hub := sessionhub.New(agentclient.New(&bytes.Buffer{}, strings.NewReader("")))
	ft := newFakeTerminal()
	srv := newTermServer(t, hub, ft)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL(srv.URL, "/terminal?as=alice"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// Not presenting until alice holds control.
	readUntil(t, ctx, c, `"type":"presenting","on":false`)
	hub.TakeControl("pid-alice", "Alice", "alice")
	readUntil(t, ctx, c, `"type":"presenting","on":true`)
}

func TestPresenterFollowsControl(t *testing.T) {
	// An open pipe keeps the agent event stream alive so hub.Run keeps running
	// (and the presenter's hub subscription stays open) for the test's duration.
	pr, pw := io.Pipe()
	defer pw.Close()
	hub := sessionhub.New(agentclient.New(&bytes.Buffer{}, pr))
	go hub.Run()
	ft := newFakeTerminal()
	srv := newTermServer(t, hub, ft)
	defer srv.Close()

	// bob holds control when the presenter connects.
	hub.TakeControl("pid-bob", "Bob", "bob")

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	// The Presenter view is open to any viewer (here an ineligible login).
	c, _, err := websocket.Dial(ctx, wsURL(srv.URL, "/terminal/shared?as=viewer"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	readUntil(t, ctx, c, "SCROLL:bob;")
	// The Presenter is told the source grid so it mirrors it (rather than fitting
	// to the viewer's own window).
	readUntil(t, ctx, c, `"type":"size"`)
	ft.push("bob", []byte("BOB_OUT"))
	readUntil(t, ctx, c, "BOB_OUT")

	// Control moves to carol -> the presenter re-points to carol's shell.
	time.Sleep(50 * time.Millisecond)
	hub.TakeControl("pid-carol", "Carol", "carol")
	readUntil(t, ctx, c, "SCROLL:carol;")
	ft.push("carol", []byte("CAROL_OUT"))
	readUntil(t, ctx, c, "CAROL_OUT")
}
