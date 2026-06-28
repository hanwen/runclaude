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
	srv := httptest.NewServer(New(hub, DevIdentifier{Base: Identity{Login: "local", Name: "operator"}}))
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

func TestEventsReplayThenLive(t *testing.T) {
	pr, pw := io.Pipe()
	hub := sessionhub.New(agentclient.New(&bytes.Buffer{}, pr))
	go hub.Run()

	// One event before anyone connects -> must arrive as replayed history.
	io.WriteString(pw, `{"type":"system","subtype":"init","session_id":"s1"}`+"\n")
	time.Sleep(30 * time.Millisecond)

	srv := httptest.NewServer(New(hub, LocalIdentifier{ID: Identity{Login: "local"}}))
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
