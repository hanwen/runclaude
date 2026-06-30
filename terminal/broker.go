package terminal

import (
	"errors"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/creack/pty"
)

// scrollbackBytes bounds the per-shell replay buffer: a viewer attaching (or
// reattaching after a reconnect) gets at most this many trailing bytes so the
// terminal looks "live" without unbounded host memory per shell.
const scrollbackBytes = 256 * 1024

// subBuffer is the per-subscriber queue depth; a watcher that falls this far
// behind is dropped and its WebSocket closes (the browser reconnects and
// replays scrollback). Mirrors sessionhub's slow-subscriber policy.
const subBuffer = 256

// openTimeout caps how long Attach waits for the sandbox Agent to return a
// freshly spawned shell's master fd.
const openTimeout = 10 * time.Second

// Event is one item in a shell's output stream: either terminal output bytes
// (Data set), or a window-size change (Data nil, Cols/Rows set). Watchers use
// size events to mirror the shell's true grid — a read-only viewer can't pick
// its own size without desyncing from the source PTY.
type Event struct {
	Data       []byte
	Cols, Rows uint16
}

// Attachment is a live view of one shell: a stream of events, the recent
// scrollback, the current size, and an unsubscribe func.
type Attachment struct {
	Out        <-chan Event
	Scrollback []byte
	Cols, Rows uint16
	Close      func()
}

// Broker owns the host end of the term socket and one shell session per login.
// Shells persist across WebSocket detach/reconnect; they end only when the shell
// process exits (the Agent sends "exited") or the Broker is closed.
type Broker struct {
	conn *conn

	mu       sync.Mutex
	sessions map[string]*shell
	closed   bool
}

// NewBroker wraps the host end of the term socket and starts the read loop.
func NewBroker(fd int) *Broker {
	b := &Broker{conn: newConn(fd), sessions: map[string]*shell{}}
	go b.readLoop()
	return b
}

// shell is one running PTY: the master fd, a bounded scrollback ring, and the
// set of live subscribers fanned the master's output.
type shell struct {
	login string

	ready   chan struct{} // closed once master is set (or openErr on failure)
	openErr error

	mu         sync.Mutex
	master     *os.File
	ring       []byte
	cols, rows uint16
	subs       map[int]chan Event
	nextSub    int
	done       bool
}

func newShell(login string) *shell {
	return &shell{login: login, ready: make(chan struct{}), subs: map[int]chan Event{}}
}

// readLoop consumes frames from the sandbox Agent until the socket closes.
func (b *Broker) readLoop() {
	for {
		f, fd, err := b.conn.recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("terminal: broker read: %v", err)
			}
			b.shutdown()
			return
		}
		switch f.Op {
		case opOpened:
			b.onOpened(f.Login, fd)
		case opError:
			if fd >= 0 {
				_ = os.NewFile(uintptr(fd), "stray").Close()
			}
			b.onError(f.Login, f.Msg)
		case opExited:
			if fd >= 0 {
				_ = os.NewFile(uintptr(fd), "stray").Close()
			}
			b.onExited(f.Login)
		default:
			if fd >= 0 {
				_ = os.NewFile(uintptr(fd), "stray").Close()
			}
		}
	}
}

func (b *Broker) onOpened(login string, fd int) {
	b.mu.Lock()
	sh := b.sessions[login]
	b.mu.Unlock()
	if sh == nil || fd < 0 {
		if fd >= 0 {
			_ = os.NewFile(uintptr(fd), "orphan").Close()
		}
		return
	}
	master := os.NewFile(uintptr(fd), "pty-master-"+login)
	sh.mu.Lock()
	sh.master = master
	// Seed the size from the PTY the Agent opened, so a watcher attaching before
	// any resize still mirrors the real grid rather than guessing.
	if ws, err := pty.GetsizeFull(master); err == nil && ws.Cols > 0 {
		sh.cols, sh.rows = ws.Cols, ws.Rows
	} else {
		sh.cols, sh.rows = 80, 24
	}
	sh.mu.Unlock()
	close(sh.ready)
	go b.pump(sh)
}

func (b *Broker) onError(login, msg string) {
	b.mu.Lock()
	sh := b.sessions[login]
	delete(b.sessions, login)
	b.mu.Unlock()
	if sh == nil {
		return
	}
	sh.openErr = errors.New(msg)
	close(sh.ready)
}

func (b *Broker) onExited(login string) {
	b.mu.Lock()
	sh := b.sessions[login]
	delete(b.sessions, login)
	b.mu.Unlock()
	if sh != nil {
		sh.close()
	}
}

// pump copies the shell's output into the scrollback ring and fans it out to
// subscribers until the master closes (shell exit).
func (b *Broker) pump(sh *shell) {
	buf := make([]byte, 32*1024)
	for {
		n, err := sh.master.Read(buf)
		if n > 0 {
			sh.broadcast(buf[:n])
		}
		if err != nil {
			break
		}
	}
	// The master closing means the shell is gone; ensure subscribers are
	// released even if the "exited" frame races or never arrives.
	b.mu.Lock()
	if b.sessions[sh.login] == sh {
		delete(b.sessions, sh.login)
	}
	b.mu.Unlock()
	sh.close()
}

