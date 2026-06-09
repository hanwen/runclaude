package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// projectSettingsHasAWS reports whether <cwd>/.claude/settings.json exists
// and contains any AWS-related key that would otherwise be scrubbed. When
// false, the project settings file is harmless and runclaude can leave it
// alone (no merged user-level dir needed).
func projectSettingsHasAWS(cwd string) bool {
	data, err := os.ReadFile(filepath.Join(cwd, ".claude", "settings.json"))
	if err != nil {
		return false
	}
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil {
		return false
	}
	for _, k := range awsClaudeSettingsTopLevelScrub {
		if _, ok := raw[k]; ok {
			return true
		}
	}
	if env, ok := raw["env"].(map[string]any); ok {
		for _, k := range awsClaudeSettingsEnvScrub {
			if _, ok := env[k]; ok {
				return true
			}
		}
	}
	return false
}

// buildMergedClaudeDir produces a merged copy of the user's ~/.claude/ and
// the project's <cwd>/.claude/ at destDir. The user tree is copied first
// (skipping credential files, live-state entries, and any pre-existing
// destDir contents), then the project tree is overlaid on top; project
// entries shadow user entries at the same relative path. settings.json gets
// a deep-merge with AWS env keys and awsAuthRefresh stripped.
//
// Live-state entries (history.jsonl, projects/, sessions/, etc.) are NOT
// copied; the caller bind-mounts them directly from ~/.claude on top of
// destDir so writes persist across invocations.
//
// destDir is bind-mounted as $HOME/.claude inside the container while
// claude is launched with --setting-sources user, so the in-container
// claude reads only this merged tree.
func buildMergedClaudeDir(home, cwd, destDir string) error {
	if err := os.RemoveAll(destDir); err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return err
	}
	userClaude := filepath.Join(home, ".claude")
	projClaude := filepath.Join(cwd, ".claude")

	if err := copyClaudeTree(userClaude, destDir, true, true); err != nil {
		return err
	}
	if err := copyClaudeTree(projClaude, destDir, false, false); err != nil {
		return err
	}

	mergedSettings := filepath.Join(destDir, "settings.json")
	if err := mergeSettingsFiles(
		filepath.Join(userClaude, "settings.json"),
		filepath.Join(projClaude, "settings.json"),
		mergedSettings,
	); err != nil {
		return err
	}
	return nil
}

// copyClaudeTree recursively copies src into dst. Existing entries at the
// same relative path are overwritten (so callers can layer trees by
// invoking with the lower-precedence tree first). Credential files at the
// top level are skipped when skipCreds is true. Live-state entries at the
// top level are skipped when skipLive is true.
func copyClaudeTree(src, dst string, skipCreds, skipLive bool) error {
	info, err := os.Stat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: not a directory", src)
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if filepath.Dir(rel) == "." {
			if skipCreds && credentialFiles[d.Name()] {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if skipLive && claudeLiveState[d.Name()] {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0700)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(out)
			return os.Symlink(target, out)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, out)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	si, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, si.Mode().Perm()|0200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// mergeSettingsFiles reads userPath and projPath as JSON objects, merges
// them (project wins; the "env" map is merged key-by-key), strips AWS env
// keys and top-level scrub keys, and writes the result to dst. Missing
// inputs are treated as empty. If both inputs are missing, dst is removed.
func mergeSettingsFiles(userPath, projPath, dst string) error {
	user, err := readJSONObject(userPath)
	if err != nil {
		return err
	}
	proj, err := readJSONObject(projPath)
	if err != nil {
		return err
	}
	if user == nil && proj == nil {
		_ = os.Remove(dst)
		return nil
	}
	merged := map[string]any{}
	for k, v := range user {
		merged[k] = v
	}
	for k, v := range proj {
		if k == "env" {
			merged["env"] = mergeEnvMaps(merged["env"], v)
			continue
		}
		merged[k] = v
	}
	if env, ok := merged["env"].(map[string]any); ok {
		for _, k := range awsClaudeSettingsEnvScrub {
			delete(env, k)
		}
		merged["env"] = env
	}
	for _, k := range awsClaudeSettingsTopLevelScrub {
		delete(merged, k)
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(dst, out, 0600)
}

func readJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return raw, nil
}

func mergeEnvMaps(dst, src any) map[string]any {
	out, _ := dst.(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	if s, ok := src.(map[string]any); ok {
		for k, v := range s {
			out[k] = v
		}
	}
	return out
}
