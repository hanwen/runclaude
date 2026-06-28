# Plan: Share a runclaude session over the web (multi-writer pair programming)

> Self-contained design doc. You should be able to start a fresh session with
> only this file and the repo, and begin at Phase 0.

## Goal

Export a running `runclaude` claude session over the internet so multiple people
can:

- watch the agent's progress live,
- have a read-only audience,
- and let writers **explicitly take control** to submit prompts (one writer at a
  time, identity-attributed).

Access/identity and NAT traversal are handled by **Tailscale**. Source-checkout
context (`jj show @`) is exposed as a separate read-only view.

This is "Option 3" from the design discussion: drive `claude` headless via its
**stream-json protocol** and build a web UI from the structured event stream.
It is *not* a terminal/TUI broadcast (that was the rejected Option 2).

## Implementation status

All phases are built and tested against the real CLI (2.1.183):

- **Phase 0** — `agentclient/`: stream-json transport port + `cmd/agentspike`.
- **Phase 1** — `--serve`: sandbox stdio pipes threaded through the re-exec chain
  (`Config.{CommFd,ServeInFd,ServeOutFd}`, `runAsInit`).
- **Phase 2** — `sessionhub/` (transcript + fan-out), `serve/` (SSE + `/whoami`),
  `frontend/`. Identity is a **pluggable `Identifier`/`Policy`** seam.
