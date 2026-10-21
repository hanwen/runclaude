package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/hanwen/runclaude/proxy"
)

func writeSession(t *testing.T, dir string, s sessionInfo) {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(s.PID)+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLiveSessionsPrunesDeadAndExcludesSelf(t *testing.T) {
	dir := t.TempDir()

	// A live session: use our own pid, which is guaranteed alive but must be
	// excluded as "self".
	writeSession(t, dir, sessionInfo{PID: os.Getpid(), TTY: "/dev/pts/9"})

	// A dead session: pid 0x7FFFFFFE is exceedingly unlikely to exist.
	deadPID := 0x7ffffffe
	writeSession(t, dir, sessionInfo{PID: deadPID, TTY: "/dev/pts/8"})

	live := liveSessions(dir)
	if len(live) != 0 {
		t.Fatalf("expected 0 live sessions (self excluded, dead pruned), got %d: %+v", len(live), live)
	}
	// The dead session's file should have been pruned.
	if _, err := os.Stat(filepath.Join(dir, strconv.Itoa(deadPID)+".json")); !os.IsNotExist(err) {
		t.Errorf("dead session file not pruned (err=%v)", err)
	}
	// Self file should remain (we only skip it in the result, not delete it).
	if _, err := os.Stat(filepath.Join(dir, strconv.Itoa(os.Getpid())+".json")); err != nil {
		t.Errorf("self session file should remain: %v", err)
	}
}

func TestLiveSessionsDropsCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "123.json"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if live := liveSessions(dir); len(live) != 0 {
		t.Fatalf("expected corrupt file ignored, got %+v", live)
	}
	if _, err := os.Stat(filepath.Join(dir, "123.json")); !os.IsNotExist(err) {
		t.Errorf("corrupt session file not pruned (err=%v)", err)
	}
}

func TestRunApproveScopesToCwdAndApproves(t *testing.T) {
	allow := proxy.NewAllowlist(nil)
	srv := httptest.NewServer(proxy.NewApprover(allow, log.New(io.Discard, "", 0)))
	defer srv.Close()

	cacheDir := t.TempDir()
	dir := filepath.Join(cacheDir, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// A live session (our parent is alive and is not this process) exposing the
	// approval server, plus a dead session that must be ignored.
	writeSession(t, dir, sessionInfo{PID: os.Getppid(), ApproveURL: srv.URL})
	writeSession(t, dir, sessionInfo{PID: 0x7ffffffe, ApproveURL: srv.URL})

	if err := runApprove(cacheDir, []string{"pkg.example.com"}); err != nil {
		t.Fatalf("runApprove: %v", err)
	}
	if !allow.Match("pkg.example.com") {
		t.Fatal("host should be allowed after runApprove")
	}
}

func TestRegisterSessionRoundTrip(t *testing.T) {
	cacheDir := t.TempDir()
	cleanup := registerSession(cacheDir, []string{"claude", "--foo"}, "http://127.0.0.1:12345")
	path := filepath.Join(cacheDir, "sessions", strconv.Itoa(os.Getpid())+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("session file not written: %v", err)
	}
	var s sessionInfo
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", s.PID, os.Getpid())
	}
	if len(s.Command) != 2 || s.Command[0] != "claude" {
		t.Errorf("Command = %v, want [claude --foo]", s.Command)
	}
	if s.ApproveURL != "http://127.0.0.1:12345" {
		t.Errorf("ApproveURL = %q, want %q", s.ApproveURL, "http://127.0.0.1:12345")
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove session file (err=%v)", err)
	}
}
