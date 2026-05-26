package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// claudeOAuthClientID is the public OAuth client id used by the Claude Code
// CLI. Overridable via RUNCLAUDE_CLAUDE_CLIENT_ID for cases where Anthropic
// rotates it.
const claudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// claudeOAuthTokenURL is the refresh endpoint. Overridable via
// RUNCLAUDE_CLAUDE_TOKEN_URL for tests.
const claudeOAuthTokenURL = "https://console.anthropic.com/v1/oauth/token"

// tokenSource owns the live Anthropic OAuth credential. It is the only place
// in runclaude that knows how to swap a stale access token for a fresh one;
// the proxy's inject path calls Bearer() and refreshTransport calls Refresh()
// reactively on a 401.
type tokenSource struct {
	mu           sync.Mutex
	accessToken  string
	refreshToken string
	path         string // ~/.claude/.credentials.json, "" if not persisted
	clientID     string
	tokenURL     string
	httpc        *http.Client
	logger       *log.Logger

	// lastRefresh guards against thundering-herd retries — if two requests
	// race and both see 401, we only want to hit the OAuth endpoint once.
	lastRefresh time.Time
}

func newTokenSource(accessToken, refreshToken, path string, logger *log.Logger) *tokenSource {
	cid := os.Getenv("RUNCLAUDE_CLAUDE_CLIENT_ID")
	if cid == "" {
		cid = claudeOAuthClientID
	}
	url := os.Getenv("RUNCLAUDE_CLAUDE_TOKEN_URL")
	if url == "" {
		url = claudeOAuthTokenURL
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	logger.Printf("oauth: token source initialized url=%s client=%s access=%s refresh=%s persisted=%v",
		url, cid, redactToken(accessToken), redactToken(refreshToken), path != "")
	return &tokenSource{
		accessToken:  accessToken,
		refreshToken: refreshToken,
		path:         path,
		clientID:     cid,
		tokenURL:     url,
		httpc:        &http.Client{Timeout: 30 * time.Second},
		logger:       logger,
	}
}

// redactToken returns a loggable fingerprint of a secret: its length and a
// short prefix, never the full value. Useful for confirming which credential
// is in play without leaking it into the proxy log.
func redactToken(tok string) string {
	if tok == "" {
		return "<empty>"
	}
	n := len(tok)
	if n > 8 {
		return fmt.Sprintf("%s…(len=%d)", tok[:8], n)
	}
	return fmt.Sprintf("…(len=%d)", n)
}

func (t *tokenSource) Bearer() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.accessToken
}

