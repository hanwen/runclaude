# runclaude on macOS — design plan

## Objective

Run runclaude's sandbox on macOS with the same security contract as on Linux: the
untrusted process cannot read the user's credentials or home directory, and all
egress is forced through the host-side MITM proxy that injects credentials the
sandboxed process never sees. Keep the single-binary, no-runtime-deps property.

## Mechanism choice

macOS has no user/mount/pid/net namespaces and no unprivileged chroot. The viable
sandbox is **Seatbelt**: `/usr/bin/sandbox-exec` with a generated SBPL profile
(or `sandbox_init(3)`). It is nominally deprecated since 10.14 but ships with
every macOS and is what Bazel, Chromium, and Anthropic's own sandbox-runtime
(`srt`, the engine behind Claude Code's built-in sandbox) use in production —
the exact stack we need (node + claude under Seatbelt, egress via a localhost
proxy) is proven. Deprecation risk is handled by isolating profile generation +
launch behind a platform interface.

The fundamental model change: **Seatbelt is an allow/deny filter over real
paths — it cannot remap paths or synthesize file views.** Linux runclaude is
built on bind-mount synthesis (a cache dir *appears at* `$HOME`, a sanitized
copy *appears at* the original settings path, a stub credentials file appears
where the real one lives). The macOS port therefore becomes: *deny the secrets,
and redirect the process to host-side materialized copies via environment
variables* (`HOME`, `TMPDIR`, `CLAUDE_CONFIG_DIR`) *and symlinks*.

The three-stage re-exec state machine (`mainErr` → `childMain` → `initMain`)
collapses on macOS: no netns means no fd-passing socketpair, no pivot_root, no
init shim. `mainErr` materializes the config trees, generates a profile, starts
the proxy on host loopback, and runs `sandbox-exec -f <profile> -- <command>`.

## Phase 0: Linux simplifications that reduce bifurcation

Several darwin mechanisms are strictly more general than the current Linux
ones. Adopting them on Linux first means the darwin port adds code instead of
forking it, and each lands as a standalone, testable refactor commit.

### 0.1 Make the merged `.claude` path unconditional

Today there are two claude-config paths: the common one (per-entry
`claudeBinds` enumeration + `materializeClaudeSettings` overlay binds +
`vcsExcludeBinds`) and the AWS-project one (`buildMergedClaudeDir` +
`--setting-sources user`). Darwin needs the merged path always; making it
unconditional on Linux too:

- deletes `vcsExcludeBinds` (vcs.go) entirely — it only hides in-place
  mutation of the project `settings.json`, which the merged path never does;
- deletes `materializeClaudeSettings`'s overlay binds — sanitization happens
  while writing the merged tree;
- collapses the `mergeProjectClaude` branching in `mainErr` to one path and
  shrinks `claudeBinds` to live-state handling;
- simplifies `test-settings-sanitize.sh` to merged-tree assertions.

Accepted costs (already the tradeoff on today's AWS path, now universal):
`--setting-sources user` always; per-session tree rebuild; in-session edits to
`settings.json` are discarded (live state is still shared). Document in README.

### 0.2 `CLAUDE_CONFIG_DIR` + symlinks instead of `Dest` binds

Point `CLAUDE_CONFIG_DIR` at the merged tree instead of bind-mounting it *at*
`~/.claude`. Live-state entries (`claudeLiveState`: `projects/`,
`history.jsonl`, `todos/`, …) and `.claude.json` become **symlinks** from the
merged tree to the real paths; Linux binds the symlink targets as plain
`Dest==Path` binds, darwin adds profile allows on the same targets. Seatbelt
evaluates resolved paths, so the allows are exactly as narrow as the binds.
The tree construction — the fiddly part — becomes fully shared code.

The recorder's repo-scoped sessions bind (`Bind{Path: rec.ProjectsDir, Dest:
cwdDir}` in mainErr) is replaced the same way: `projects/` in the merged tree
is a real directory whose `<encoded-cwd>` entry is a symlink to the shared
main-worktree dir, identical on both platforms.

### 0.3 Drop the `Tmpfs` bind type

`claudeScratchBinds`' per-session tmpfs overlays exist because the `.claude`
bind used to be per-cwd-shared. With the merged tree per-session (`MkdirTemp`),
empty real directories in it give the same isolation — including the
session-env deletion race fix. Deletes `Bind.Tmpfs`, `claudeScratchBinds`, and
the tmpfs branch in `childMain`.

### 0.4 Shrink `Bind` to `{Path, ReadOnly}`; platform-neutral sandbox spec

After 0.1–0.3, the remaining `Dest != Path` users are `--only` mode's cwd remap
(stays Linux-only, see below) and `/runclaude.json` — move the latter to an env
var (`RUNCLAUDE_JSON=<path in merged dir>`) on both platforms. The config then
becomes a declarative spec — read-write paths, read-only paths, exec paths,
env, command, proxy address — that Linux compiles to binds + namespaces and
darwin compiles to an SBPL profile. This spec is the `sandbox` interface seam;
above it, `mainErr` is platform-independent.

### 0.5 Proxy auth token on both platforms

Darwin's proxy sits on shared host loopback, so it needs a per-session random
token (`HTTPS_PROXY=http://runclaude:<token>@127.0.0.1:<port>`, checked and
stripped by `proxy.Serve`) — otherwise *any local process* gets
credential-injected egress. Adding the token on Linux too is free (the netns
already isolates; this is defense-in-depth) and keeps proxy setup, env
construction, and the approval-UI hardening (token in its URL as well)
identical across platforms.

