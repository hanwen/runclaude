package terminal

import (
	"log"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// Agent runs inside the sandbox (in the pid-1 init process). It serves "open"
// requests from the host by opening a PTY, forking the user's shell on its
// slave, and sending the PTY master fd back to the host. It does not wait on the
// shells itself: pid 1's single Wait4(-1) reaper is authoritative and reports
// exits via OnReap, which the Agent turns into "exited" frames.
type Agent struct {
	conn *conn
	cwd  string
	env  []string // base environment for spawned shells

	mu     sync.Mutex
	byPID  map[int]string // pid -> login, for reaper attribution
	closed bool
}

// NewAgent builds an Agent over the sandbox end of the term socket. cwd is the
// shell's working directory (the session checkout); env is the container
// environment shells inherit (TERM is forced on top).
func NewAgent(fd int, cwd string, env []string) *Agent {
	return &Agent{
		conn:  newConn(fd),
		cwd:   cwd,
		env:   env,
		byPID: map[int]string{},
	}
}

// Serve reads requests until the socket closes. Run it in its own goroutine.
func (a *Agent) Serve() {
	for {
		f, _, err := a.conn.recv()
		if err != nil {
			return
		}
		switch f.Op {
		case opOpen:
			a.handleOpen(f.Login)
		}
	}
}

// handleOpen spawns a fresh shell for login and ships its PTY master to the
// host. Each open spawns a new shell; the host Broker guarantees it only sends
// open when it has no live shell for the login (persistence is host-side).
func (a *Agent) handleOpen(login string) {
	master, pid, err := a.spawn()
	if err != nil {
		log.Printf("terminal: open shell for %q: %v", login, err)
		_ = a.conn.send(frame{Op: opError, Login: login, Msg: err.Error()}, -1)
		return
	}
	a.mu.Lock()
	a.byPID[pid] = login
	a.mu.Unlock()

	// Hand the master to the host, then drop our copy: the host owns terminal
	// I/O and the slave keeps the shell alive until the host closes the master.
	if err := a.conn.send(frame{Op: opOpened, Login: login}, int(master.Fd())); err != nil {
		log.Printf("terminal: send master for %q: %v", login, err)
	}
	_ = master.Close()
}

// spawn opens a PTY and starts the user's shell on its slave in a new session
// with the slave as controlling terminal. It returns the PTY master and the
// shell pid; the shell is left for the pid-1 reaper to wait on.
func (a *Agent) spawn() (*os.File, int, error) {
	cmd := exec.Command(a.shell())
	cmd.Dir = a.cwd
	cmd.Env = append(append([]string{}, a.env...), "TERM=xterm-256color")
	// pty.StartWithSize sets Setsid + Setctty and closes the slave in the parent,
	// leaving us only the master. It calls cmd.Start (no Wait).
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return nil, 0, err
	}
	return master, cmd.Process.Pid, nil
}

// shell picks the login shell: $SHELL, then bash, then sh.
func (a *Agent) shell() string {
	for _, e := range a.env {
		if len(e) > 6 && e[:6] == "SHELL=" {
			if sh := e[6:]; sh != "" {
				return sh
			}
		}
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "/bin/sh"
}

// OnReap is called by the pid-1 reaper for every child it harvests. If the pid
// is one of our shells, it reports the exit to the host and forgets the pid.
// Unknown pids (claude, orphans) are ignored.
func (a *Agent) OnReap(pid, code int) {
	a.mu.Lock()
	login, ok := a.byPID[pid]
	if ok {
		delete(a.byPID, pid)
	}
	a.mu.Unlock()
	if !ok {
		return
	}
	_ = a.conn.send(frame{Op: opExited, Login: login, Code: code}, -1)
}