- **Phase 2b** — `serve/tsnet.go`: embedded `tsnet` node via `--serve-tailnet
  <hostname>` (tailnet-only, never funnel); `Identifier` resolves
  `LocalClient.WhoIs`. `TS_AUTHKEY` for unattended login. Pinned
  `tailscale.com@v1.98.0` (latest that builds with the repo's Go toolchain).
- **Phase 3** — single-writer take-control (steal-immediately), web prompts,
  identity-gated eligibility, audit log.
- **Phase 4** — controller-gated interrupt (`control_request`/`interrupt`).
- **Phase 5** — read-only `jj show @` source panel (`/source`), terminal echo,
  this doc + `CLAUDE.md`.

The tsnet path compiles, comes up, persists node state under
`<cacheDir>/tsnet/`, and reaches the login step; a **live tailnet run** (auth +
serving + WhoIs round-trip) could not be exercised from the build host here,
which runs *inside* runclaude and whose egress allowlist blocks Tailscale's
control plane. Validate it on a host with open egress and a tailnet:
`TS_AUTHKEY=… runclaude --serve --serve-tailnet myhost --serve-writer you@example.com`.
Localhost mode (`--serve-addr`, optionally `--serve-dev`) is fully exercised.

## How to test

```
go build -o runclaude .
go test ./...          # unit + in-process e2e; agentclient/sessionhub/serve
```

**Protocol spike (no sandbox, no web)** — sanity-check the stream-json port
against the real CLI:

```
go run ./cmd/agentspike "what is 2+2?" "now multiply that by 3"
```

**Localhost, single user** — run a sandboxed session and open the web UI:

```
cd <a project dir>
runclaude --serve --serve-addr 127.0.0.1:8711
# browse http://127.0.0.1:8711 ; type prompts in the terminal too (operator path)
# the web header shows the session id (copy button). To continue it later, in the
# SAME cwd: runclaude --serve --serve-resume <id>
```

**Localhost, simulate multiple writers from one machine** — `--serve-dev` lets
each browser tab pick an identity via `?as=<login>`; everyone is write-eligible
unless you pass `--serve-writer`:

```
runclaude --serve --serve-dev
# tab A: http://127.0.0.1:8711/?as=alice   tab B: .../?as=bob
# take control in A, watch B's prompt box gate; take control in B -> A sees
# "you lost control to bob". Add --serve-writer alice to make bob's take 403.
```

Same flow via curl (note `?as=` rides on every request; the participant `id` in
the body is per-tab):

```
b=http://127.0.0.1:8711
curl -s "$b/whoami?as=alice"                                   # {"login":"alice",...,"mayWrite":true}
curl -s -XPOST -d '{"id":"A","name":"alice"}'        "$b/take-control?as=alice"
curl -s -XPOST -d '{"id":"A","name":"alice","text":"hello"}' "$b/prompt?as=alice"   # 204
curl -s -XPOST -d '{"id":"B","name":"bob","text":"hi"}'      "$b/prompt?as=bob"     # 409 (not controller)
curl -s -XPOST -d '{"id":"A"}'                        "$b/interrupt?as=alice"       # stop a running turn
curl -sN "$b/events?as=carol"                                 # SSE transcript stream (read-only)
curl -s  "$b/source"                                          # jj show @ (read-only panel)
```

If your shell itself runs inside runclaude, clear the proxy for these:
`http_proxy= https_proxy= curl …`. Prompt/audit lines (who drove, what they
asked) print to the runclaude stderr.

**Tailnet (real identities)** — on a host with open egress + a tailnet:

```
TS_AUTHKEY=tskey-… runclaude --serve --serve-tailnet myhost \
    --serve-writer you@example.com --serve-writer teammate@example.com
# without TS_AUTHKEY, a login URL is printed; open it once to register the node.
# then browse http://myhost/ from any tailnet device; identity = WhoIs login,
# only the --serve-writer logins may take control.
```

## Key decisions (locked unless noted)

1. **UI is web-rendered from the stream-json event stream, not the TUI.** Headless
   claude has no TUI; the frontend is a chat/transcript UI we render ourselves.
2. **Go port of the SDK transport, not the TS/Python SDK.** runclaude is a single
   Go binary with zero runtime deps; pulling in Node/Python + the SDK's own
   subprocess-spawning model fights that and the sandbox. We speak the stream-json
   wire protocol directly in Go. The SDK is only a *convenience* layer over this
   protocol — the agent intelligence lives in the `claude` CLI either way.
3. **Tailscale via embedded `tsnet`, using `serve` (tailnet-only), never `funnel`.**
   Funnel exposes to the public internet and bypasses the identity model. Identity
   per connection via `tsnet` `LocalClient.WhoIs(remoteAddr)`.
4. **Single writer at a time, explicit take-control.** Identity-gated. Because
   every prompt arrives bound to a Tailscale identity, we get an audit trail for
   free (which terminal-sharing options could not provide).
5. **Keep `--dangerously-skip-permissions`** (see "Permissions" below). This makes
   the riskiest part of the build (the permission round-trip control subprotocol)
   unnecessary. Phase 4 collapses to "interrupt."
6. **Additive `--serve` mode.** The existing interactive path is untouched.

### Decisions locked since first draft

- **Take-control = steal-immediately.** Any eligible writer can take control at any
  time; the hub atomically transfers the single writer token to them and the
  previous controller drops to viewer. No pending-request/grant state. (The
  displaced controller should get a visible "you lost control to X" event.)

### Decisions still open (confirm before/while building)

- **Role policy source:** local config allowlist (recommended for v1) vs. tailnet
  ACL grants for who-may-write. See "Role policy" below. Plan assumes a small
  pluggable interface so this can change without touching the hub.
- **Human-to-human governance:** whether to ever drop skip-permissions to route
  tool approvals between users (see Permissions). Default: no, ship trusting the
  take-control gate.

## How runclaude works today (the integration seam)

runclaude is a re-exec state machine in `main.go`, three modes via env vars:

1. `mainErr` (no env) — host entry. Parses flags, builds `Config` (`main.go:71`),
   sets up the **host-side MITM proxy + DNS goroutines**, then re-execs into a new
   user+mount namespace. **The host process stays alive to run the proxy** — this
   is where the web server/session hub will also live.
2. `childMain` (`_RUNCLAUDE_CHILD`) — inside user+mount ns. Bind-mounts rootfs,
   re-execs into a fresh pid/net ns.
3. `initMain` (`_RUNCLAUDE_INIT`) — pivots root, sets up netns + proxy env, drops
   caps, then launches the user command via `runAsInit` (`main.go:1283`).

**The seam:** `runAsInit` (`main.go:1283`) currently wires the child's stdio to the
terminal:

```go
cmd.Stdin  = os.Stdin
cmd.Stdout = os.Stdout
cmd.Stderr = os.Stderr
```

For `--serve` we instead connect claude's stdin/stdout to **pipes carrying
stream-json**. claude is launched (today) as:

```go
// main.go:787
cmd := []string{"claude", "--dangerously-skip-permissions"}
```

For `--serve` this becomes roughly:

```
claude --dangerously-skip-permissions \
       --print --input-format stream-json --output-format stream-json --verbose
```

(Confirm exact flags against the CLI version during Phase 0.)

### Data-path plumbing (pipes, not the netns)

The MITM proxy uses a socketpair (fd 3) to pass *listener fds* from inside the
netns back to the host, because the listener must be born inside the netns. **Our
stdio pipes do not need that** — they're ordinary pipes:

- `mainErr` (host) creates two `os.Pipe()` pairs before re-exec.
- The relevant ends are threaded down through `childMain` → `initMain` via
  `exec.Cmd.ExtraFiles` (they survive the re-execs as inherited fds).
- `runAsInit` sets `cmd.Stdin`/`cmd.Stdout` to the in-sandbox ends.
- The host keeps the other ends and runs the session hub on them.
- The **web server + tsnet live on the host** (`mainErr`), which has real network
  access; the sandbox netns stays isolated. stderr can stay on the terminal or be
  captured for logs.

This mirrors the existing "sandboxed process, host-side network surface" design.

## Component map

```
agentclient/   Go port of the stream-json transport (the "SDK port")
sessionhub/    single source of truth: owns one agentclient, canonical transcript,
               fan-out to subscribers, single-writer control lock
serve/         tsnet web server: serves embedded frontend + WebSocket,
               WhoIs -> identity -> role, take-control handling
frontend/      go:embed static HTML/JS: renders transcript from events, shows
               who holds control, take-control button, prompt box (controller only),
               separate read-only jj panel
main.go        --serve flag; runAsInit launches claude in stream-json mode with
               piped stdio; host runs hub + tsnet server
```

### `agentclient/` — the SDK port

Ported from the Python SDK's `_internal/transport/subprocess_cli.py` (spawn + pipe
framing) and `types.py` (message schema). Responsibilities:

