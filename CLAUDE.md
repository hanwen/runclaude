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
./test-record.sh            # session recording e2e (no claude CLI needed)
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
- `--test.mitm-upstream host=url` overrides the upstream for a host — used by tests to point at `fake-anthropic` (like the other `test.*` flags, it is hidden from `--help`).
- Runtime approval (`proxy/approval.go`): the passthrough allowlist is a concurrency-safe `proxy.Allowlist` shared by both the proxy allow-check and the DNS server, so it can grow mid-session. A `proxy.Approver` records every denied host (`Setup.OnDeny`) and serves a small localhost web UI (`mainErr` starts it on `--approve-listen`, default `127.0.0.1:0`, address printed on startup) that lists denied hosts with an "approve" button; approving calls `Allowlist.Add`, immediately permitting DNS + egress for that host. Approvals are in-memory only (not persisted across sessions).

### Claude-mode helpers

- `claudeBinds` (main.go) — exposes `~/.claude/` (minus `.credentials.json`!), `~/.claude.json`, and the dirs containing the `claude` binary. Credentials never enter the container.
- `writeStubCredentials` — drops a fake `.credentials.json` into the container's `$HOME/.claude/` so claude thinks it's logged in; the real token is held only by the host proxy.
- `trust.go` — `ensureProjectTrusted` pre-marks the cwd trusted in `~/.claude.json` before launch (same fields the "Quick safety check" dialog writes), since the sandbox — not the folder prompt — is the trust boundary here.
- `claudesettings.go` — reads `.claude/settings.json` env into the host process (so runclaude sees `CLAUDE_CODE_USE_BEDROCK` etc. without the user exporting them), and `materializeClaudeSettings` overlays a sanitized copy into the container with all AWS env stripped (the in-container claude must not do its own AWS auth).
- `claudemerge.go` — handles the case where the **project's** `.claude/settings.json` carries AWS-related env. Rewriting it in place fights with VCS (jj/git keep restoring the original), so when `projectSettingsHasAWS(cwd)` is true, `buildMergedClaudeDir` builds a fresh merged user+project `.claude/` tree under `<cacheDir>/claude-merged/`: user tree first (skipping credential files), project tree overlaid on top (project wins on path collisions including subdirs like `skills/`, `agents/`, `commands/`), then `settings.json` is deep-merged with AWS env keys and `awsAuthRefresh` stripped. The merged dir is bind-mounted as `$HOME/.claude` inside the container, claude is launched with `--setting-sources user` so it ignores the project's `.claude/`, and the in-place sanitize + VCS-exclude paths are skipped (no host-side mutation, nothing to hide from VCS). For projects without AWS fields the existing in-place sanitize path is unchanged.
- `gitconfig.go` — `materializeGitConfig` copies the user's `~/.gitconfig` into the container's cache home with credential helpers / userinfo URLs stripped.
- `vcs.go` — `vcsExcludeBinds` overlays per-clone git/jj `info/exclude` with extra patterns (e.g. `.claude/settings.json`) so claude's runtime mutations don't show up as repo changes. Only used on the non-merged path; the merged path doesn't touch the host project file at all.
- `workspaceBinds` (main.go) — detects linked git worktrees (`.git` file → `gitdir:`) and jj workspaces (`.jj/repo` → repo path), binds the underlying repo storage so VCS works inside the container.

### Session recording (`record.go`, `recordgit.go`, `recordcmd.go`)

