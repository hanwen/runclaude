package fakeapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const (
	testAPIKey       = "sk-ant-fake-test-key"
	testAccessKey    = "AKIAIOSFODNN7EXAMPLE"
	testSecretKey    = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	testSessionToken = "FAKE-SESSION-TOKEN"
	testRegion       = "us-east-1"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(Handler(Config{
		APIKey:          testAPIKey,
		AWSAccessKey:    testAccessKey,
		AWSSecretKey:    testSecretKey,
		AWSSessionToken: testSessionToken,
		AWSRegion:       testRegion,
	}))
}

func TestAnthropicNonStream(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", testAPIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	text := parsed["content"].([]any)[0].(map[string]any)["text"]
	if text != FixedReply {
		t.Errorf("text = %q, want %q", text, FixedReply)
	}
}

func TestAnthropicStreamingExtractsText(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), FixedReply) {
		t.Errorf("stream missing %q:\n%s", FixedReply, body)
	}
}

func TestAnthropicRejectsBadKey(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestBedrockVerifiesSignature(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"anthropic_version":"bedrock-2023-05-31"}`)
	srvURL, _ := url.Parse(srv.URL)
	req, _ := http.NewRequest(http.MethodPost,
		srvURL.String()+"/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke",
		bytes.NewReader(body))
	// Sign for the bedrock host even though we hit srvURL — the fake
	// server verifies against r.Host (which it gets from this header).
	req.Host = "bedrock-runtime." + testRegion + ".amazonaws.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.ContentLength = int64(len(body))

	signReq := req.Clone(context.Background())
	signReq.URL = &url.URL{Scheme: "https", Host: req.Host, Path: req.URL.Path}
	signReq.Body = io.NopCloser(bytes.NewReader(body))
	signer := v4.NewSigner()
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	creds := aws.Credentials{
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
		SessionToken:    testSessionToken,
	}
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	if err := signer.SignHTTP(context.Background(), creds, signReq, hash, "bedrock", testRegion, now); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"Authorization", "X-Amz-Date", "X-Amz-Security-Token"} {
		if v := signReq.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	text := parsed["content"].([]any)[0].(map[string]any)["text"]
	if text != FixedReply {
		t.Errorf("text = %q, want %q", text, FixedReply)
	}
}

func TestBedrockRejectsBadSignature(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	body := []byte(`{}`)
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/model/anthropic.claude/invoke", bytes.NewReader(body))
	req.Host = "bedrock-runtime." + testRegion + ".amazonaws.com"
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIA/20260101/us-east-1/bedrock/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef")
	req.Header.Set("X-Amz-Date", "20260101T000000Z")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}
