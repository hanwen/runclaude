package record

// Resolution of the git object store backing a working directory: a plain
// .git, a linked git worktree, or a jj repo's git backend store. Used by
// the recorder to find where session refs live; exported because the
// sandbox setup also keys VCS-specific behavior off these probes.

import (
	"os"
	"path/filepath"
	"strings"
)

// ResolveGitDir returns cwd's git dir: .git itself, or — for a linked
// worktree, where .git is a pointer file — the dir it points at.
func ResolveGitDir(cwd string) (string, bool) {
	p := filepath.Join(cwd, ".git")
	fi, err := os.Stat(p)
	if err != nil {
		return "", false
	}
	if fi.IsDir() {
		return p, true
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	rest, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", false
	}
	gd := strings.TrimSpace(rest)
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(cwd, gd)
	}
	return filepath.Clean(gd), true
}

// jjRepoDir returns cwd's jj repo dir. In the main workspace .jj/repo is
// the dir itself; a linked workspace (`jj workspace add`) has a pointer
// file there instead, holding the main repo dir's path (relative to .jj).
func jjRepoDir(cwd string) (string, bool) {
	p := filepath.Join(cwd, ".jj", "repo")
	fi, err := os.Stat(p)
	if err != nil {
		return "", false
	}
	if fi.IsDir() {
		return p, true
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	repo := strings.TrimSpace(string(data))
	if !filepath.IsAbs(repo) {
		repo = filepath.Join(cwd, ".jj", repo)
	}
	return filepath.Clean(repo), true
}

// ResolveJJGitStore returns the git object store of cwd's jj repo when it
// uses the git backend.
func ResolveJJGitStore(cwd string) (string, bool) {
	repo, ok := jjRepoDir(cwd)
	if !ok {
		return "", false
	}
	store := filepath.Join(repo, "store")
	typ, err := os.ReadFile(filepath.Join(store, "type"))
	if err != nil || strings.TrimSpace(string(typ)) != "git" {
		return "", false
	}
	target, err := os.ReadFile(filepath.Join(store, "git_target"))
	if err != nil {
		return "", false
	}
	gd := strings.TrimSpace(string(target))
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(store, gd)
	}
	return filepath.Clean(gd), true
}

// ResolveRecordGitDir returns the git object store backing cwd for session
// recording: the repo's own .git, or a jj repo's git backend store — which
// also covers jj workspaces, whose dirs have no .git at all.
func ResolveRecordGitDir(cwd string) (string, bool) {
	if d, ok := ResolveGitDir(cwd); ok {
		return d, true
	}
	return ResolveJJGitStore(cwd)
}
