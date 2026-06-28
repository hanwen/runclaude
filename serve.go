package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hanwen/runclaude/agentclient"
	web "github.com/hanwen/runclaude/serve"
	"github.com/hanwen/runclaude/sessionhub"
)

// runServe is the host-side session: it owns the agentclient talking to the
// sandboxed claude over the stream-json pipes (stdinW carries prompt turns in,
// stdoutR carries events back), drives it through a sessionhub, and exposes the
// transcript to web viewers.
//
// Phase 2 serves read-only over plain localhost (addr); the terminal operator
// drives the session and browsers watch. Phase 2b swaps the listener for tsnet
// and adds per-connection identity; Phase 3 enables web prompt submission.
func runServe(stdinW, stdoutR *os.File, cwd, addr string, dev bool, writers []string) {
	client := agentclient.New(stdinW, stdoutR)
	hub := sessionhub.New(client)

	// Identity layer. On localhost there is no per-connection identity, so
	// everyone is the local operator; --serve-dev instead reads a ?as= label so
	// distinct principals can be simulated from one machine. The tailnet WhoIs
	// identifier will slot in here behind the same interface.
	operator := web.Identity{Login: "local", Name: "operator"}
	var ident web.Identifier = web.LocalIdentifier{ID: operator}
	if dev {
		ident = web.DevIdentifier{Base: operator}
	}

	// Write-eligibility policy: the local operator plus any explicitly allowed
	// logins. In dev mode with no allowlist, everyone is eligible so the
	// take-control flow can be exercised from one machine; pass --serve-writer
	// to instead test denials.
	logins := map[string]bool{}
	for _, l := range writers {
		logins[l] = true
	}
	policy := web.AllowlistPolicy{
		Logins:     logins,
		AllowLocal: true,
		AllowAll:   dev && len(writers) == 0,
	}

	// The local terminal operator drives the session; web viewers watch.
	go feedPromptsFromTerminal(hub, client)
	// Mirror the transcript to the host terminal so the local operator gets
	// feedback without a browser (web viewers get the same via the hub).
	go echoTranscript(hub)

	srv := &http.Server{Addr: addr, Handler: web.New(web.Config{
		Hub:      hub,
		Identity: ident,
		Policy:   policy,
		Source:   jjSource(cwd),
	})}
	go func() {
		log.Printf("serve: web UI on http://%s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("serve: http: %v", err)
		}
	}()

	hub.Run() // blocks until claude's event stream ends

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Printf("serve: session ended")
}

// echoTranscript prints assistant text and turn results from the hub to the
// host terminal, so the local operator has visibility without opening the web
// UI. It subscribes like any other viewer and ends when the session does.
func echoTranscript(hub *sessionhub.Hub) {
	sub := hub.Subscribe()
	defer sub.Close()
	for _, raw := range sub.History {
		echoEvent(raw)
	}
	for raw := range sub.C {
		echoEvent(raw)
	}
}

func echoEvent(raw []byte) {
	var hdr struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &hdr) != nil {
		return
	}
	ev := agentclient.Event{Type: hdr.Type, Raw: raw}
	switch hdr.Type {
	case "assistant":
		if t := ev.AssistantText(); t != "" {
			fmt.Println(t)
		}
	case "result":
		fmt.Printf("[result] %s\n", ev.ResultText())
	}
}

// jjSource returns a provider for the read-only source panel: it runs
// `jj show @` in the session cwd (the host sees the same working-copy tree the
// sandboxed agent edits, since cwd is bind-mounted in). Returns nil when cwd is
// not a jj repo, which disables the panel. Each call snapshots and shows the
// current working-copy change, so viewers see the agent's edits as they land.
func jjSource(cwd string) func(context.Context) (string, error) {
	if _, err := os.Stat(filepath.Join(cwd, ".jj")); err != nil {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "jj", "show", "@", "--color", "never")
		cmd.Dir = cwd
		out, err := cmd.Output()
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
}

// feedPromptsFromTerminal sends each non-blank line from the host terminal as a
// prompt turn; terminal EOF (^D) ends the session by closing claude's stdin.
func feedPromptsFromTerminal(hub *sessionhub.Hub, client *agentclient.Client) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if err := hub.SendPrompt(line); err != nil {
			log.Printf("serve: send prompt: %v", err)
			return
		}
	}
	client.CloseStdin()
}
