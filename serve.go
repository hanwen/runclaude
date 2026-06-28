package main

import (
	"bufio"
	"context"
	"log"
	"net/http"
	"os"
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
func runServe(stdinW, stdoutR *os.File, addr string, dev bool, writers []string) {
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

	srv := &http.Server{Addr: addr, Handler: web.New(hub, ident, policy)}
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
