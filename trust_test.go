package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureProjectTrusted(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	orig := `{
		"numStartups": 42,
		"largeNum": 12345678901234567890,
		"projects": {
			"/other": {"hasTrustDialogAccepted": true, "allowedTools": ["Bash"], "lastCost": 0.25}
		}
	}`
	if err := os.WriteFile(path, []byte(orig), 0600); err != nil {
		t.Fatal(err)
	}

	if err := ensureProjectTrusted(home, "/new/project"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	projects := root["projects"].(map[string]any)
	proj := projects["/new/project"].(map[string]any)
	if proj["hasTrustDialogAccepted"] != true || proj["hasCompletedProjectOnboarding"] != true {
		t.Errorf("new project not trusted: %v", proj)
	}
	// Everything else must survive the round trip untouched.
	other := projects["/other"].(map[string]any)
	if other["lastCost"] != 0.25 || other["allowedTools"].([]any)[0] != "Bash" {
		t.Errorf("existing project mangled: %v", other)
	}
	if string(data) == "" || !json.Valid(data) {
		t.Error("output not valid JSON")
	}
	// Large integers must not be float-mangled (UseNumber round trip).
	var raw struct {
		LargeNum json.Number `json:"largeNum"`
	}
	if err := json.Unmarshal(data, &raw); err != nil || raw.LargeNum.String() != "12345678901234567890" {
		t.Errorf("largeNum mangled: %q, %v", raw.LargeNum, err)
	}

	// Idempotent: already-trusted projects don't rewrite the file.
	before, _ := os.Stat(path)
	if err := ensureProjectTrusted(home, "/new/project"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Error("file rewritten despite already-trusted project")
	}
}

func TestEnsureProjectTrustedMissingFile(t *testing.T) {
	home := t.TempDir()
	if err := ensureProjectTrusted(home, "/p"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Error("missing ~/.claude.json must not be created (first-run onboarding is real)")
	}
}
