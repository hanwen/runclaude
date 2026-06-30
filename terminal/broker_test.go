package terminal

import (
	"bytes"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// startReaper stands in for the pid-1 Wait4 loop: it harvests the agent's shell
// children and reports each exit, so the broker sees "exited" frames. It stops
// when the test signals done.
func startReaper(agent *Agent, done <-chan struct{}) {
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid > 0 {
				agent.OnReap(pid, shellExit(ws))
				continue
			}
			_ = err
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

func shellExit(ws syscall.WaitStatus) int {
	switch {
	case ws.Exited():
		return ws.ExitStatus()
	case ws.Signaled():
		return 128 + int(ws.Signal())
	default:
		return 0
	}
}

// waitFor accumulates stream output until it contains sub, failing on timeout or
// premature close.
func waitFor(t *testing.T, out <-chan Event, sub string, d time.Duration) []byte {
	t.Helper()
	var buf []byte
	to := time.After(d)
	for {
		select {
		case ev, ok := <-out:
			if !ok {
				t.Fatalf("stream closed before seeing %q; got:\n%s", sub, buf)
			}
			buf = append(buf, ev.Data...)
			if bytes.Contains(buf, []byte(sub)) {
				return buf
			}
		case <-to:
			t.Fatalf("timeout waiting for %q; got:\n%s", sub, buf)
		}
	}
}

func newPair(t *testing.T) (host, sandbox int) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	return fds[0], fds[1]
}

func TestBrokerAgentRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	hostFd, sandFd := newPair(t)
	env := []string{"SHELL=/bin/sh", "PATH=/usr/bin:/bin"}
	agent := NewAgent(sandFd, t.TempDir(), env)
	go agent.Serve()

	done := make(chan struct{})
	defer close(done)
	startReaper(agent, done)

	broker := NewBroker(hostFd)
	defer broker.Close()

	// First attach opens a shell.
	a1, err := broker.Attach("alice")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := broker.Write("alice", []byte("echo hello123\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, a1.Out, "hello123", 5*time.Second)

	// Persistence: a second attach for the same login reuses the running shell,
	// so its scrollback already carries the earlier output (a fresh shell would
	// not).
	a2, err := broker.Attach("alice")
	if err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	if !bytes.Contains(a2.Scrollback, []byte("hello123")) {
		t.Fatalf("re-attach scrollback missing prior output; got:\n%s", a2.Scrollback)
	}
	// Output fans out to both subscribers.
	if err := broker.Write("alice", []byte("echo second99\n")); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	waitFor(t, a1.Out, "second99", 5*time.Second)
	waitFor(t, a2.Out, "second99", 5*time.Second)
	a1.Close()
	a2.Close()

	// Resize should not error on a live shell, and a watcher should learn the
	// new size via a size event.
	a5, err := broker.Attach("alice")
	if err != nil {
		t.Fatalf("attach for resize: %v", err)
	}
	if err := broker.Resize("alice", 100, 30); err != nil {
		t.Fatalf("resize: %v", err)
	}
	sawSize := false
	st := time.After(5 * time.Second)
	for !sawSize {
		select {
		case ev, ok := <-a5.Out:
			if !ok {
				t.Fatal("stream closed before size event")
			}
			if ev.Data == nil && ev.Cols == 100 && ev.Rows == 30 {
				sawSize = true
			}
		case <-st:
			t.Fatal("no size event after resize")
		}
	}
	a5.Close()

	// Exiting the shell drops the session: the subscription closes.
	a3, err := broker.Attach("alice")
	if err != nil {
		t.Fatalf("attach 3: %v", err)
	}
	defer a3.Close()
	if err := broker.Write("alice", []byte("exit\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	closed := false
	to := time.After(5 * time.Second)
	for !closed {
		select {
		case _, ok := <-a3.Out:
			if !ok {
				closed = true
			}
		case <-to:
			t.Fatal("subscription did not close after shell exit")
		}
	}

	// After exit a fresh attach spawns a new shell with empty scrollback.
	a4, err := broker.Attach("alice")
	if err != nil {
		t.Fatalf("attach 4: %v", err)
	}
	defer a4.Close()
	if bytes.Contains(a4.Scrollback, []byte("hello123")) {
		t.Fatalf("new shell unexpectedly carried old scrollback:\n%s", a4.Scrollback)
	}
}
