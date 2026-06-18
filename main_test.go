package main

import (
	"path/filepath"
	"testing"
)

// Scratch dirs must never be shared with the host: they are not in
// claudeLiveState (merged-path host bind) and are skipped by claudeBinds
// (normal-path host bind). They are instead backed by per-container tmpfs.
func TestClaudeScratchNotHostShared(t *testing.T) {
	for name := range claudeScratch {
		if claudeLiveState[name] {
			t.Errorf("%q is in both claudeScratch and claudeLiveState; "+
				"scratch dirs must not be host-shared", name)
		}
	}
}

func TestClaudeScratchBindsAreTmpfs(t *testing.T) {
	home := "/home/u"
	binds := claudeScratchBinds(home)
	if len(binds) != len(claudeScratch) {
		t.Fatalf("got %d scratch binds, want %d", len(binds), len(claudeScratch))
	}
	seen := map[string]bool{}
	for _, b := range binds {
		if !b.Tmpfs {
			t.Errorf("scratch bind %+v: Tmpfs not set", b)
		}
		if b.Path != "" {
			t.Errorf("scratch bind %+v: Path must be empty for tmpfs", b)
		}
		name := filepath.Base(b.dest())
		if !claudeScratch[name] {
			t.Errorf("scratch bind dest %q not a known scratch dir", b.dest())
		}
		want := filepath.Join(home, ".claude", name)
		if b.dest() != want {
			t.Errorf("scratch bind dest = %q, want %q", b.dest(), want)
		}
		seen[name] = true
	}
	for name := range claudeScratch {
		if !seen[name] {
			t.Errorf("scratch dir %q has no tmpfs bind", name)
		}
	}
}
