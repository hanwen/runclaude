package serve

import "net/http"

// Identity is who is on the other end of a connection. Login is the stable
// authorization key — in a tailnet deployment it is the Tailscale login
// (e.g. alice@example.com) from tsnet WhoIs; for a local/dev server it is a
// fixed or query-supplied label. Name is for display only.
//
// This is the *authorization* layer. The per-connection participant label used
// for take-control attribution (Phase 3) is deliberately separate: a cosmetic
// name a viewer picks, never the authz gate.
type Identity struct {
	Login string `json:"login"`
	Name  string `json:"name"`
}

// Identifier resolves the Identity for an HTTP request. Keeping the hub and
// handlers behind this interface means the transport (tailnet WhoIs vs. local)
// is swappable without touching session logic — the tsnet implementation slots
// in here later.
type Identifier interface {
	Identify(r *http.Request) Identity
}

// LocalIdentifier reports a single fixed identity for every request. Used when
// the web UI is bound to localhost (no tailnet, no per-connection identity):
// everyone is the local operator.
type LocalIdentifier struct {
	ID Identity
}

func (l LocalIdentifier) Identify(*http.Request) Identity { return l.ID }

// DevIdentifier fakes distinct principals from a single machine for testing the
// identity and control flow without a second Tailscale account: it reads the
// login from the ?as= query parameter (falling back to Base). It is wired up
// only under the explicit --serve-dev flag and must never be used in a real
// tailnet deployment, where identity has to come from WhoIs — otherwise anyone
// could name themselves anything.
type DevIdentifier struct {
	Base Identity
}

func (d DevIdentifier) Identify(r *http.Request) Identity {
	if as := r.URL.Query().Get("as"); as != "" {
		return Identity{Login: as, Name: as}
	}
	return d.Base
}
