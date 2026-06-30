package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"github.com/hanwen/runclaude/terminal"
)

// Terminal is the host-side per-user shell broker the serve layer drives. It is
// satisfied by *terminal.Broker. Shells are keyed by authenticated login and
// persist across reconnects. Attach serves both the read-write owner and
// read-only Presenter watchers — write-eligibility is enforced here in the
// handlers, not in Attach.
type Terminal interface {
	Attach(login string) (terminal.Attachment, error)
	Write(login string, p []byte) error
	Resize(login string, cols, rows uint16) error
}

// wsWriter serializes writes to a WebSocket: the shell-output forwarder and the
// control path (scrollback, clears, notices) write concurrently, and a Conn
// permits only one writer at a time.
type wsWriter struct {
	c  *websocket.Conn
	mu sync.Mutex
}

func (w *wsWriter) bin(ctx context.Context, p []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c.Write(ctx, websocket.MessageBinary, p)
}

func (w *wsWriter) text(ctx context.Context, p []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c.Write(ctx, websocket.MessageText, p)
}

// size tells the client the shell's current grid, so a read-only Presenter can
// mirror it exactly instead of fitting to its own window.
func (w *wsWriter) size(ctx context.Context, cols, rows uint16) error {
	return w.text(ctx, []byte(fmt.Sprintf(`{"type":"size","cols":%d,"rows":%d}`, cols, rows)))
}

// resizeMsg is the only client→server text message: a PTY window-size update.
type resizeMsg struct {
	Resize *struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	} `json:"resize"`
}

// handleTerminal is the read-write "My shell" endpoint: a write-eligible user's
// own persistent shell, keyed by login. Gated by Policy.MayWrite, exactly like
// /prompt.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	if s.term == nil {
		http.Error(w, "no terminal", http.StatusNotFound)
		return
	}
	id := s.ident.Identify(r)
	if !s.policy.MayWrite(id) {
		http.Error(w, "not write-eligible", http.StatusForbidden)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	s.runTerminal(r.Context(), c, id.Login, true)
}

// handleSharedTerminal is the read-only Presenter endpoint, open to any viewer
// (no privacy expectation). It streams whichever login currently holds session
// control, re-pointing when control changes.
func (s *Server) handleSharedTerminal(w http.ResponseWriter, r *http.Request) {
	if s.term == nil {
		http.Error(w, "no terminal", http.StatusNotFound)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	s.runPresenter(r.Context(), c)
}

// runTerminal bridges one WebSocket to login's shell. Output is forwarded
// shell→ws; when rw, binary ws messages are stdin and text messages are resize
// controls. Detaching leaves the shell alive (persistence); the shell exiting
// closes out, which ends the bridge.
func (s *Server) runTerminal(ctx context.Context, c *websocket.Conn, login string, rw bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	att, err := s.term.Attach(login)
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, "terminal unavailable")
		return
	}
	defer att.Close()

	ww := &wsWriter{c: c}
	if len(att.Scrollback) > 0 {
		_ = ww.bin(ctx, att.Scrollback)
	}

	// Tell the owner when their shell is the one on the shared Presenter view —
	// i.e. they hold control, so the meeting is watching this shell — so the UI
	// can show a "presenting" badge. Tracks control changes via the hub.
	go func() {
		sub := s.hub.Subscribe()
		defer sub.Close()
		last, sent := false, false
		notify := func() {
			now := s.hub.ControllerLogin() == login
			if now != last || !sent {
				last, sent = now, true
				_ = ww.text(ctx, []byte(fmt.Sprintf(`{"type":"presenting","on":%t}`, now)))
			}
		}
		notify()
		for {
			select {
			case <-ctx.Done():
				return
			case raw, ok := <-sub.C:
				if !ok {
					return
				}
				if isControlEvent(raw) {
					notify()
				}
			}
		}
	}()

	// The owner drives its own size (its xterm fits to its window and sends
	// resize), so size events are ignored here; only output bytes are forwarded.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-att.Out:
				if !ok {
					_ = ww.text(ctx, []byte(`{"type":"exit"}`))
					cancel()
					return
				}
				if ev.Data == nil {
					continue
				}
				if ww.bin(ctx, ev.Data) != nil {
					cancel()
					return
				}
			}
		}
	}()

	c.SetReadLimit(1 << 20)
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if !rw {
			continue
		}
		switch typ {
		case websocket.MessageBinary:
			_ = s.term.Write(login, data)
		case websocket.MessageText:
			var m resizeMsg
			if json.Unmarshal(data, &m) == nil && m.Resize != nil {
				_ = s.term.Resize(login, m.Resize.Cols, m.Resize.Rows)
			}
		}
	}
}

// runPresenter streams the current controller's shell read-only, following the
// control token. It subscribes to the hub to learn of control changes and
// re-attaches to the new controller's shell on each one. Client input is ignored
// (read only to detect disconnect).
func (s *Server) runPresenter(ctx context.Context, c *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ww := &wsWriter{c: c}

	// Drain reads only to notice the client going away.
	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()

	sub := s.hub.Subscribe()
	defer sub.Close()

	var fwdCancel context.CancelFunc
	var attachClose func()
	curLogin := "\x00" // sentinel distinct from "" (no controller)

	switchTo := func(login string) {
		if login == curLogin {
			return
		}
		curLogin = login
		if fwdCancel != nil {
			fwdCancel()
			fwdCancel = nil
		}
		if attachClose != nil {
			attachClose()
			attachClose = nil
		}
		if login == "" {
			// \x1bc resets the emulator; then a hint.
			_ = ww.bin(ctx, []byte("\x1bc[no presenter — take control and open a terminal]\r\n"))
			return
		}
		att, err := s.term.Attach(login)
		if err != nil {
			_ = ww.bin(ctx, []byte("\x1bc[presenter terminal unavailable]\r\n"))
			return
		}
		attachClose = att.Close
		_ = ww.bin(ctx, append([]byte("\x1bc"), att.Scrollback...))
		_ = ww.size(ctx, att.Cols, att.Rows)

		out := att.Out
		var fctx context.Context
		fctx, fwdCancel = context.WithCancel(ctx)
		go func() {
			for {
				select {
				case <-fctx.Done():
					return
				case ev, ok := <-out:
					if !ok {
						return
					}
					var werr error
					if ev.Data != nil {
						werr = ww.bin(ctx, ev.Data)
					} else {
						werr = ww.size(ctx, ev.Cols, ev.Rows)
					}
					if werr != nil {
						cancel()
						return
					}
				}
			}
		}()
	}

	switchTo(s.hub.ControllerLogin())
	for {
		select {
		case <-ctx.Done():
			if fwdCancel != nil {
				fwdCancel()
			}
			if attachClose != nil {
				attachClose()
			}
			return
		case raw, ok := <-sub.C:
			if !ok {
				return
			}
			if isControlEvent(raw) {
				switchTo(s.hub.ControllerLogin())
			}
		}
	}
}

func isControlEvent(raw []byte) bool {
	var h struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &h) == nil && h.Type == "control"
}
