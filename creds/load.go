package creds

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// AnthropicCredentials are the fields needed to inject Anthropic auth into an
// outgoing request. Exactly one of APIKey or Bearer is non-empty. When Bearer
// is set, RefreshToken and Path may also be set so the caller can rotate the
// access token on a 401 (see RefreshTransport).
type AnthropicCredentials struct {
	APIKey       string
	Bearer       string
	RefreshToken string
	// Path is the credentials file the Bearer was loaded from, or "" if it
	// came from $ANTHROPIC_API_KEY. Refreshed tokens are persisted back to
	// this path when set.
	Path string
}

// LoadAnthropicCredentials reads ~/.claude/.credentials.json (preferred) or
// falls back to $ANTHROPIC_API_KEY. Returns an error if no credentials are
// found.
func LoadAnthropicCredentials(home string) (AnthropicCredentials, error) {
	for _, name := range []string{".credentials.json", "credentials.json"} {
		p := filepath.Join(home, ".claude", name)
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			continue
		}
		var parsed struct {
			ClaudeAiOauth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
			} `json:"claudeAiOauth"`
			APIKey string `json:"apiKey"`
		}
		if json.Unmarshal(data, &parsed) == nil {
			if parsed.ClaudeAiOauth.AccessToken != "" {
				return AnthropicCredentials{
					Bearer:       parsed.ClaudeAiOauth.AccessToken,
					RefreshToken: parsed.ClaudeAiOauth.RefreshToken,
					Path:         p,
				}, nil
			}
			if parsed.APIKey != "" {
				return AnthropicCredentials{APIKey: parsed.APIKey}, nil
			}
		}
	}
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		return AnthropicCredentials{APIKey: k}, nil
	}
	return AnthropicCredentials{}, fmt.Errorf("no claude credentials found (looked in ~/.claude/.credentials.json and $ANTHROPIC_API_KEY)")
}
