package serve

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hanwen/runclaude/agentclient"
	"github.com/hanwen/runclaude/sessionhub"
)

func TestWhoamiDevOverride(t *testing.T) {
	hub := sessionhub.New(agentclient.New(&bytes.Buffer{}, strings.NewReader("")))
	srv := httptest.NewServer(New(Config{Hub: hub, Identity: DevIdentifier{Base: Identity{Login: "local", Name: "operator"}}, Policy: AllowlistPolicy{AllowAll: true}}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/whoami?as=alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "alice@example.com") {
		t.Errorf("/whoami?as=alice = %s, want the override login", body)
	}
}

func TestSourcePanel(t *testing.T) {
	hub := sessionhub.New(agentclient.New(&bytes.Buffer{}, strings.NewReader("")))
	ident := LocalIdentifier{ID: Identity{Login: "local"}}

	// No provider -> 404.
	noSrc := httptest.NewServer(New(Config{Hub: hub, Identity: ident, Policy: AllowlistPolicy{}}))
	defer noSrc.Close()
	if res, _ := http.Get(noSrc.URL + "/source"); res == nil || res.StatusCode != http.StatusNotFound {
		t.Errorf("/source with no provider: got %v, want 404", res)
	}

	// Provider -> its text.
	withSrc := httptest.NewServer(New(Config{
		Hub: hub, Identity: ident, Policy: AllowlistPolicy{},
		Source: func(_ context.Context, view, path string) (string, error) {
			return "jj " + view + " output", nil
		},
	}))
	defer withSrc.Close()
	res, err := http.Get(withSrc.URL + "/source?view=diff")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if string(body) != "jj diff output" {
		t.Errorf("/source body = %q (view should reach the provider)", body)
	}
}

func TestEventsReplayThenLive(t *testing.T) {
	pr, pw := io.Pipe()
	hub := sessionhub.New(agentclient.New(&bytes.Buffer{}, pr))
	go hub.Run()

	// One event before anyone connects -> must arrive as replayed history.
	io.WriteString(pw, `{"type":"system","subtype":"init","session_id":"s1"}`+"\n")
	time.Sleep(30 * time.Millisecond)

	srv := httptest.NewServer(New(Config{Hub: hub, Identity: LocalIdentifier{ID: Identity{Login: "local"}}, Policy: AllowlistPolicy{AllowLocal: true}}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}

	// Emit a live event after connecting.
	io.WriteString(pw, `{"type":"assistant","session_id":"s1"}`+"\n")

	// Collect lines until we've seen the history event (id 0) and the live one (id 1).
	br := bufio.NewReader(res.Body)
	var sawInit, sawAssistant bool
	deadline := time.After(2 * time.Second)
	lines := make(chan string)
	go func() {
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if err != nil {
				return
			}
		}
	}()
	for !(sawInit && sawAssistant) {
		select {
		case line := <-lines:
			if strings.Contains(line, `"system"`) && strings.Contains(line, `"s1"`) {
				sawInit = true
			}
			if strings.Contains(line, `"assistant"`) {
				sawAssistant = true
			}
		case <-deadline:
			t.Fatalf("timed out; sawInit=%v sawAssistant=%v", sawInit, sawAssistant)
		}
	}
	pw.Close()
}
