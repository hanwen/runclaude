# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`runclaude` is a Linux sandbox for running untrusted processes (primarily the `claude` CLI) on the host. It builds an isolated rootfs using unprivileged user namespaces, hides `$HOME` (where credentials live), and forces all egress through an in-process HTTPS proxy that MITMs Anthropic / Bedrock traffic to inject credentials the sandboxed process never sees.

No runtime dependencies — it's a single Go binary that re-execs itself in different modes via env vars.

## Build / test

```
go build -o runclaude .             # main binary
go build -o fake-anthropic ./cmd/fake-anthropic
go test ./...                       # unit + in-process e2e (TestMitmE2E*)
go test -run TestName ./...         # single test
```

Shell-level end-to-end tests (require `claude` on `$PATH` and working unprivileged user namespaces):

```
./test-mitm.sh              # full MITM e2e: fake-anthropic + claude, both Anthropic and Bedrock auth paths
./test-network.sh           # egress allowlist enforcement
./test-pathmap.sh           # $PATH bind-mounting
./test-workspace.sh         # git worktree / jj workspace mapping
./test-settings-sanitize.sh # .claude/settings.json AWS scrubbing
./test-claude-bedrock.sh    # live Bedrock (needs AWS creds)
./test-claude-cred.sh       # live Anthropic (needs ~/.claude credentials)
```

For deterministic MITM coverage that does not need the `claude` CLI, prefer `TestMitmE2EAnthropic` / `TestMitmE2EBedrock` in `mitm_e2e_test.go`.

## Architecture

### Re-exec state machine (`main.go`)

The binary runs in three modes, distinguished by env vars. Each mode re-execs the next via `exec.Command(self, ...)` with `SysProcAttr.Cloneflags`:

1. **`mainErr`** (no env set) — host-side entry. Parses flags, builds a `Config`, sets up the host-side proxy + DNS goroutines, creates a socketpair, then re-execs itself with `Cloneflags = CLONE_NEWUSER|CLONE_NEWNS` and single-uid `UidMappings`/`GidMappings` (the container only ever needs the user's own uid — see README). All caps are added to `AmbientCaps` so the new ns retains mount privileges across execve. `_RUNCLAUDE_CHILD=<json config>` is passed via env.
2. **`childMain`** (`_RUNCLAUDE_CHILD`) — inside user+mount ns. Bind-mounts host `/` (minus `/home`, `/tmp`, `/run`, `/dev`, `/mnt`, `/media`) into the rootfs, overlays cache dirs and user-specified binds, runs `checkContainment` (see `containment.go`) to refuse start when any mount inside rootfs aliases the cache via the same backing filesystem, then re-execs itself with `SysProcAttr.Cloneflags = CLONE_NEWPID|NEWIPC|NEWUTS|NEWCGROUP[|NEWNET]` and `_RUNCLAUDE_INIT=<json>` — the new process is pid 1 of a fresh pid ns.
3. **`initMain`** (`_RUNCLAUDE_INIT`) — mounts `/proc`, `pivot_root`s into the rootfs, sets up network inside the new netns (if `RestrictNet`), drops all caps, sets `HTTPS_PROXY` + CA env vars, then `execve`s the user command.

The host-side proxy listener / DNS sockets are created by `initMain` inside the new netns, then their fds are sent over the socketpair (fd 3) back to `mainErr`, which runs the proxy goroutines on the host. The container sees `127.0.0.1:<port>` because the netns has its own loopback.

### Network layer (`network.go`)

- Generates a CA on first use (`~/.cache/runclaude/`), persists it, and mints leaf certs on demand for MITM domains via `leafCache`.
- DNS server inside the netns resolves only allowlisted names (passthrough + MITM lists). Everything else: NXDOMAIN.
- The HTTP proxy enforces `CONNECT` allowlist; for MITM hosts it terminates TLS with the minted leaf and re-issues the request upstream with credentials injected by the `inject` callback.
- `inject` rewrites `Authorization` / `x-api-key` for `api.anthropic.com`, and SigV4-signs for `bedrock-runtime.*.amazonaws.com` using `awsCreds` retrieved on the host.
- `--mitm-upstream host=url` overrides the upstream for a host — used by tests to point at `fake-anthropic`.

### Claude-mode helpers

- `claudeBinds` (main.go) — exposes `~/.claude/` (minus `.credentials.json`!), `~/.claude.json`, and the dirs containing the `claude` binary. Credentials never enter the container.
- `writeStubCredentials` — drops a fake `.credentials.json` into the container's `$HOME/.claude/` so claude thinks it's logged in; the real token is held only by the host proxy.
- `claudesettings.go` — reads `.claude/settings.json` env into the host process (so runclaude sees `CLAUDE_CODE_USE_BEDROCK` etc. without the user exporting them), and `materializeClaudeSettings` overlays a sanitized copy into the container with all AWS env stripped (the in-container claude must not do its own AWS auth).
- `gitconfig.go` — `materializeGitConfig` copies the user's `~/.gitconfig` into the container's cache home with credential helpers / userinfo URLs stripped.
- `vcs.go` — `vcsExcludeBinds` overlays per-clone git/jj `info/exclude` with extra patterns (e.g. `.claude/settings.json`) so claude's runtime mutations don't show up as repo changes.
- `workspaceBinds` (main.go) — detects linked git worktrees (`.git` file → `gitdir:`) and jj workspaces (`.jj/repo` → repo path), binds the underlying repo storage so VCS works inside the container.

### Cache layout

Per-cwd cache at `~/.cache/runclaude/<sha256(cwd)[:16]>/` with subdirs `home/`, `tmp/`, `run/`, `vcs/`, `claude/`. These are bind-mounted to `$HOME`, `/tmp`, `/run` inside the container — so build/module/auth caches persist across sessions for the same working directory.

### Bedrock path

When `--claude` + `CLAUDE_CODE_USE_BEDROCK=1`: the host calls `awsconfig.LoadDefaultConfig` (so SSO / profile / IMDS all work on the host), keeps the creds, and the in-container claude is given stub `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`. The container's aws-sdk signs requests with the stub; the MITM strips the stub signature and re-signs with the real creds. `scrubAWSCreds` ensures no real AWS env leaks into the container.

### `fakeapi/` and `cmd/fake-anthropic/`

A fake Anthropic + Bedrock server used by both `go test` and `test-mitm.sh`. Validates credentials (so MITM injection is actually tested) and streams `FixedReply` ("I am a fake anthropic API server") back as SSE — tests grep for that string.