Recording — enabled by the `new`/`resume` subcommands or `record: true` in `.claude/runclaude.json` (there is no `--record` flag) — correlates Claude Code transcript positions with worktree snapshots so sessions can be reviewed as "conversation + tree state" (design: `docs/session-recording.md`). Sessions are **repo-scoped, not cwd-scoped**: all worktrees of a clone share the main worktree's `~/.claude/projects/` dir, bind-mounted inside the container over the current cwd's encoded dir (`repoSessionsDir`), so any worktree can list and `--resume` the clone's sessions; recorder startup also re-materializes missing transcripts from the session branch (purged by claude's cleanup, or fetched via `download`). A host-side goroutine in `mainErr` tails the session JSONL and on each mutating tool result commits the worktree (private index; host `git` required only when recording) at `refs/runclaude/<sid>/tree/<utc-ts>/<msg-uuid>`, coalescing same-second events. A parentless `refs/runclaude/<sid>/meta` session branch holds `meta.json` (write-once discovery metadata: author, recorder id, start, first prompt) plus the transcript truncated at the parse offset per flush — so the tip blob's size IS the crash-resume offset and every committed transcript ends on a complete JSONL line. Stickiness — once recorded, later plain runs keep recording — is scoped by a per-clone recorder id (random, minted into the common git dir, never transferred by clone/fetch): it survives repo moves, and a reviewer's clone can never match the author's, so only the recording clone extends a session. Recorder state + per-sid flocks live in `<git-common-dir>/runclaude/record/` (clone-global, so the flock also excludes double-recording across worktrees); a recorder claims a session only when the transcript's latest entries carry its own worktree's `cwd`, and hands off mid-session (flush, release flock) when entries start coming from another worktree — that worktree's recorder resumes at the blob offset. Resuming under `--claude` auto-appends `--fork-session` when the session's recorder id is not this clone's (`autoForkSession`); same-clone sessions continue and keep recording. Sessions are managed by subcommands (first argument; `dispatchSubcommand` in recordcmd.go): `new` (sandbox path with recording forced on), `resume [<sid>]` (same, injecting `--resume` into the claude command; without an id — or with `latest` — it resolves the newest recorded session), and the host-only `list`, `upload <dest>` (bundle bounded by the earliest merge-base to `--upstream`, or push refs to a remote), `download <src>` (fetch + `git worktree add` + materialize transcript into the repo-scoped dir; replay with `runclaude resume <sid>`, forking is automatic), `rm <sid>...`. `runclaude -- <verb>` escapes a sandbox command named like a verb. jj repos record too: `resolveRecordGitDir` falls back to the jj git backend store (`resolveJJGitStore`, which follows the `.jj/repo` pointer file of linked jj workspaces), covering colocated repos, pure-jj repos (store at `.jj/repo/store/git`; checkpoints are rootless there — git HEAD is unborn), and `jj workspace add` dirs; snapshots always exclude `.jj/` (linked workspaces have no `.jj/.gitignore`). jj ignores the `refs/runclaude/` namespace, so recording is invisible to normal VCS use — including the op log: when host `jj` is available (`recordjj.go`), checkpoints are native jj changes without ever creating an operation. Per checkpoint the recorder resolves `@`'s change id via `jj log --ignore-working-copy` (op-free, and stale-workspace-safe: a wc commit rewritten from another workspace keeps its change id); recorder-authored commits carry it in a `change-id` commit header (`commitTreeChangeID` assembles the raw object — jj round-trips the header on import), and when jj's own snapshot already matches the worktree the checkpoint ref reuses the jj-authored wc commit verbatim. jj failures log once and degrade to plain git checkpoints.

### Cache layout

Per-cwd cache at `~/.cache/runclaude/<sha256(cwd)[:16]>/` with subdirs `home/`, `tmp/`, `run/`, `vcs/`, `claude/`, `claude-merged/`. These are bind-mounted to `$HOME`, `/tmp`, `/run` inside the container — so build/module/auth caches persist across sessions for the same working directory. `claude-merged/` is rebuilt from scratch on each invocation when the merged-`.claude/` path engages.

### Bedrock path

When `--claude` + `CLAUDE_CODE_USE_BEDROCK=1`: the host calls `awsconfig.LoadDefaultConfig` (so SSO / profile / IMDS all work on the host), keeps the creds, and the in-container claude is given stub `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`. The container's aws-sdk signs requests with the stub; the MITM strips the stub signature and re-signs with the real creds. `scrubAWSCreds` ensures no real AWS env leaks into the container.

### `fakeapi/` and `cmd/fake-anthropic/`

A fake Anthropic + Bedrock server used by both `go test` and `test-mitm.sh`. Validates credentials (so MITM injection is actually tested) and streams `FixedReply` ("I am a fake anthropic API server") back as SSE — tests grep for that string.
