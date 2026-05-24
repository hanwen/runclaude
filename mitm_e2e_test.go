package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	fake "github.com/hanwen/runclaude/fakeapi"
)

func testNow() time.Time {
	return time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
}

// TestMitmE2EAnthropic exercises the Anthropic-path MITM end-to-end:
// client -> reverse-proxy-with-inject -> fake anthropic server. The
// inject swaps a stub api-key for the real one, the fake server checks
// the api-key, and we assert the fixed reply comes back.
func TestMitmE2EAnthropic(t *testing.T) {
	const realKey = "sk-ant-real-key"
	const stubKey = "sk-ant-stub"
	upstream := httptest.NewServer(fake.Handler(fake.Config{APIKey: realKey}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)

	logger := log.New(io.Discard, "", 0)
	inject := func(host string, r *http.Request) {
		// Mirror main.go's injection for api.anthropic.com.
		r.Header.Del("Authorization")
		r.Header.Del("x-api-key")
		r.Header.Set("x-api-key", realKey)
		if r.Header.Get("anthropic-version") == "" {
			r.Header.Set("anthropic-version", "2023-06-01")
		}
	}
	rp := newMitmReverseProxy("api.anthropic.com", upstreamURL, inject, nil, logger)
	mitm := httptest.NewServer(rp)
	defer mitm.Close()

	// Client sends with the stub key (mimicking what claude code does
	// when no real creds are available). The inject layer must replace
	// it with the real key before the fake server sees the request.
	body := []byte(`{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, mitm.URL+"/v1/messages", bytes.NewReader(body))
	req.Host = "api.anthropic.com"
	req.Header.Set("x-api-key", stubKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), fake.FixedReply) {
		t.Errorf("response missing %q:\n%s", fake.FixedReply, out)
	}
}

// TestMitmE2EBedrock exercises the bedrock-path MITM end-to-end: a
// stub-signed request enters the proxy, injectBedrock re-signs with the
// real AWS credentials, the fake server verifies the signature against
// those credentials, and we assert the fixed reply.
func TestMitmE2EBedrock(t *testing.T) {
	realCreds := aws.Credentials{
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	credsProvider := aws.CredentialsProviderFunc(func(_ context.Context) (aws.Credentials, error) {
		return realCreds, nil
	})

	upstream := httptest.NewServer(fake.Handler(fake.Config{
		AWSAccessKey: realCreds.AccessKeyID,
		AWSSecretKey: realCreds.SecretAccessKey,
		AWSRegion:    "us-east-1",
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)

	host := "bedrock-runtime.us-east-1.amazonaws.com"
	logger := log.New(io.Discard, "", 0)
	signer := v4.NewSigner()
	inject := func(h string, r *http.Request) {
		injectBedrock(h, r, credsProvider, signer, logger)
	}
	rp := newMitmReverseProxy(host, upstreamURL, inject, nil, logger)
	mitm := httptest.NewServer(rp)
	defer mitm.Close()

	// Client signs with stub creds. The proxy's injectBedrock strips
	// the stub signature and re-signs with real creds before forwarding.
	stubCreds := aws.Credentials{
		AccessKeyID:     "AKIA000RUNCLAUDESTUB",
		SecretAccessKey: "runclaude-stub-secret-do-not-use",
	}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"anthropic_version":"bedrock-2023-05-31"}`)
	req, _ := http.NewRequest(http.MethodPost,
		mitm.URL+"/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke",
		bytes.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))

	signReq := req.Clone(context.Background())
	signReq.URL = &url.URL{Scheme: "https", Host: host, Path: req.URL.Path}
	signReq.Body = io.NopCloser(bytes.NewReader(body))
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	if err := signer.SignHTTP(context.Background(), stubCreds, signReq,
		hash, "bedrock", "us-east-1", testNow()); err != nil {
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
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	var parsed map[string]any
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	got := parsed["content"].([]any)[0].(map[string]any)["text"]
	if got != fake.FixedReply {
		t.Errorf("text = %q, want %q", got, fake.FixedReply)
	}
}