- Spawn claude with stream-json flags (or attach to the inherited pipe fds).
- Write JSON-line user messages to stdin.
- Read JSON-line events from stdout: `system` (init → carries `sessionId`),
  `assistant`, `tool_use`, `result`.
- Session resume = pass the session id flag.
- **Deferred:** the bidirectional control subprotocol (`interrupt`, `canUseTool`).
  The plain message stream is stable and simple; the control channel is
  under-documented and version-locked to the CLI (the SDK ships lockstep with a
  bundled CLI). Only `interrupt` is needed (Phase 4), and only if wanted.

### `sessionhub/`

- Owns the single `agentclient`.
- Maintains the canonical ordered transcript (so late joiners get full history).
- Fans events out to all WebSocket subscribers.
- Accepts prompts **only from the current controller**; enforces the single-writer
  token.
- Logs every prompt with its Tailscale identity (audit trail).

### `serve/` (tsnet)

- Embeds `tsnet`, joins the tailnet, exposes via `serve` (tailnet-only).
- `LocalClient.WhoIs(remoteAddr)` → user identity for each HTTP/WS connection.
- Maps identity → role (viewer / writer-eligible) via the role policy (config
  allowlist for v1; tailnet ACL grants/tags optional later).
- Handles "take control" requests, gated by role + the hub's control lock.

### Frontend

- Static, `go:embed`-ed. Renders the transcript from the structured event stream
  (assistant text, tool calls, results) — a chat-style view, not a terminal.
- Shows who currently holds control; "take control" button for eligible writers.
- Prompt input enabled only for the controller.
- Separate read-only **jj panel**: its own endpoint streaming `jj show @` run in
  the sandbox cwd (timer refresh for v1; fsnotify on `.jj/` optional). Inherently
  read-only, so it sidesteps all RO/RW access-control concerns. Can land any time
  after Phase 2.

## Role policy: local allowlist vs tailnet ACL

Both share the same identity source — `tsnet` `WhoIs(remoteAddr)` gives the
Tailscale login for every connection regardless. The only question is **where
write-eligibility is encoded.** "Eligibility" (may this user ever write) is a
separate layer from "steal" (which eligible user holds the token right now).

**Local config allowlist** — runclaude holds a list of identities permitted to
write; `serve` checks `WhoIs` against it.

- Owner-controlled and ad hoc: you (whoever runs runclaude) can add a writer
  instantly, no tailnet-admin rights. Best for "let Bob drive right now."
- Trivial to build (~a slice of strings) and per-session/per-host scoped — misconfig
  blast radius is this session only.
