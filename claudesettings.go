package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// awsClaudeSettingsEnvScrub lists env keys we strip from the "env" map in
// .claude/settings.json before exposing it to the in-container claude. The
// container authenticates to Bedrock via the host proxy's re-signing, not its
// own AWS SDK; leaving these in causes claude to try the host profile / IMDS.
var awsClaudeSettingsEnvScrub = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_SECURITY_TOKEN",
	"AWS_PROFILE",
	"AWS_CONFIG_FILE",
	"AWS_SHARED_CREDENTIALS_FILE",
	// These tell claude to use Bedrock directly; in-container claude must not
	// do its own Bedrock auth — the host proxy handles signing.
	"CLAUDE_CODE_USE_BEDROCK",
	"AWS_REGION",
	"AWS_DEFAULT_REGION",
}

// awsClaudeSettingsTopLevelScrub lists top-level keys we strip from
// .claude/settings.json. awsAuthRefresh tells claude to run `aws sso login`
// when credentials fail; that would reference a host profile unavailable in
// the container and is unnecessary since the proxy handles re-signing.
var awsClaudeSettingsTopLevelScrub = []string{
	"awsAuthRefresh",
}

type claudeSettings struct {
	Env map[string]string `json:"env"`
	// AwsAuthRefresh is a shell command runclaude executes when AWS
	// credentials fail to load (e.g. expired SSO). Read host-side; stripped
	// from the in-container settings.json.
	AwsAuthRefresh string `json:"awsAuthRefresh"`
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

// loadClaudeSettings merges claude's settings files (user-level then
// project-level), with later files overriding earlier ones.
func loadClaudeSettings(home, cwd string) claudeSettings {
	out := claudeSettings{Env: map[string]string{}}
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
			out.Env[k] = v
		}
		if s.AwsAuthRefresh != "" {
			out.AwsAuthRefresh = s.AwsAuthRefresh
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
	stripped := false
	if envAny, ok := raw["env"].(map[string]any); ok {
		for _, k := range awsClaudeSettingsEnvScrub {
			if _, present := envAny[k]; present {
				delete(envAny, k)
				stripped = true
			}
		}
		raw["env"] = envAny
	}
	for _, k := range awsClaudeSettingsTopLevelScrub {
		if _, present := raw[k]; present {
			delete(raw, k)
			stripped = true
		}
	}
	if !stripped {
		return nil, nil
	}
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