### 0.6 (Experiment) `CLAUDE_CODE_OAUTH_TOKEN` stub instead of the stub file

If claude honors a stub `CLAUDE_CODE_OAUTH_TOKEN` (skipping credential-file
and keychain lookup, accepting the value client-side), `writeStubCredentials`
disappears on both platforms and the darwin keychain-fallback question (below)
becomes moot — the MITM replaces the stub either way. Verify empirically before
relying on it; keep the stub file as fallback.

## Synthesized-view inventory (Linux mechanism → macOS replacement)

| Linux synthesis | macOS replacement |
|---|---|
| Cache dir mounted **at** `$HOME` (childMain cache mounts) | `HOME=<cacheDir>/home` env + default-deny reads of the real home. `getpwuid()`/`NSHomeDirectory()` still return the real home — the Seatbelt deny makes those reads fail, which is the correct outcome. cwd is *not* relocated, so transcript/recorder `cwd` fields stay host-identical. |
| `~/.claude` overlay stack (claudeBinds minus credentials, merged dir, sanitized settings, stub credentials) | One mechanism: per-session merged tree + `CLAUDE_CONFIG_DIR` (phase 0.1/0.2). Stub credentials written into the merged tree (or env stub, 0.6). Real `~/.claude/.credentials.json` and the keychain are denied. |
| Project `cwd/.claude/settings.json` sanitize overlay + `vcsExcludeBinds` | Not needed: the always-merged path never presents or mutates files at in-repo paths (deleted in phase 0.1). |
| Recorder sessions bind `ProjectsDir → cwdDir` | `projects/<encoded-cwd>` symlink in the merged tree (phase 0.2) + rw allow on the real shared dir. |
| `claudeScratch` tmpfs overlays | Real empty dirs in the per-session merged tree (phase 0.3). |
| `/tmp`, `/run` cache mounts | `TMPDIR=<cacheDir>/tmp` (macOS-native convention). `/run` has no macOS equivalent. Hardcoded `/tmp` users: allow writes to `/private/tmp` (shared with host; acceptable — the threat model is home/credential exfiltration, not /tmp). |
| `/etc/resolv.conf` stub + in-netns DNS server | None. macOS resolves via the `mDNSResponder` unix socket; there is no per-process resolver override. Deny that socket (DNS as exfil channel, matching the Linux NXDOMAIN design) and rely solely on the proxy CONNECT allowlist — proxy-aware clients (claude/node, curl, git) pass hostnames to the proxy. Cost: non-proxy-aware tools fail with resolution errors, and the approval UI only sees denials that reach the proxy. |
| `/runclaude.json` bind | `RUNCLAUDE_JSON` env var (phase 0.4, both platforms). |
| `pathBinds`, `workspaceBinds`, `jjConfigBinds`, `--expose` | No remap involved — direct profile allow rules: rw for workspace git/jj storage and `--expose`; read+exec for PATH dirs and the claude binary dirs (native installs live in `~/.claude/local`, inside the denied real home — allows must be explicit, including `process-exec`); read-only for jj config. |
| `--only` mode (cache-backed cwd with selected items overlaid) | **No faithful equivalent** — deny-all-but-listed yields EPERM (visible-but-forbidden) instead of ENOENT (absent), and tools behave differently on the two. v1: `--only` is Linux-only. Possible v2: APFS `clonefile()` CoW copies of the listed items into a shadow cwd — but the changed path perturbs per-cwd session identity; needs its own design. |
| `materializeGitConfig` into cache home | Unchanged — already writes into `<cacheDir>/home`, which *is* `$HOME` on darwin. Credential-helper stripping already covers git's osxkeychain helper. |

## Edge cases beyond file synthesis