// Refresh exchanges the refresh token for a new access+refresh pair. If
// staleBearer matches the token we currently hold, we actually hit the network;
// otherwise another goroutine has already refreshed and we just return.
func (t *tokenSource) Refresh(ctx context.Context, staleBearer string) error {
	rt, clientID, tokenURL, proceed, err := t.snapshot(staleBearer)
	if err != nil {
		t.logger.Printf("oauth: refresh aborted: %v", err)
		return err
	}
	if !proceed {
		t.logger.Printf("oauth: refresh skipped — token already rotated past stale bearer %s by another request",
			redactToken(staleBearer))
		return nil
	}
	t.logger.Printf("oauth: POST %s grant=refresh_token client=%s refresh=%s", tokenURL, clientID, redactToken(rt))

	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": rt,
		"client_id":     clientID,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, bytes.NewReader(body))
	if err != nil {
		t.logger.Printf("oauth: build request: %v", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := t.httpc.Do(req)
	if err != nil {
		t.logger.Printf("oauth: request failed after %s: %v", time.Since(start).Round(time.Millisecond), err)
		return fmt.Errorf("oauth refresh: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	t.logger.Printf("oauth: response status=%d bytes=%d elapsed=%s", resp.StatusCode, len(respBody), time.Since(start).Round(time.Millisecond))
	if resp.StatusCode != 200 {
		// The endpoint's error body explains the failure (expired/revoked
		// refresh token, bad client_id, etc.) — log it verbatim.
		t.logger.Printf("oauth: refresh rejected: status %d body=%s", resp.StatusCode, truncate(string(respBody), 512))
		return fmt.Errorf("oauth refresh: status %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		t.logger.Printf("oauth: parse response: %v body=%s", err, truncate(string(respBody), 512))
		return fmt.Errorf("oauth refresh: parse: %w", err)
	}
	if parsed.AccessToken == "" {
		t.logger.Printf("oauth: response had no access_token; body=%s", truncate(string(respBody), 512))
		return fmt.Errorf("oauth refresh: empty access_token in response")
	}

	access, refresh, path, expiresAtMs := t.commit(parsed.AccessToken, parsed.RefreshToken, parsed.ExpiresIn)
	t.logger.Printf("oauth: refreshed access=%s refresh_rotated=%v expires_in=%ds",
		redactToken(access), parsed.RefreshToken != "" && parsed.RefreshToken != rt, parsed.ExpiresIn)
	if path != "" {
		if err := writeCredentialsFile(path, access, refresh, expiresAtMs); err != nil {
			// Non-fatal: in-memory copy is updated; the next runclaude
			// invocation will just re-refresh from the old file.
			t.logger.Printf("oauth: write credentials %s failed: %v", path, err)
			return fmt.Errorf("oauth refresh: write %s: %w", path, err)
		}
		t.logger.Printf("oauth: wrote refreshed credentials to %s", path)
	}
	return nil
}

// truncate caps s at n bytes for logging, marking elision.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// snapshot returns the inputs needed for a refresh call, or (proceed=false)
// when another goroutine has already rotated past staleBearer.
func (t *tokenSource) snapshot(staleBearer string) (rt, clientID, tokenURL string, proceed bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.refreshToken == "" {
		return "", "", "", false, fmt.Errorf("no refresh token available")
	}
	if staleBearer != "" && staleBearer != t.accessToken {
		return "", "", "", false, nil
	}
	return t.refreshToken, t.clientID, t.tokenURL, true, nil
}

// commit installs the refreshed tokens and returns the values the caller
// needs to persist to disk.
func (t *tokenSource) commit(newAccess, newRefresh string, expiresIn int64) (access, refresh, path string, expiresAtMs int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.accessToken = newAccess
	if newRefresh != "" {
		t.refreshToken = newRefresh
	}
	t.lastRefresh = time.Now()
	if expiresIn > 0 {
		expiresAtMs = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
	}
	return t.accessToken, t.refreshToken, t.path, expiresAtMs
}

// writeCredentialsFile atomically rewrites ~/.claude/.credentials.json with
// the new token pair while preserving every other field the CLI may have
// written (scopes, subscriptionType, …).
func writeCredentialsFile(path, access, refresh string, expiresAtMs int64) error {
	var top map[string]any
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &top)
	}
	if top == nil {
		top = map[string]any{}
	}
	oauth, _ := top["claudeAiOauth"].(map[string]any)
	if oauth == nil {
		oauth = map[string]any{}
	}
	oauth["accessToken"] = access
	if refresh != "" {
		oauth["refreshToken"] = refresh
	}
	if expiresAtMs > 0 {
		oauth["expiresAt"] = expiresAtMs
	}
	top["claudeAiOauth"] = oauth

	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials.json.tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

// refreshTransport wraps a RoundTripper so that a single 401 from upstream
// triggers an OAuth refresh and a one-shot replay of the request with the
// freshly-injected bearer.
type refreshTransport struct {
	base     http.RoundTripper
	tokens   *tokenSource
	reinject func(*http.Request) // re-runs the proxy's inject(host, r)
	logger   *log.Logger
}

func (t *refreshTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the body once so we can replay it. Anthropic request bodies
	// are small JSON; the cost is negligible.
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
		body = b
		req.Body = io.NopCloser(bytes.NewReader(body))
	}

	stale := t.tokens.Bearer()
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		t.logger.Printf("anthropic %s %s upstream error: %v", req.Method, req.URL.Path, err)
		return resp, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	// Read the 401 body so we can log *why* upstream rejected us (expired
	// token, invalid client, scope error, …); then close so the connection
	// can be reused for the retry.
	failBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	t.logger.Printf("anthropic 401 on %s %s with bearer %s — attempting OAuth refresh; upstream body=%s",
		req.Method, req.URL.Path, redactToken(stale), truncate(string(failBody), 512))
	if err := t.tokens.Refresh(req.Context(), stale); err != nil {
		t.logger.Printf("refresh failed: %v", err)
		// Synthesize a 401 response so the client sees the original
		// failure rather than a connection error.
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 Unauthorized",
			Proto:      "HTTP/1.1",
			ProtoMajor: 1, ProtoMinor: 1,
			Header:  http.Header{"Content-Type": []string{"application/json"}},
			Body:    io.NopCloser(bytes.NewReader([]byte(`{"error":"runclaude: token refresh failed"}`))),
			Request: req,
		}, nil
	}

	req2 := req.Clone(req.Context())
	if body != nil {
		req2.Body = io.NopCloser(bytes.NewReader(body))
		req2.ContentLength = int64(len(body))
	}
	t.reinject(req2)
	t.logger.Printf("retrying %s %s with refreshed token %s", req2.Method, req2.URL.Path, redactToken(t.tokens.Bearer()))
	resp2, err := t.base.RoundTrip(req2)
	switch {
	case err != nil:
		t.logger.Printf("retry transport error: %v", err)
	case resp2.StatusCode == http.StatusUnauthorized:
		t.logger.Printf("retry still 401 after refresh — refreshed token rejected by upstream")
	default:
		t.logger.Printf("retry succeeded: status=%d", resp2.StatusCode)
	}
	return resp2, err
}
