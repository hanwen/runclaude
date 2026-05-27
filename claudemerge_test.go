package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectSettingsHasAWS(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"missing", "", false},
		{"no aws", `{"theme":"dark","env":{"FOO":"bar"}}`, false},
		{"aws_profile in env", `{"env":{"AWS_PROFILE":"x"}}`, true},
		{"aws region in env", `{"env":{"AWS_REGION":"us-east-1"}}`, true},
		{"bedrock in env", `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`, true},
		{"awsAuthRefresh top level", `{"awsAuthRefresh":"aws sso login"}`, true},
		{"malformed json", `{not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			if tc.content != "" {
				if err := os.MkdirAll(filepath.Join(cwd, ".claude"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cwd, ".claude", "settings.json"),
					[]byte(tc.content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if got := projectSettingsHasAWS(cwd); got != tc.want {
				t.Errorf("projectSettingsHasAWS = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMergeSettingsFilesBothPresent(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user.json")
	proj := filepath.Join(dir, "proj.json")
	dst := filepath.Join(dir, "out.json")
	if err := os.WriteFile(user, []byte(`{
  "theme": "user-theme",
  "awsAuthRefresh": "aws sso login --profile user",
  "env": {"AWS_PROFILE": "user", "FOO": "user-foo", "BAR": "user-bar"}
}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proj, []byte(`{
  "theme": "proj-theme",
  "env": {"AWS_PROFILE": "proj", "FOO": "proj-foo", "BAZ": "proj-baz"}
}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := mergeSettingsFiles(user, proj, dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["theme"] != "proj-theme" {
		t.Errorf("project should win at top level: theme=%v", got["theme"])
	}
	if _, ok := got["awsAuthRefresh"]; ok {
		t.Errorf("awsAuthRefresh should be stripped: %v", got["awsAuthRefresh"])
	}
	env := got["env"].(map[string]any)
	if _, ok := env["AWS_PROFILE"]; ok {
		t.Errorf("AWS_PROFILE should be stripped: %v", env)
	}
	if env["FOO"] != "proj-foo" {
		t.Errorf("project env entry should win: FOO=%v", env["FOO"])
	}
	if env["BAR"] != "user-bar" {
		t.Errorf("user-only env entry lost: BAR=%v", env["BAR"])
	}
	if env["BAZ"] != "proj-baz" {
		t.Errorf("project-only env entry lost: BAZ=%v", env["BAZ"])
	}
}

func TestMergeSettingsFilesUserOnly(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user.json")
	dst := filepath.Join(dir, "out.json")
	if err := os.WriteFile(user, []byte(`{"env":{"AWS_PROFILE":"user","FOO":"bar"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := mergeSettingsFiles(user, filepath.Join(dir, "missing.json"), dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	env := got["env"].(map[string]any)
	if _, ok := env["AWS_PROFILE"]; ok {
		t.Errorf("AWS_PROFILE should be stripped: %v", env)
	}
	if env["FOO"] != "bar" {
		t.Errorf("FOO=%v", env["FOO"])
	}
}

func TestMergeSettingsFilesBothMissing(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.json")
	if err := os.WriteFile(dst, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	err := mergeSettingsFiles(
		filepath.Join(dir, "missing-user.json"),
		filepath.Join(dir, "missing-proj.json"),
		dst,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("expected dst removed; stat err=%v", err)
	}
}

func TestBuildMergedClaudeDirSkipsCredentials(t *testing.T) {
	home, cwd, dest := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "merged")
	mustWrite(t, filepath.Join(home, ".claude", ".credentials.json"), `{"real":"secret"}`)
	mustWrite(t, filepath.Join(home, ".claude", "settings.json"), `{"theme":"dark"}`)

	if err := buildMergedClaudeDir(home, cwd, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".credentials.json")); !os.IsNotExist(err) {
		t.Errorf(".credentials.json should not be in merged dir; err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "settings.json")); err != nil {
		t.Errorf("settings.json missing from merged dir: %v", err)
	}
}

func TestBuildMergedClaudeDirProjectOverlay(t *testing.T) {
	home, cwd, dest := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "merged")
	// User: settings + skills/{a.md, shared.md}.
	mustWrite(t, filepath.Join(home, ".claude", "settings.json"), `{
  "env": {"AWS_PROFILE":"user","KEEP_USER":"1"}
}`)
	mustWrite(t, filepath.Join(home, ".claude", "skills", "a.md"), "user-a")
	mustWrite(t, filepath.Join(home, ".claude", "skills", "shared.md"), "user-shared")
	mustWrite(t, filepath.Join(home, ".claude", "agents", "alpha.md"), "user-alpha")

	// Project: settings + skills/{b.md, shared.md}; shared.md should win.
	mustWrite(t, filepath.Join(cwd, ".claude", "settings.json"), `{
  "env": {"AWS_PROFILE":"proj","KEEP_PROJ":"1"}
}`)
	mustWrite(t, filepath.Join(cwd, ".claude", "skills", "b.md"), "proj-b")
	mustWrite(t, filepath.Join(cwd, ".claude", "skills", "shared.md"), "proj-shared")

	if err := buildMergedClaudeDir(home, cwd, dest); err != nil {
		t.Fatal(err)
	}

	// Subdirectory union, project wins on conflict.
	for path, want := range map[string]string{
		"skills/a.md":      "user-a",
		"skills/b.md":      "proj-b",
		"skills/shared.md": "proj-shared",
		"agents/alpha.md":  "user-alpha",
	} {
		got, err := os.ReadFile(filepath.Join(dest, path))
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}

	// Settings: env merged, AWS_PROFILE scrubbed, KEEP_* preserved.
	data, err := os.ReadFile(filepath.Join(dest, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	env := s["env"].(map[string]any)
	if _, ok := env["AWS_PROFILE"]; ok {
		t.Errorf("AWS_PROFILE should be stripped: %v", env)
	}
	if env["KEEP_USER"] != "1" || env["KEEP_PROJ"] != "1" {
		t.Errorf("env merge lost an entry: %v", env)
	}
}

func TestBuildMergedClaudeDirRecreatesFromScratch(t *testing.T) {
	home, cwd, dest := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "merged")
	mustWrite(t, filepath.Join(home, ".claude", "skills", "first.md"), "first")
	if err := buildMergedClaudeDir(home, cwd, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "skills", "first.md")); err != nil {
		t.Fatalf("first.md missing after first build: %v", err)
	}

	// Remove first.md from the source and re-run; it must be gone from dest.
	if err := os.Remove(filepath.Join(home, ".claude", "skills", "first.md")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, ".claude", "skills", "second.md"), "second")
	if err := buildMergedClaudeDir(home, cwd, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "skills", "first.md")); !os.IsNotExist(err) {
		t.Errorf("stale first.md survived rebuild; err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "skills", "second.md")); err != nil {
		t.Errorf("second.md missing: %v", err)
	}
}

func TestBuildMergedClaudeDirNoUserHome(t *testing.T) {
	home, cwd, dest := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "merged")
	mustWrite(t, filepath.Join(cwd, ".claude", "settings.json"),
		`{"env":{"AWS_PROFILE":"proj","FOO":"bar"}}`)
	if err := buildMergedClaudeDir(home, cwd, dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	env := s["env"].(map[string]any)
	if _, ok := env["AWS_PROFILE"]; ok {
		t.Errorf("AWS_PROFILE should be stripped: %v", env)
	}
	if env["FOO"] != "bar" {
		t.Errorf("FOO=%v", env["FOO"])
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
