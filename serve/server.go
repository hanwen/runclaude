// Package serve is the HTTP surface for a shared runclaude session: it mounts
// the embedded frontend and streams the live transcript to viewers over
// Server-Sent Events. (Prompt submission and take-control land in Phase 3; the
// tsnet listener and WhoIs identity wrap this handler in Phase 2b.)
//
// Why SSE rather than WebSocket: the transcript is a server→client push, and
// the only client→server actions (prompt, take-control, interrupt) are discrete
// requests better modelled as plain POSTs. SSE needs no extra dependency, which
// matches runclaude's single-binary, zero-runtime-dep design.
package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/hanwen/runclaude/frontend"
	"github.com/hanwen/runclaude/sessionhub"
)

// Config wires a Server to its session. Hub, Identity and Policy are required;
// Source is optional read-only context (jj show @) for the source panel.
type Config struct {
	Hub      *sessionhub.Hub
	Identity Identifier
	Policy   Policy
	// Source, if set, returns a read-only source-checkout view run in the
	// session cwd: view selects which (e.g. "show", "diff", "log", "files"),
	// path names a file for the "file" view. Output may contain ANSI color
	// codes (the frontend renders them). nil disables the source panel.
	Source func(ctx context.Context, view, path string) (string, error)
}

// Server routes HTTP requests for one session.
type Server struct {
	hub    *sessionhub.Hub
	ident  Identifier
	policy Policy
	source func(ctx context.Context, view, path string) (string, error)
	mux    *http.ServeMux
}

// New builds the HTTP handler for the session described by cfg.
func New(cfg Config) *Server {
	s := &Server{
		hub:    cfg.Hub,
		ident:  cfg.Identity,
		policy: cfg.Policy,
		source: cfg.Source,
		mux:    http.NewServeMux(),
	}
	s.mux.Handle("/", http.FileServer(http.FS(frontend.FS)))
	s.mux.HandleFunc("/events", s.handleEvents)
	s.mux.HandleFunc("/whoami", s.handleWhoami)
	s.mux.HandleFunc("/take-control", s.handleTakeControl)
	s.mux.HandleFunc("/release-control", s.handleReleaseControl)
	s.mux.HandleFunc("/prompt", s.handlePrompt)
	s.mux.HandleFunc("/interrupt", s.handleInterrupt)
	s.mux.HandleFunc("/source", s.handleSource)
	return s
}

// handleSource returns the current read-only source-checkout view as plain
// text. The frontend polls it. 404 when no source provider is configured (not
// a jj repo); 503 if the command fails transiently.
func (s *Server) handleSource(w http.ResponseWriter, r *http.Request) {
	if s.source == nil {
		http.Error(w, "no source view", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	out, err := s.source(r.Context(), q.Get("view"), q.Get("path"))
	if err != nil {
		http.Error(w, "source unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, out)
}

// handleWhoami reports the caller's resolved identity and whether they are
// write-eligible, so the frontend knows whether to offer the take-control
// button.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	id := s.ident.Identify(r)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Identity
		MayWrite bool `json:"mayWrite"`
	}{id, s.policy.MayWrite(id)})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleEvents streams the transcript as SSE. Each event carries an id equal to
// its index in the canonical transcript; on reconnect the browser sends
// Last-Event-ID and we resume after it, so a dropped/reconnected viewer never
// re-renders events it already has.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	last := lastEventID(r)

	sub := s.hub.Subscribe()
	defer sub.Close()

	// Replay history the client hasn't seen yet.
	for i, raw := range sub.History {
		if i <= last {
			continue
		}
		writeEvent(w, i, raw)
	}
	// Mark the history/live boundary so the client can tell replay from live
	// events (e.g. to suppress "you lost control" notices while catching up).
	fmt.Fprint(w, "data: {\"type\":\"synced\"}\n\n")
	flusher.Flush()

	// Live events get ids continuing past the history snapshot.
	id := len(sub.History)
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case raw, ok := <-sub.C:
			if !ok {
				// Session ended: tell the client so it stops reconnecting.
				fmt.Fprint(w, "event: end\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			writeEvent(w, id, raw)
			id++
			flusher.Flush()
		}
	}
}

// writeEvent emits one SSE message. raw is a single-line JSON event, so it is
// safe to inline after "data: " without multi-line escaping.
func writeEvent(w http.ResponseWriter, id int, raw []byte) {
	fmt.Fprintf(w, "id: %d\ndata: ", id)
	w.Write(raw)
	fmt.Fprint(w, "\n\n")
}

// lastEventID returns the highest event id the client has already received, or
// -1 if this is a fresh connection.
func lastEventID(r *http.Request) int {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		return -1
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
	}
	return n
}