// Attach returns the shell for login (opening one on first use), a channel of
// its subsequent output, a snapshot of recent scrollback, and an unsubscribe
// func. The same call serves the read-write owner and read-only Presenter
// watchers; write-eligibility is enforced by the caller.
func (b *Broker) Attach(login string) (Attachment, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return Attachment{}, errors.New("terminal broker closed")
	}
	sh := b.sessions[login]
	fresh := sh == nil
	if fresh {
		sh = newShell(login)
		b.sessions[login] = sh
	}
	b.mu.Unlock()

	if fresh {
		if err := b.conn.send(frame{Op: opOpen, Login: login}, -1); err != nil {
			b.mu.Lock()
			delete(b.sessions, login)
			b.mu.Unlock()
			return Attachment{}, err
		}
	}

	select {
	case <-sh.ready:
	case <-time.After(openTimeout):
		return Attachment{}, errors.New("timed out opening shell")
	}
	if sh.openErr != nil {
		return Attachment{}, sh.openErr
	}

	ch, id, scroll, cols, rows := sh.subscribe()
	if ch == nil {
		return Attachment{}, errors.New("shell already exited")
	}
	return Attachment{Out: ch, Scrollback: scroll, Cols: cols, Rows: rows, Close: func() { sh.unsubscribe(id) }}, nil
}

// Write sends p to the shell's stdin. Reachable only via the write-gated
// handler.
func (b *Broker) Write(login string, p []byte) error {
	sh := b.lookup(login)
	if sh == nil {
		return errors.New("no such terminal")
	}
	sh.mu.Lock()
	m := sh.master
	sh.mu.Unlock()
	if m == nil {
		return errors.New("terminal not ready")
	}
	_, err := m.Write(p)
	return err
}

// Resize sets the PTY window size. Reachable only via the write-gated handler.
func (b *Broker) Resize(login string, cols, rows uint16) error {
	sh := b.lookup(login)
	if sh == nil {
		return errors.New("no such terminal")
	}
	sh.mu.Lock()
	m := sh.master
	sh.mu.Unlock()
	if m == nil {
		return errors.New("terminal not ready")
	}
	if err := pty.Setsize(m, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
		return err
	}
	// Record and fan the new size out so read-only watchers (the Presenter)
	// mirror the controller's grid instead of fitting to their own window.
	sh.setSize(cols, rows)
	return nil
}

func (b *Broker) lookup(login string) *shell {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[login]
}

// Close tears down every shell and the socket. Idempotent.
func (b *Broker) Close() error {
	b.shutdown()
	return b.conn.close()
}

func (b *Broker) shutdown() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	shells := make([]*shell, 0, len(b.sessions))
	for _, sh := range b.sessions {
		shells = append(shells, sh)
	}
	b.sessions = map[string]*shell{}
	b.mu.Unlock()
	for _, sh := range shells {
		sh.close()
	}
}

// --- shell fan-out ---

func (sh *shell) broadcast(p []byte) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.ring = appendRing(sh.ring, p, scrollbackBytes)
	sh.emit(Event{Data: append([]byte(nil), p...)})
}

// setSize records the shell's current grid and fans the change out to watchers.
func (sh *shell) setSize(cols, rows uint16) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.cols, sh.rows = cols, rows
	sh.emit(Event{Cols: cols, Rows: rows})
}

// emit fans one event to all subscribers, dropping any that fall behind. The
// caller must hold sh.mu.
func (sh *shell) emit(ev Event) {
	for id, ch := range sh.subs {
		select {
		case ch <- ev:
		default:
			close(ch)
			delete(sh.subs, id)
		}
	}
}

// subscribe registers a watcher, returning its channel, id, a scrollback
// snapshot, and the current size. Returns a nil channel if the shell has
// already exited.
func (sh *shell) subscribe() (<-chan Event, int, []byte, uint16, uint16) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.done {
		return nil, 0, nil, 0, 0
	}
	ch := make(chan Event, subBuffer)
	id := sh.nextSub
	sh.nextSub++
	sh.subs[id] = ch
	scroll := append([]byte(nil), sh.ring...)
	return ch, id, scroll, sh.cols, sh.rows
}

func (sh *shell) unsubscribe(id int) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if ch, ok := sh.subs[id]; ok {
		close(ch)
		delete(sh.subs, id)
	}
}

// close marks the shell done, closes the master and all subscriber channels.
// Idempotent.
func (sh *shell) close() {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.done {
		return
	}
	sh.done = true
	if sh.master != nil {
		_ = sh.master.Close()
	}
	for id, ch := range sh.subs {
		close(ch)
		delete(sh.subs, id)
	}
}

// appendRing appends p to buf, keeping at most max trailing bytes.
func appendRing(buf, p []byte, max int) []byte {
	buf = append(buf, p...)
	if len(buf) > max {
		buf = append([]byte(nil), buf[len(buf)-max:]...)
	}
	return buf
}
