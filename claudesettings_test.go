package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeSettingsStripsAWSEnv(t *testing.T) {
	src := filepath.Join(t.TempDir(), "settings.json")
	in := []byte(`{
  "awsAuthRefresh": "aws sso login --profile myprofile",
  "env": {
    "AWS_PROFILE": "myprofile",
    "AWS_ACCESS_KEY_ID": "AKIA-host",
    "AWS_SECRET_ACCESS_KEY": "secret-host",
    "AWS_SESSION_TOKEN": "token-host",
    "AWS_REGION": "us-east-1",
    "CLAUDE_CODE_USE_BEDROCK": "1"
  }
}`)
	if err := os.WriteFile(src, in, 0644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out.json")
	b, err := sanitizeSettingsFile(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil {
		t.Fatal("expected a Bind, got nil")
	}
	if b.Path != dst || b.Dest != src {
		t.Errorf("bind = %+v", b)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	env := raw["env"].(map[string]any)
	for _, k := range []string{
		"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	} {
		if _, ok := env[k]; ok {
			t.Errorf("env[%q] should be stripped, kept; full env: %v", k, env)
		}
	}
	for _, k := range []string{"AWS_REGION", "CLAUDE_CODE_USE_BEDROCK"} {
		if _, ok := env[k]; ok {
			t.Errorf("env[%q] should be stripped; full env: %v", k, env)
		}
	}
	if _, ok := raw["awsAuthRefresh"]; ok {
		t.Errorf("awsAuthRefresh should be stripped, got: %v", raw["awsAuthRefresh"])
	}

	// Host file untouched.
	host, _ := os.ReadFile(src)
	if string(host) != string(in) {
		t.Errorf("host file modified:\nbefore: %q\nafter:  %q", in, host)
	}
}

func TestSanitizeSettingsNoEnvNoBind(t *testing.T) {
	src := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(src, []byte(`{"theme":"dark"}`), 0644); err != nil {
		t.Fatal(err)
	}
	b, err := sanitizeSettingsFile(src, filepath.Join(t.TempDir(), "out.json"))
	if err != nil {
		t.Fatal(err)
	}
	if b != nil {
		t.Errorf("expected nil bind for settings without env, got %+v", b)
	}
}

func TestLoadClaudeSettings(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{
  "awsAuthRefresh": "aws sso login --profile user",
  "env": {"AWS_PROFILE": "user", "FOO": "bar"}
}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".claude", "settings.json"), []byte(`{
  "env": {"AWS_PROFILE": "project"}
}`), 0644); err != nil {
		t.Fatal(err)
	}
	s := loadClaudeSettings(home, cwd)
	if s.AwsAuthRefresh != "aws sso login --profile user" {
		t.Errorf("AwsAuthRefresh = %q", s.AwsAuthRefresh)
	}
	if s.Env["AWS_PROFILE"] != "project" {
		t.Errorf("project should override user; AWS_PROFILE = %q", s.Env["AWS_PROFILE"])
	}
	if s.Env["FOO"] != "bar" {
		t.Errorf("user-level entry lost: FOO = %q", s.Env["FOO"])
	}
}

func TestSanitizeSettingsNoAWSNoBind(t *testing.T) {
	src := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(src, []byte(`{"env":{"FOO":"bar"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	b, err := sanitizeSettingsFile(src, filepath.Join(t.TempDir(), "out.json"))
	if err != nil {
		t.Fatal(err)
	}
	if b != nil {
		t.Errorf("expected nil bind for env without AWS keys, got %+v", b)
	}
}
