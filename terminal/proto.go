// Package terminal gives a shared runclaude session an in-browser shell per
// participant. A shell runs inside the sandbox (same rootfs, dropped caps, netns
// egress restrictions, MITM proxy env as the sandboxed claude) while its PTY is
// driven from the host web server.
//
// The split mirrors the rest of runclaude's "sandboxed process, host-side
// surface" design: a single SOCK_SEQPACKET control socket is threaded from the
// host into the sandbox. The sandbox-side Agent opens a PTY, forks the user's
// shell on its slave, and passes the PTY master fd back to the host over the
// socket (SCM_RIGHTS). The host-side Broker then reads/writes the master and
// bridges it to a browser WebSocket. The shell process never leaves the sandbox;
// only an fd crosses the boundary.
//
// Frames are one JSON object per datagram (SEQPACKET preserves boundaries):
//
//	host   -> sandbox  {"op":"open","login":...}
//	sandbox -> host    {"op":"opened","login":...}  + 1 fd (PTY master)
//	sandbox -> host    {"op":"error","login":...,"msg":...}
//	sandbox -> host    {"op":"exited","login":...,"code":N}   (async on exit)
package terminal

import (
	"encoding/json"
	"io"
	"sync"

	"golang.org/x/sys/unix"
)

// frame is one control message exchanged over the term socket. Op selects the
// kind; the other fields are populated as the op requires.
type frame struct {
	Op    string `json:"op"`
	Login string `json:"login"`
	Msg   string `json:"msg,omitempty"`
	Code  int    `json:"code,omitempty"`
}

const (
	opOpen   = "open"   // host -> sandbox: open (or confirm) a shell for Login
	opOpened = "opened" // sandbox -> host: shell ready; one fd (master) attached
	opError  = "error"  // sandbox -> host: open failed (Msg explains)
	opExited = "exited" // sandbox -> host: shell process exited (Code = status)
)

// conn is a framed message channel over one end of a SOCK_SEQPACKET socketpair.
// A single goroutine reads (recv); writes (send) may come from several
// goroutines, so they are serialized. The underlying fd is owned by conn.
type conn struct {
	fd  int
	wmu sync.Mutex
}

func newConn(fd int) *conn { return &conn{fd: fd} }

// send writes one frame. If passFd >= 0 it is sent as an SCM_RIGHTS ancillary
// message; the kernel dups it into the socket buffer, so the caller may close
// its copy as soon as send returns.
func (c *conn) send(f frame, passFd int) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	var oob []byte
	if passFd >= 0 {
		oob = unix.UnixRights(passFd)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return unix.Sendmsg(c.fd, data, oob, nil, 0)
}

// recv reads one frame and, if the peer attached one, a received fd (else -1).
// A zero-length datagram from an orderly peer close is reported as io.EOF.
func (c *conn) recv() (frame, int, error) {
	buf := make([]byte, 64*1024)
	oob := make([]byte, unix.CmsgSpace(4)) // room for a single fd
	n, oobn, _, _, err := unix.Recvmsg(c.fd, buf, oob, 0)
	if err != nil {
		return frame{}, -1, err
	}
	if n == 0 && oobn == 0 {
		return frame{}, -1, io.EOF
	}

	gotFd := -1
	if oobn > 0 {
		if scms, perr := unix.ParseSocketControlMessage(oob[:oobn]); perr == nil {
			for _, scm := range scms {
				if fds, ferr := unix.ParseUnixRights(&scm); ferr == nil {
					for _, fd := range fds {
						if gotFd == -1 {
							gotFd = fd
						} else {
							// Defensive: never expect more than one fd per frame.
							unix.Close(fd)
						}
					}
				}
			}
		}
	}

	var f frame
	if jerr := json.Unmarshal(buf[:n], &f); jerr != nil {
		if gotFd >= 0 {
			unix.Close(gotFd)
		}
		return frame{}, -1, jerr
	}
	return f, gotFd, nil
}

// close releases the underlying socket fd.
func (c *conn) close() error { return unix.Close(c.fd) }
