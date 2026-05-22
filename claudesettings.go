package main

import (
	"encoding/json"
	"errors"
	"fmt"
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

// claudeSettingsPaths returns the settings.json files that claude itself
// loads, in lowest-to-highest precedence. Project-level wins over
// user-level, matching claude's own resolution.
func claudeSettingsPaths(home, cwd string) []string {
	return []string{
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(cwd, ".claude", "settings.json"),
	}
}

// loadClaudeSettingsEnv merges env maps from claude's settings files
// (user-level then project-level), with later files overriding earlier
// ones.
func loadClaudeSettingsEnv(home, cwd string) map[string]string {
	out := map[string]string{}
	for _, p := range claudeSettingsPaths(home, cwd) {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s claudeSettings
		if json.Unmarshal(data, &s) != nil {
			continue
		}
		for k, v := range s.Env {
			out[k] = v
		}
	}
	return out
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

// materializeClaudeSettings rewrites each settings.json claude reads (both
// ~/.claude/settings.json and cwd/.claude/settings.json) with AWS
// credential-style env keys stripped, and returns Binds that overlay each
// sanitized copy at its original path inside the container. Host files
// are not modified. Files without AWS env keys are skipped.
func materializeClaudeSettings(home, cwd, outDir string) ([]Bind, error) {
	var binds []Bind
	for i, src := range claudeSettingsPaths(home, cwd) {
		b, err := sanitizeSettingsFile(src, filepath.Join(outDir, fmt.Sprintf("settings-%d.json", i)))
		if err != nil {
			return nil, err
		}
		if b != nil {
			binds = append(binds, *b)
		}
	}
	return binds, nil
}

func sanitizeSettingsFile(src, dst string) (*Bind, error) {
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
		return nil, nil
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
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(dst, out, 0644); err != nil {
		return nil, err
	}
	return &Bind{Path: dst, Dest: src}, nil
}
