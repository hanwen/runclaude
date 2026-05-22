package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// awsClaudeSettingsScrub lists env keys we strip from .claude/settings.json
// before exposing it to the in-container claude. The container authenticates
// to Bedrock via the host proxy's re-signing, not its own AWS SDK; leaving
// these in causes claude to try the host profile / IMDS / etc.
var awsClaudeSettingsScrub = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_SECURITY_TOKEN",
	"AWS_PROFILE",
	"AWS_CONFIG_FILE",
	"AWS_SHARED_CREDENTIALS_FILE",
}

type claudeSettings struct {
	Env map[string]string `json:"env"`
}

// loadClaudeSettingsEnv returns the env map from cwd/.claude/settings.json,
// or nil if the file is missing or malformed.
func loadClaudeSettingsEnv(cwd string) map[string]string {
	data, err := os.ReadFile(filepath.Join(cwd, ".claude", "settings.json"))
	if err != nil {
		return nil
	}
	var s claudeSettings
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	return s.Env
}

// applyClaudeSettingsEnv sets host env vars from settings.env that aren't
// already in the process environment. Host env wins; settings.json only
// fills gaps. Returns the keys that were actually applied.
func applyClaudeSettingsEnv(env map[string]string) []string {
	var applied []string
	for k, v := range env {
		if os.Getenv(k) != "" {
			continue
		}
		_ = os.Setenv(k, v)
		applied = append(applied, k)
	}
	return applied
}

// materializeClaudeSettings rewrites cwd/.claude/settings.json into outDir
// with the AWS credential-style env keys stripped, and returns a Bind that
// overlays the sanitized copy at the original path inside the container.
// The host file is not modified.
func materializeClaudeSettings(cwd, outDir string) ([]Bind, error) {
	src := filepath.Join(cwd, ".claude", "settings.json")
	data, err := os.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil // leave malformed settings.json alone
	}
	envAny, ok := raw["env"].(map[string]any)
	if !ok {
		return nil, nil // nothing to sanitize
	}
	stripped := false
	for _, k := range awsClaudeSettingsScrub {
		if _, present := envAny[k]; present {
			delete(envAny, k)
			stripped = true
		}
	}
	if !stripped {
		return nil, nil
	}
	raw["env"] = envAny
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0700); err != nil {
		return nil, err
	}
	dst := filepath.Join(outDir, "claude-settings.json")
	if err := os.WriteFile(dst, out, 0644); err != nil {
		return nil, err
	}
	return []Bind{{Path: dst, Dest: src}}, nil
}
