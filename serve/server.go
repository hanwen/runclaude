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
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hanwen/runclaude/frontend"
	"github.com/hanwen/runclaude/sessionhub"
)

// Server routes HTTP requests for one session.
type Server struct {
	hub   *sessionhub.Hub
	ident Identifier
	mux   *http.ServeMux
}

// New builds the HTTP handler for hub. ident resolves the caller identity for
// each request (LocalIdentifier on localhost, a tailnet WhoIs identifier later).
func New(hub *sessionhub.Hub, ident Identifier) *Server {
	s := &Server{hub: hub, ident: ident, mux: http.NewServeMux()}
	s.mux.Handle("/", http.FileServer(http.FS(frontend.FS)))
	s.mux.HandleFunc("/events", s.handleEvents)
	s.mux.HandleFunc("/whoami", s.handleWhoami)
	return s
}

// handleWhoami reports the caller's resolved identity so the frontend can show
// who you are. Identity is not yet a gate in Phase 2 (read-only for everyone);
// Phase 3 uses it to decide who may take control.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.ident.Identify(r))
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
