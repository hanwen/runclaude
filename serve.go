package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
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

// serveOptions configures runServe.
type serveOptions struct {
	cwd      string   // session working directory (for the jj source panel)
	addr     string   // localhost bind address (when not on a tailnet)
	dev      bool     // dev ?as= identity override
	writers  []string // logins allowed to take control
	tailnet  string   // tsnet node hostname; empty = localhost
	tsnetDir string   // tsnet state dir (node identity persistence)
}

// runServe is the host-side session: it owns the agentclient talking to the
// sandboxed claude over the stream-json pipes (stdinW carries prompt turns in,
// stdoutR carries events back), drives it through a sessionhub, and exposes the
// transcript to web viewers — over a localhost listener or, when opt.tailnet is
// set, an embedded tsnet node with per-connection WhoIs identity.
func runServe(stdinW, stdoutR *os.File, opt serveOptions) {
	client := agentclient.New(stdinW, stdoutR)
	hub := sessionhub.New(client)

	// Identity + listener. On a tailnet, identity is the real WhoIs login and
	// the listener is the tsnet node. On localhost there is no per-connection
	// identity (everyone is the local operator); --serve-dev reads a ?as= label
	// so distinct principals can be simulated from one machine.
	operator := web.Identity{Login: "local", Name: "operator"}
	var ident web.Identifier = web.LocalIdentifier{ID: operator}
	if opt.dev {
		ident = web.DevIdentifier{Base: operator}
	}

	var ln net.Listener
	if opt.tailnet != "" {
		ctx := context.Background()
		l, tident, tn, err := web.ListenTailnet(ctx, opt.tailnet, opt.tsnetDir, os.Getenv("TS_AUTHKEY"), log.Printf)
		if err != nil {
			log.Printf("serve: tailnet: %v", err)
			client.CloseStdin()
			return
		}
		defer tn.Close()
		ln, ident = l, tident
		log.Printf("serve: tailnet node %q up; open http://%s/ from your tailnet", opt.tailnet, opt.tailnet)
	} else {
		l, err := net.Listen("tcp", opt.addr)
		if err != nil {
			log.Printf("serve: listen %s: %v", opt.addr, err)
			client.CloseStdin()
			return
		}
		ln = l
		log.Printf("serve: web UI on http://%s", opt.addr)
	}

	// Write-eligibility policy: any explicitly allowed login, plus — only on
	// localhost — the local operator. In dev mode with no allowlist, everyone is
	// eligible so the take-control flow can be exercised from one machine; pass
	// --serve-writer to test denials. On a tailnet AllowLocal never matches (the
	// login is a real WhoIs identity), so only --serve-writer logins may write.
	logins := map[string]bool{}
	for _, l := range opt.writers {
		logins[l] = true
	}
	policy := web.AllowlistPolicy{
		Logins:     logins,
		AllowLocal: opt.tailnet == "",
		AllowAll:   opt.dev && len(opt.writers) == 0,
	}

	// The local terminal operator drives the session; web viewers watch.
	go feedPromptsFromTerminal(hub, client)
	// Mirror the transcript to the host terminal so the local operator gets
	// feedback without a browser (web viewers get the same via the hub).
	go echoTranscript(hub)

	srv := &http.Server{Handler: web.New(web.Config{
		Hub:      hub,
		Identity: ident,
		Policy:   policy,
		Source:   jjSource(opt.cwd),
	})}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
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
// UI. It also logs the session id once it is known (the system/init event), so
// it can be copied to `--serve-resume` even without the web UI. It subscribes
// like any other viewer and ends when the session does.
func echoTranscript(hub *sessionhub.Hub) {
	sub := hub.Subscribe()
	defer sub.Close()
	var sessionLogged bool
	for _, raw := range sub.History {
		echoEvent(raw, &sessionLogged)
	}
	for raw := range sub.C {
		echoEvent(raw, &sessionLogged)
	}
}

func echoEvent(raw []byte, sessionLogged *bool) {
	var hdr struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(raw, &hdr) != nil {
		return
	}
	ev := agentclient.Event{Type: hdr.Type, Raw: raw}
	switch hdr.Type {
	case "system":
		if hdr.Subtype == "init" && hdr.SessionID != "" && !*sessionLogged {
			log.Printf("serve: session %s started (resume with --serve-resume %s)", hdr.SessionID, hdr.SessionID)
			*sessionLogged = true
		}
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
