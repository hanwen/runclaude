package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVCSExcludeBindsGit(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	info := filepath.Join(dir, ".git", "info")
	if err := os.MkdirAll(info, 0755); err != nil {
		t.Fatal(err)
	}
	hostExclude := filepath.Join(info, "exclude")
	if err := os.WriteFile(hostExclude, []byte("# user excludes\nfoo\n"), 0644); err != nil {
		t.Fatal(err)
	}

	binds, err := vcsExcludeBinds(dir, outDir, ".claude/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 1 {
		t.Fatalf("expected 1 bind, got %d", len(binds))
	}
	if binds[0].Dest != hostExclude {
		t.Errorf("Dest = %q, want %q", binds[0].Dest, hostExclude)
	}

	// Host file untouched.
	hostAfter, _ := os.ReadFile(hostExclude)
	if string(hostAfter) != "# user excludes\nfoo\n" {
		t.Errorf("host file was modified: %q", hostAfter)
	}

	// Copy contains both original lines and the new pattern.
	got, err := os.ReadFile(binds[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	want := "# user excludes\nfoo\n.claude/settings.json\n"
	if string(got) != want {
		t.Errorf("copy = %q, want %q", got, want)
	}
}

func TestVCSExcludeBindsGitFile(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	gitdir := filepath.Join(dir, "linked", "wt")
	if err := os.MkdirAll(filepath.Join(gitdir, "info"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"),
		[]byte("gitdir: "+gitdir+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	binds, err := vcsExcludeBinds(dir, outDir, ".claude/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 1 {
		t.Fatalf("expected 1 bind, got %d", len(binds))
	}
	if binds[0].Dest != filepath.Join(gitdir, "info", "exclude") {
		t.Errorf("Dest = %q", binds[0].Dest)
	}
}

func TestVCSExcludeBindsJJ(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	store := filepath.Join(dir, ".jj", "repo", "store")
	gitdir := filepath.Join(store, "git")
	if err := os.MkdirAll(filepath.Join(gitdir, "info"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "type"), []byte("git\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "git_target"), []byte("git\n"), 0644); err != nil {
		t.Fatal(err)
	}

	binds, err := vcsExcludeBinds(dir, outDir, ".claude/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 1 {
		t.Fatalf("expected 1 bind, got %d", len(binds))
	}
	got, err := os.ReadFile(binds[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), ".claude/settings.json") {
		t.Errorf("copy missing pattern: %q", got)
	}
}

func TestVCSExcludeBindsColocatedDedup(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	gitInfo := filepath.Join(dir, ".git", "info")
	if err := os.MkdirAll(gitInfo, 0755); err != nil {
		t.Fatal(err)
	}
	// jj store with git_target pointing back to the colocated .git.
	store := filepath.Join(dir, ".jj", "repo", "store")
	if err := os.MkdirAll(store, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "type"), []byte("git\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "git_target"),
		[]byte("../../../.git\n"), 0644); err != nil {
		t.Fatal(err)
	}

	binds, err := vcsExcludeBinds(dir, outDir, ".claude/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 1 {
		t.Fatalf("expected dedup to 1 bind, got %d: %+v", len(binds), binds)
	}
}

func TestVCSExcludeBindsAlreadyPresent(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	info := filepath.Join(dir, ".git", "info")
	if err := os.MkdirAll(info, 0755); err != nil {
		t.Fatal(err)
	}
	hostExclude := filepath.Join(info, "exclude")
	hostContent := "foo\n.claude/settings.json\nbar\n"
	if err := os.WriteFile(hostExclude, []byte(hostContent), 0644); err != nil {
		t.Fatal(err)
	}
	binds, err := vcsExcludeBinds(dir, outDir, ".claude/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(binds[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != hostContent {
		t.Errorf("copy = %q, want unchanged %q", got, hostContent)
	}
}

func TestVCSExcludeBindsNoRepo(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	binds, err := vcsExcludeBinds(dir, outDir, ".claude/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 0 {
		t.Errorf("expected no binds, got %+v", binds)
	}
}
