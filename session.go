package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// sessionInfo identifies a single runclaude invocation. It is written to
// <cacheDir>/sessions/<pid>.json on startup so concurrent sessions sharing a
// cwd can discover and identify one another. The cache home, /tmp and the
// merged .claude tree are still partly shared across same-cwd sessions, so two
// at once can interfere; the registry turns that from a silent surprise into a
// logged warning that names the other terminal.
type sessionInfo struct {
	PID      int      `json:"pid"`
	TTY      string   `json:"tty,omitempty"`
	Term     string   `json:"term,omitempty"`     // $TERM_PROGRAM
	TmuxPane string   `json:"tmuxPane,omitempty"` // $TMUX_PANE
	Title    string   `json:"title,omitempty"`    // best-effort window/title hint
	Command  []string `json:"command,omitempty"`
}

// describe renders a one-line human-readable identification of the session.
func (s sessionInfo) describe() string {
	parts := []string{fmt.Sprintf("pid %d", s.PID)}
	if s.TTY != "" {
		parts = append(parts, s.TTY)
	}
	if s.TmuxPane != "" {
		parts = append(parts, "tmux "+s.TmuxPane)
	}
	if s.Title != "" {
		parts = append(parts, fmt.Sprintf("%q", s.Title))
	} else if s.Term != "" {
		parts = append(parts, s.Term)
	}
	if len(s.Command) > 0 {
		parts = append(parts, "running "+strings.Join(s.Command, " "))
	}
	return strings.Join(parts, ", ")
}

// currentSessionInfo gathers identifying info about this process. The terminal
// title is not reliably queryable (it lives in the emulator and querying it
// requires raw-mode tty trickery that many emulators disable), so we fall back
// to the env hints terminals/multiplexers commonly export.
func currentSessionInfo(command []string) sessionInfo {
	s := sessionInfo{
		PID:      os.Getpid(),
		Term:     os.Getenv("TERM_PROGRAM"),
		TmuxPane: os.Getenv("TMUX_PANE"),
		Command:  command,
	}
	// Controlling terminal: whichever of stdin/stdout/stderr is a tty.
	for _, fd := range []int{0, 1, 2} {
		if name, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd)); err == nil {
			if strings.HasPrefix(name, "/dev/pts/") || strings.HasPrefix(name, "/dev/tty") {
				s.TTY = name
				break
			}
		}
	}
	// Best-effort terminal/tab hint. gnome-terminal exports
	// GNOME_TERMINAL_SCREEN (the tab's stable D-Bus screen path) — the closest
	// thing to a tab identifier, since gnome-terminal exposes no tab ordinal to
	// clients and won't answer tty title queries. Other terminals export their
	// own window/pane ids.
	for _, k := range []string{
		"GNOME_TERMINAL_SCREEN", "WEZTERM_PANE", "KITTY_WINDOW_ID",
		"WINDOWID", "TERM_TITLE",
	} {
		if v := os.Getenv(k); v != "" {
			s.Title = k + "=" + v
			break
		}
	}
	return s
}

// registerSession writes this session's info file and warns about any other
// live runclaude sessions in the same cwd. Stale files from dead processes are
// pruned. It returns a cleanup func to remove this session's file on exit.
func registerSession(cacheDir string, command []string) func() {
	dir := filepath.Join(cacheDir, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Printf("warning: session registry: %v", err)
		return func() {}
	}

	others := liveSessions(dir)
	if len(others) > 0 {
		var lines []string
		for _, o := range others {
			lines = append(lines, "  - "+o.describe())
		}
		log.Printf("warning: %d other runclaude session(s) share this cwd's cache "+
			"(%s); they share $HOME cache, /tmp and the merged ~/.claude tree, so "+
			"concurrent use may interfere:\n%s",
			len(others), cacheDir, strings.Join(lines, "\n"))
	}

	self := currentSessionInfo(command)
	path := filepath.Join(dir, strconv.Itoa(self.PID)+".json")
	if data, err := json.Marshal(self); err != nil {
		log.Printf("warning: session registry: %v", err)
	} else if err := os.WriteFile(path, data, 0600); err != nil {
		log.Printf("warning: session registry: %v", err)
	}
	return func() { _ = os.Remove(path) }
}

// liveSessions reads the registry dir, returning sessions whose process is
// still alive and removing files for processes that are not.
func liveSessions(dir string) []sessionInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var live []sessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s sessionInfo
		if json.Unmarshal(data, &s) != nil || s.PID == 0 {
			_ = os.Remove(path) // corrupt entry
			continue
		}
		if s.PID == self {
			continue
		}
		if !processAlive(s.PID) {
			_ = os.Remove(path) // stale: process gone
			continue
		}
		live = append(live, s)
	}
	return live
}

// processAlive reports whether a process with the given pid exists. signal 0
// performs error checking without delivering a signal: ESRCH means no such
// process; EPERM means it exists but we can't signal it (still alive).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
