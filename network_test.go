package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// TestMitmNoHeaderDrift verifies that, when the MITM proxy holds the same
// AWS credentials the client used to sign its request, the request the
// upstream sees is identical (headers, body, Host) to the request that
// arrived at the proxy. This catches drift introduced anywhere in the
// proxy pipeline: the Director, httputil.ReverseProxy itself, or Go's
// http.Transport. We run a real two-hop HTTP pipeline (client -> mitm
// server -> upstream server) and compare what the proxy received against
// what the upstream received, which isolates proxy-introduced drift from
// any headers Go's http.Client adds before the request ever reaches the
// proxy.
func TestMitmNoHeaderDrift(t *testing.T) {
	host := "bedrock-runtime.us-east-1.amazonaws.com"
	creds := aws.CredentialsProviderFunc(func(_ context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
			SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			SessionToken:    "FAKE-SESSION-TOKEN",
		}, nil
	})
	signer := v4.NewSigner()
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	logger := log.New(io.Discard, "", 0)

	var upstreamReq *http.Request
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamBody = b
		clone := r.Clone(context.Background())
		clone.Body = nil
		upstreamReq = clone
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	inject := func(h string, r *http.Request) {
		injectBedrockAt(h, r, creds, signer, now, logger)
	}
	rp := newMitmReverseProxy(host, upstreamURL, inject, logger)

	var clientReq *http.Request
	var clientBody []byte
	mitm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		clientBody = b
		r.Body = io.NopCloser(bytes.NewReader(b))
		clone := r.Clone(context.Background())
		clone.Body = nil
		clientReq = clone
		rp.ServeHTTP(w, r)
	}))
	defer mitm.Close()

	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"anthropic_version":"bedrock-2023-05-31"}`)
	req, err := http.NewRequest(http.MethodPost,
		mitm.URL+"/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	// Make the client send (and sign) for the bedrock host even though
	// the TCP connection goes to the local mitm server.
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "bedrock-2023-05-31")
	// Real bedrock SDKs put Accept-Encoding on the wire before signing.
	// Set it explicitly so Go's transport won't append it after we sign
	// (it only adds Accept-Encoding when absent), keeping the signed
	// header set in sync with what actually goes over the wire.
	req.Header.Set("Accept-Encoding", "gzip")
	req.ContentLength = int64(len(body))

	// Build a request struct that mirrors what we'll send, but with
	// URL.Host = bedrock so the signature canonicalizes correctly.
	signReq := req.Clone(context.Background())
	signReq.URL = &url.URL{Scheme: "https", Host: host, Path: req.URL.Path}
	signReq.Body = io.NopCloser(bytes.NewReader(body))
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	c, err := creds.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.SignHTTP(context.Background(), c, signReq, hash, "bedrock", "us-east-1", now); err != nil {
		t.Fatal(err)
	}
	// Copy the signed headers back onto the outgoing request.
	for _, h := range []string{"Authorization", "X-Amz-Date", "X-Amz-Security-Token", "X-Amz-Content-Sha256"} {
		if v := signReq.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()

	if clientReq == nil || upstreamReq == nil {
		t.Fatal("did not capture both sides of the proxy")
	}
	if clientReq.Host != upstreamReq.Host {
		t.Errorf("Host changed by proxy:\nclient:   %q\nupstream: %q", clientReq.Host, upstreamReq.Host)
	}
	if clientReq.Method != upstreamReq.Method {
		t.Errorf("Method changed: %q -> %q", clientReq.Method, upstreamReq.Method)
	}
	if clientReq.URL.Path != upstreamReq.URL.Path {
		t.Errorf("Path changed: %q -> %q", clientReq.URL.Path, upstreamReq.URL.Path)
	}
	if !bytes.Equal(clientBody, upstreamBody) {
		t.Errorf("body changed:\nclient:   %q\nupstream: %q", clientBody, upstreamBody)
	}
	if !reflect.DeepEqual(clientReq.Header, upstreamReq.Header) {
		t.Errorf("headers changed by proxy.\nclient headers:   %v\nupstream headers: %v", clientReq.Header, upstreamReq.Header)
	}
}

// TestMitmHopByHopSignedByClient verifies that if a client signs a hop-by-hop
// header (e.g. Connection), the upstream still receives a valid SigV4
// signature. The proxy must strip hop-by-hop headers *before* re-signing,
// otherwise its SignedHeaders list would name headers that
// httputil.ReverseProxy removes downstream — and Bedrock would reject the
// signature.
func TestMitmHopByHopSignedByClient(t *testing.T) {
	host := "bedrock-runtime.us-east-1.amazonaws.com"
	creds := aws.CredentialsProviderFunc(func(_ context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
			SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		}, nil
	})
	signer := v4.NewSigner()
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	logger := log.New(io.Discard, "", 0)

	var upstreamReq *http.Request
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamBody = b
		clone := r.Clone(context.Background())
		clone.Body = nil
		upstreamReq = clone
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	rp := newMitmReverseProxy(host, upstreamURL, func(h string, r *http.Request) {
		injectBedrockAt(h, r, creds, signer, now, logger)
	}, logger)
	mitm := httptest.NewServer(rp)
	defer mitm.Close()

	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequest(http.MethodPost,
		mitm.URL+"/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "bedrock-2023-05-31")
	req.Header.Set("Accept-Encoding", "gzip")
	// The interesting bit: a hop-by-hop header the (hypothetical) client
	// chose to include in its signature.
	req.Header.Set("Connection", "keep-alive")
	req.ContentLength = int64(len(body))

	signReq := req.Clone(context.Background())
	signReq.URL = &url.URL{Scheme: "https", Host: host, Path: req.URL.Path}
	signReq.Body = io.NopCloser(bytes.NewReader(body))
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	c, err := creds.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.SignHTTP(context.Background(), c, signReq, hash, "bedrock", "us-east-1", now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(signReq.Header.Get("Authorization"), "connection") {
		t.Fatalf("test setup: expected Connection in client SignedHeaders, got %q", signReq.Header.Get("Authorization"))
	}
	for _, h := range []string{"Authorization", "X-Amz-Date", "X-Amz-Content-Sha256"} {
		if v := signReq.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()

	if upstreamReq == nil {
		t.Fatal("upstream did not receive a request")
	}
	if got := upstreamReq.Header.Get("Connection"); got != "" {
		t.Errorf("upstream should not see Connection header, got %q", got)
	}
	if strings.Contains(upstreamReq.Header.Get("Authorization"), "connection") {
		t.Errorf("upstream Authorization still claims to sign 'connection': %q",
			upstreamReq.Header.Get("Authorization"))
	}

	// Recompute SigV4 over exactly what upstream received and confirm
	// it matches the Authorization the proxy produced. This is the
	// check Bedrock would perform.
	upSum := sha256.Sum256(upstreamBody)
	upHash := hex.EncodeToString(upSum[:])
	verify := upstreamReq.Clone(context.Background())
	verify.URL = &url.URL{Scheme: "https", Host: host, Path: upstreamReq.URL.Path}
	verify.Host = host
	verify.Body = io.NopCloser(bytes.NewReader(upstreamBody))
	verify.ContentLength = int64(len(upstreamBody))
	gotAuth := verify.Header.Get("Authorization")
	verify.Header.Del("Authorization")
	verify.Header.Del("X-Amz-Date")
	verify.Header.Del("X-Amz-Content-Sha256")
	if err := signer.SignHTTP(context.Background(), c, verify, upHash, "bedrock", "us-east-1", now); err != nil {
		t.Fatalf("verify sign: %v", err)
	}
	if verify.Header.Get("Authorization") != gotAuth {
		t.Errorf("upstream signature does not validate.\nexpected: %s\ngot:      %s",
			verify.Header.Get("Authorization"), gotAuth)
	}
}
