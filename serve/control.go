package serve

import (
	"encoding/json"
	"net/http"

	"github.com/hanwen/runclaude/sessionhub"
)

// participantReq is the JSON body shared by the write endpoints. id is the
// per-connection participant id the browser generates (sessionStorage); it is
// coordination/attribution only. The security gate is the resolved Identity
// (WhoIs/dev), never this id or name.
type participantReq struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Text string `json:"text"`
}

func decodeParticipant(w http.ResponseWriter, r *http.Request) (participantReq, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return participantReq{}, false
	}
	var req participantReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return participantReq{}, false
	}
	if req.ID == "" {
		http.Error(w, "missing participant id", http.StatusBadRequest)
		return participantReq{}, false
	}
	return req, true
}

// handleTakeControl steals the single writer token for the caller, if their
// identity is write-eligible. The steal is unconditional among eligible
// writers — any of them can take control at any time (the displaced controller
// drops to viewer via the broadcast control event).
func (s *Server) handleTakeControl(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeParticipant(w, r)
	if !ok {
		return
	}
	id := s.ident.Identify(r)
	if !s.policy.MayWrite(id) {
		http.Error(w, "not write-eligible", http.StatusForbidden)
		return
	}
	name := req.Name
	if name == "" {
		name = id.Name
	}
	s.hub.TakeControl(req.ID, name, id.Login)
	w.WriteHeader(http.StatusNoContent)
}

// handleReleaseControl drops the token if the caller holds it.
func (s *Server) handleReleaseControl(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeParticipant(w, r)
	if !ok {
		return
	}
	s.hub.ReleaseControl(req.ID)
	w.WriteHeader(http.StatusNoContent)
}

// handlePrompt submits a prompt turn. The hub enforces that the caller is the
// current controller; a stale/non-controller caller gets 409 so the UI can
// refresh its control state.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeParticipant(w, r)
	if !ok {
		return
	}
	if req.Text == "" {
		http.Error(w, "empty prompt", http.StatusBadRequest)
		return
	}
	id := s.ident.Identify(r)
	// Defence in depth: a controller token held by an ineligible identity should
	// never submit (e.g. eligibility revoked mid-session).
	if !s.policy.MayWrite(id) {
		http.Error(w, "not write-eligible", http.StatusForbidden)
		return
	}
	switch err := s.hub.Submit(req.ID, req.Name, id.Login, req.Text); err {
	case nil:
		w.WriteHeader(http.StatusNoContent)
	case sessionhub.ErrNotController:
		http.Error(w, "not the controller", http.StatusConflict)
	default:
		http.Error(w, "send failed", http.StatusInternalServerError)
	}
}

// handleInterrupt stops the running turn on behalf of the controller.
func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeParticipant(w, r)
	if !ok {
		return
	}
	switch err := s.hub.Interrupt(req.ID); err {
	case nil:
		w.WriteHeader(http.StatusNoContent)
	case sessionhub.ErrNotController:
		http.Error(w, "not the controller", http.StatusConflict)
	default:
		http.Error(w, "interrupt failed", http.StatusInternalServerError)
	}
}