- One service can carry both viewers and writers; the app decides per-connection,
  so the RO/RW split lives in code, not in network topology.
- Cost: policy is app-local — no central review/version history, and it can drift
  across hosts. You build your own audit of who's on the list.

**Tailnet ACL grants** — encode write-eligibility in the central Tailscale policy
(grants carrying an app capability, read via `WhoIs`/CapMap; or coarse
network-level reachability to a writable port).

- Central single source of truth: versioned, reviewable, org-wide — fits a governed
  environment and many users/services.
- Idiomatic Tailscale authz (grants are designed for "service exposes capability X
  to these principals").
- Cost: editing eligibility needs **tailnet-admin access** (bad for ad hoc; you
  can't just promote someone mid-session without a policy edit + propagation), and
  misconfig touches central policy (bigger blast radius, but caught by review).
- If you use coarse *network-level* ACL (reachable == eligible) you must split RO
  and RW into separate services/ports, since one port can't distinguish them. ACL
  *grants/caps* avoid this (one service, per-user capability) at the cost of more
  setup (model the grant, parse CapMap).

**Recommendation:** local allowlist for v1 — it matches the ad-hoc pairing use case,
needs no tailnet-admin dependency, and keeps one service with the RO/RW decision in
code. Move to (or add) ACL grants if this ever runs in a governed org context where
central, reviewable policy matters. They're not exclusive: the policy interface can
read tailnet grants *and* fall back to a local allowlist.

## Identity layering & single-user testing

Identity is **two layers**, and conflating them is a trap:

1. **Tailscale identity** (from `tsnet` `WhoIs`) = *authorization + grouping*: may
   you connect, are you write-eligible (allowlist/grant). This is the real security
   boundary.
2. **Session participant identity** = a per-connection label (chosen or assigned at
   connect time). The steal/take-control logic and the audit log key on *this* for
   attribution within a session.

This split is correct regardless of testing: one real Tailscale user legitimately
has multiple tabs/devices, and we want per-connection attribution anyway.

**Guardrail:** the participant label is cosmetic/coordination only — never the authz
gate. Real authorization stays on the Tailscale identity, or someone could name
themselves "owner" and self-promote.

**Testing as a single user (no second account needed):** open several tabs, each
names itself (Alice/Bob), all backed by your one Tailscale login. All are
write-eligible (same Tailscale identity), but are distinct participants — so Bob can
steal from Alice, you get the "lost control" event, fan-out hits both, audit shows
two participants. Exercises Phases 2–4 solo.

To also test the *authorization* layer with genuinely distinct principals, without a
second Google account:

- **Dev-only `WhoIs` override** (`?as=alice` / header) behind a `--dev` flag —
  impossible in production. Tests allowlist *denies*, not just the happy path.
- **A second `tsnet` node** with its own auth key + tag (e.g. `tag:guest`). `WhoIs`
  distinguishes nodes, not just users — a second real principal, one human. Heavier;
  use only to validate the WhoIs plumbing itself.

## Permissions: why Phase 4 is small

runclaude launches `claude --dangerously-skip-permissions` (`main.go:787`). That is
the `bypassPermissions` mode. In the permission pipeline
(`PreToolUse → deny → allow → ask → permission-mode check → canUseTool → PostToolUse`)
bypass short-circuits at the mode check, so **`canUseTool` is never called and no
tool-approval prompts exist.** As long as we keep the flag, the permission-approval
machinery — the under-documented, version-coupled control round-trip — is
**unnecessary**.

What remains for "Phase 4":

- **Interrupt** — stop a runaway turn. A control action, not a prompt; simpler and
  more stable than the permission round-trip. Keep it.
- **Clarifying questions the agent asks** arrive as normal transcript text and end
  the turn; answered via the normal prompt box. No special machinery.

**Optional governance fork (not default):** in a multi-writer setting you *could*
deliberately drop `--dangerously-skip-permissions` and use `canUseTool` as a
human-to-human gate (e.g., session owner approves what guest writers trigger). That
is the *only* reason to build the permission round-trip back, and it carries the
CLI-version-pinning cost. Default plan: keep skip-permissions, trust the
take-control gate, treat governance as a later optional feature.

## Phased build

**Phase 0 — protocol spike (de-risk the wire format).**
Port the framing to Go; drive `claude` headless from a plain host-side `main`;
print the transcript. No sandbox, no web. Validates exactly how big the SDK port
is. Deliverable: a Go program that sends a prompt and prints the streamed reply.

**Phase 1 — sandbox plumbing.**
Add `--serve`. Create stdio pipes on the host; thread them through
`childMain → initMain` via `ExtraFiles`; point `cmd.Stdin/Stdout` at them in
`runAsInit`. Host-side hub just prints events. Single local user, no web.
Deliverable: a sandboxed claude driven over pipes from the host.

**Phase 2 — read-only web.**
tsnet `serve` + WebSocket + minimal frontend rendering the transcript. Everyone
read-only; `WhoIs` identity shown but not gating yet. **This phase alone delivers
"multiple people watch the agent" + the read-only audience** — usable on its own.
Deliverable: open a tailnet URL, watch the live transcript from several browsers.

**Phase 3 — single writer with explicit take-control.**
Prompt input; control token in the hub; "take control" **steals** the token
atomically (previous controller drops to viewer, gets a "lost control to X"
event). Only the controller's prompts reach claude. `WhoIs` + role policy
gate who may take control. Prompts logged with identity (audit). **This is the
core requested feature.**
Deliverable: two users, one watching, one taking control and driving the agent.

**Phase 4 — interrupt (small; permissions only if governance wanted).**
Wire an interrupt button to the control channel to stop a turn. Pin the claude CLI
version if/when touching the control subprotocol. Permission round-trip only if you
opt into human-to-human governance (then drop skip-permissions for shared sessions).
Deliverable: a working "stop" button.

**Phase 5 — jj panel + polish.**
Standalone read-only `jj show @` stream (can land after Phase 2), audit-log
surfacing, teardown discipline (don't leave a writable session listening).

## Security notes

- A writer drives an agent that runs commands = **RCE, contained only by
  runclaude's sandbox** — which is exactly why doing this *inside* runclaude is the
  right place. Default everyone to viewer; promotion to writer is explicit and
  identity-gated.
- Stay on the tailnet (`serve`); never `funnel` the writable surface.
- Credentials never enter the container (existing runclaude property, unchanged):
  the real Anthropic/Bedrock creds stay in the host MITM proxy. A malicious writer
  with full sandbox RCE still cannot exfiltrate the token, only use it via the
  proxy (quota/abuse) within the egress allowlist.
- Everything on screen (transcript + jj panel) is visible to all viewers; don't
  share a tree containing secrets.

## Effort (Claude-assisted, honest)

Phases 0–3 — the genuinely useful product (view + read-only + identity-gated
take-control writing) — are a small protocol port plus well-trodden Go web code:
**days of supervised iteration**, preserving runclaude's single-binary/zero-dep
identity. Phase 4 is tiny under skip-permissions. The version-fragile permission
work only exists if you opt into governance.

Compression note: code authoring compresses well with an agent; the
iteration-bound parts (getting the wire protocol and tsnet wiring right against
real clients) do not. Budget for run-it-and-watch-it-break cycles, not typing.

## References

- Python SDK (cleanest protocol reference — pure subprocess-pipe client):
  https://github.com/anthropics/claude-agent-sdk-python
  - `src/claude_agent_sdk/_internal/transport/subprocess_cli.py` — the Go port target
  - `src/claude_agent_sdk/types.py` — message schema
- TypeScript SDK (bundles the binary):
  https://github.com/anthropics/claude-agent-sdk-typescript
- SDK docs: streaming input, permissions, sessions, user-input under
  https://docs.claude.com/en/docs/agent-sdk/ (and platform.claude.com mirror)
- Note: SDK and CLI ship lockstep (e.g. Python v0.2.107 ↔ CLI 2.1.186); pin the
  CLI version if you ever depend on the control subprotocol.

## Key repo anchors

- `main.go:71` — `Config` struct (extend for serve options if needed)
- `main.go:787` — where `claude --dangerously-skip-permissions` is assembled
- `main.go:1283` — `runAsInit`, the stdio seam to repoint at pipes
- `main.go:417` — `mainErr`, host entry that stays alive for the proxy (and will
  host the web server)
- `network.go` — MITM proxy / DNS / leaf certs (host-side network surface pattern
  to mirror for the web server)
- `CLAUDE.md` — architecture overview