- **Credentials live in the Keychain on macOS**, not `.credentials.json`.
  Host side: `creds.LoadAnthropicCredentials` gets a darwin path reading the
  `Claude Code-credentials` generic-password item via `security
  find-generic-password -w` (payload is the same JSON shape), and
  `TokenSource`'s refresh persistence writes back via `security
  add-generic-password -U` instead of the file rewrite. Sandbox side: deny
  keychain files (`~/Library/Keychains`) and the securityd mach services, then
  verify what in-sandbox claude does — falls back to the credentials file
  (where our stub sits), or needs the `CLAUDE_CODE_OAUTH_TOKEN` stub (0.6).
  **Biggest empirical unknown; test on real hardware first.**
- **Sandbox → host loopback**: the sandbox could reach the user's local dev
  servers, Docker socket, ollama, ssh-agent. Scope the network allow to
  exactly the proxy port; deny unix sockets by default, with the baseline
  mach-lookup/sysctl allows a functioning process needs cribbed from the
  srt/Bazel profiles (they encode years of "what node actually requires").
- **No pid namespace**: the sandbox can enumerate host processes via sysctl
  (`ps eww` exposes same-user process environments — often secrets) and signal
  them. Profile: `(deny process-info* …)` and `(deny signal …)` except
  self/same-sandbox.
- **EPERM vs ENOENT on ancestor walks**: git walks up from cwd looking for
  `.git`; node walks up resolving `node_modules`. cwd typically sits *under*
  the denied real home, so grant `file-read-metadata` on cwd's ancestor chain
  (standard Seatbelt trick) — stats succeed, content reads stay denied.
- **Path canonicalization**: Seatbelt matches resolved paths. `/tmp` →
  `/private/tmp`, `/var` → `/private/var`, `$TMPDIR` under
  `/private/var/folders`, firmlinked `/Users`. Realpath every path at profile
  generation time (generalize the `EvalSymlinks` habit from `pathBinds`).
- **CA trust**: the env-var story (`SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`,
  `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`) carries over and covers claude, node,
  git, curl. Tools that consult the *system keychain* trust store ignore these
  and fail TLS on MITM'd hosts; installing the CA system-wide requires an
  admin prompt — don't. Accepted gap.
- **Bedrock path**: unchanged. Env stubs + host-side SigV4 re-signing work
  identically; `scrubAWSCreds` is portable; `~/.aws/` is inside the denied
  real home, matching the Linux hiding.
- **Recording**: essentially free. Host-side `git`/`jj` exec and
  `syscall.Flock` exist on darwin; `RestoreCheckpoint` untouched; only the
  projects-dir redirect changes (phase 0.2, shared).
- **Accepted isolation losses**: no hostname/IPC isolation, shared `/dev`
  (ptys work under Seatbelt with the standard `/dev/ttys*` allows), and
  Seatbelt is a weaker boundary than namespaces against kernel-level escape.
  The threat model — untrusted claude-driven code vs. the user's credentials
  and home directory — is still covered.

## Implementation steps

1. **Phase 0 refactors on Linux** (0.1–0.5 above), each a standalone commit
   with existing tests green; 0.6 as a spike.
2. **Portability split**: move namespaces/mounts/caps/fd-passing/`childMain`/
   `initMain`/`checkUserNS`/containment/mountinfo/nftables into
   `sandbox_linux.go` (+ `_linux` files) behind the spec interface from 0.4.
   Get `GOOS=darwin go build` clean.
3. **Darwin materialization**: merged-tree construction is shared already
   (phase 0); darwin adds only `HOME`/`TMPDIR` redirection and skips binds.
4. **SBPL profile generator** (`profile_darwin.go`): default deny
   `file-write*`; broad `file-read*` allow minus real home, keychains, and
   other secret stores; the computed allow list (cwd rw, cache home rw,
   workspace storage rw, live-state targets rw, PATH/claude dirs read+exec,
   ancestor metadata); network deny except loopback:proxyport; deny
   mDNSResponder, `process-info*`, `signal`; baseline mach/sysctl allows from
   the srt/Bazel profiles. Golden-file tests for generated profiles.
5. **Proxy without netns**: darwin `mainErr` listens on `127.0.0.1:0`
   directly; token auth already in place from 0.5.
6. **Keychain creds**: `creds/anthropic_darwin.go` (load + refresh
   write-back via `security`); pick the stub strategy after the 0.6/keychain
   experiments.
7. **Launch**: `exec /usr/bin/sandbox-exec -f <profile>` with the env set
   (`HOME`, `TMPDIR`, `CLAUDE_CONFIG_DIR`, `HTTPS_PROXY` incl. token, CA
   vars, `RUNCLAUDE_JSON`). No init shim: the child is a normal foreground
   process; signal/tty handling is the shell's problem again.
8. **Tests**: `go test ./...` on darwin (fakeapi + MITM e2e are in-process
   and run once the proxy is netns-free); `test-seatbelt.sh` analog of
   `test-network.sh` asserting real-home-read denial, direct-egress denial,
   proxy-only egress with token; manual live test for the keychain path.

## v1 scope cuts

`--only` unsupported on darwin; no DNS for non-proxy-aware tools; tools using
the system truststore unsupported through MITM'd hosts; no
hostname/pid/IPC isolation (mitigated in-profile as above).

## Open questions (verify on real hardware before building on them)

1. Claude's credential fallback order when the keychain is denied — file stub
   sufficient, or is `CLAUDE_CODE_OAUTH_TOKEN` (0.6) required?
2. Does claude + its shell tool run cleanly under a default-deny Seatbelt
   profile with only the allows above? Diff against srt's profile when not.
3. Exact `CLAUDE_CONFIG_DIR` semantics for `.claude.json` placement in current
   claude releases (the 0.2 symlink assumes `$CLAUDE_CONFIG_DIR/.claude.json`).
