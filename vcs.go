package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// vcsExcludeBinds returns Binds that overlay a copy of each per-clone VCS
// exclude file (git + jj-with-git-backend) at the path inside the container.
// The copy is augmented with pattern (idempotently). The host's original
// file is never modified.
//
// outDir is a host-side directory (typically a subdir of the runclaude
// cache) where the augmented copies live.
func vcsExcludeBinds(cwd, outDir, pattern string) ([]Bind, error) {
	excludePaths := map[string]struct{}{}
	if d, ok := resolveGitDir(cwd); ok {
		excludePaths[filepath.Join(d, "info", "exclude")] = struct{}{}
	}
	if d, ok := resolveJJGitStore(cwd); ok {
		excludePaths[filepath.Join(d, "info", "exclude")] = struct{}{}
	}
	if len(excludePaths) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(outDir, 0700); err != nil {
		return nil, err
	}
	var binds []Bind
	i := 0
	for hostPath := range excludePaths {
		i++
		copyPath := filepath.Join(outDir, "exclude"+strconv.Itoa(i))
		if err := writeExcludeCopy(hostPath, copyPath, pattern); err != nil {
			return nil, err
		}
		binds = append(binds, Bind{Path: copyPath, Dest: hostPath})
	}
	return binds, nil
}

func writeExcludeCopy(hostPath, copyPath, pattern string) error {
	existing, err := os.ReadFile(hostPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Already excluded?
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == pattern {
			return os.WriteFile(copyPath, existing, 0644)
		}
	}
	var out []byte
	if len(existing) == 0 {
		out = []byte(pattern + "\n")
	} else if existing[len(existing)-1] == '\n' {
		out = append(existing, []byte(pattern+"\n")...)
	} else {
		out = append(existing, []byte("\n"+pattern+"\n")...)
	}
	return os.WriteFile(copyPath, out, 0644)
}

func resolveGitDir(cwd string) (string, bool) {
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

func resolveJJGitStore(cwd string) (string, bool) {
	store := filepath.Join(cwd, ".jj", "repo", "store")
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
