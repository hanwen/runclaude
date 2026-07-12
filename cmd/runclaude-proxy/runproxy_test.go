package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fake "github.com/hanwen/runclaude/fakeapi"
)

// TestRunproxyE2E builds the runproxy binary, points it at a fake Anthropic
// server, then drives a request through it as a real HTTPS_PROXY client. This
// exercises the same path a Docker container would take: stub creds in,
// real creds injected, fake server validates.
func TestRunproxyE2E(t *testing.T) {
	const realKey = "sk-ant-real-runproxy"
	upstream := httptest.NewServer(fake.Handler(fake.Config{APIKey: realKey}))
	defer upstream.Close()

	dir := t.TempDir()
	credHome := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(credHome, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	credsJSON := `{"apiKey":"` + realKey + `"}`
	if err := os.WriteFile(filepath.Join(credHome, ".claude", ".credentials.json"), []byte(credsJSON), 0600); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "runproxy")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}

	caCert := filepath.Join(dir, "ca.crt")
	cmd := exec.Command(bin,
		"--listen", "127.0.0.1:0", // not honored — see workaround below
		"--ca-dir", filepath.Join(dir, "ca"),
		"--export-ca", caCert,
		"--anthropic",
		"--anthropic-home", credHome,
		"--test.mitm-upstream", "api.anthropic.com="+upstream.URL,
		"--log", filepath.Join(dir, "proxy.log"),
	)
	// Pick a free port ourselves so we know where to dial.
	port := freePort(t)
	cmd.Args[2] = "127.0.0.1:" + port
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start runproxy: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			t.Logf("runproxy stderr:\n%s", stderr.String())
		}
	}()

	if err := waitListening("127.0.0.1:"+port, 5*time.Second); err != nil {
		t.Fatalf("runproxy did not start listening: %v (stderr: %s)", err, stderr.String())
	}
	if err := waitFile(caCert, 5*time.Second); err != nil {
		t.Fatalf("CA cert not exported: %v", err)
	}

	caPEM, err := os.ReadFile(caCert)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("AppendCertsFromPEM failed")
	}

	proxyURL, _ := url.Parse("http://127.0.0.1:" + port)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}

	body := []byte(`{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	req.Header.Set("x-api-key", "sk-ant-stub-from-container")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v (stderr: %s)", err, stderr.String())
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

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	return port
}

func waitListening(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &timeoutError{addr}
}

func waitFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &timeoutError{path}
}

type timeoutError struct{ what string }

func (e *timeoutError) Error() string { return "timeout waiting for " + e.what }
