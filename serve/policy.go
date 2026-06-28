package serve

// Policy decides write-eligibility: may this identity ever take control and
// submit prompts? It is separate from the runtime control token (who holds it
// right now) — eligibility is the security gate, the token is coordination.
//
// v1 is a local allowlist (owner-controlled, ad hoc, per-session). The
// interface keeps the hub and handlers independent of where eligibility is
// encoded, so a tailnet-ACL-grant policy can replace it later without touching
// control logic.
type Policy interface {
	MayWrite(Identity) bool
}

// AllowlistPolicy permits writing by the local operator, by any login in the
// allowlist, or — only when AllowAll is set (dev mode) — by anyone. A malicious
// writer drives an agent with sandbox RCE, so the default is deny: viewers stay
// viewers unless explicitly promoted.
type AllowlistPolicy struct {
	Logins     map[string]bool // explicitly allowed Tailscale logins
	AllowLocal bool            // the local operator (Login == "local")
	AllowAll   bool            // dev only: every identity is write-eligible
}

func (p AllowlistPolicy) MayWrite(id Identity) bool {
	if p.AllowAll {
		return true
	}
	if p.AllowLocal && id.Login == "local" {
		return true
	}
	return p.Logins[id.Login]
}
