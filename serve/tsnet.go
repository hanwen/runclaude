package serve

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"
)

// Tailnet is a running tsnet node exposing the session web UI on the tailnet
// (never via funnel — public exposure would bypass the identity model). Its
// Identifier resolves per-connection identity from tsnet WhoIs, the real
// authorization boundary.
type Tailnet struct {
	srv *tsnet.Server
	lc  *local.Client
}

// ListenTailnet brings up a tsnet node named hostname (state persisted under
// stateDir) and returns a tailnet-only listener plus an Identifier backed by
// WhoIs. authKey is optional (TS_AUTHKEY-style) for unattended first login; when
// empty, tsnet prints a login URL to logf on first run. Call Close to tear the
// node down.
func ListenTailnet(ctx context.Context, hostname, stateDir, authKey string, logf func(string, ...any)) (net.Listener, Identifier, *Tailnet, error) {
	srv := &tsnet.Server{
		Hostname: hostname,
		Dir:      stateDir,
		AuthKey:  authKey,
		// Keep tsnet's own chatter off our stdout; surface only the login URL.
		Logf:     func(string, ...any) {},
		UserLogf: logf,
	}
	if _, err := srv.Up(ctx); err != nil {
		srv.Close()
		return nil, nil, nil, fmt.Errorf("tsnet up: %w", err)
	}
	lc, err := srv.LocalClient()
	if err != nil {
		srv.Close()
		return nil, nil, nil, fmt.Errorf("tsnet local client: %w", err)
	}
	// Plain HTTP on the tailnet: traffic is WireGuard-encrypted node-to-node, and
	// this is the tailnet-only ("serve") path, not funnel.
	ln, err := srv.Listen("tcp", ":80")
	if err != nil {
		srv.Close()
		return nil, nil, nil, fmt.Errorf("tsnet listen: %w", err)
	}
	return ln, tsnetIdentifier{lc}, &Tailnet{srv: srv, lc: lc}, nil
}

// Close shuts the tsnet node down.
func (t *Tailnet) Close() error { return t.srv.Close() }

// tsnetIdentifier resolves identity via WhoIs on the node's local client.
type tsnetIdentifier struct {
	lc *local.Client
}

func (t tsnetIdentifier) Identify(r *http.Request) Identity {
	who, err := t.lc.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil || who.UserProfile == nil {
		// Fail closed: an unidentifiable caller gets a non-matching login that
		// no allowlist contains, so it can watch but never take control.
		return Identity{Login: "unknown", Name: "unknown"}
	}
	name := who.UserProfile.DisplayName
	if name == "" {
		name = who.UserProfile.LoginName
	}
	return Identity{Login: who.UserProfile.LoginName, Name: name}
}
