# runclaude session recording — design plan (rev 2, post-critique)

## Objective

Allow collaborative code review based on agent transcripts rather than generated code. Instead of reviewing only the final diff an AI agent produced, reviewers fetch the recorded session and step through what the agent did and why: each conversation event is correlated with a filesystem snapshot, so the unit of review becomes "transcript position + tree state", not a squashed patch.

## Requirements (fixed)

- `--record` enables recording. Sticky: once a session is marked recorded, it keeps recording on later resumes even without the flag.
- `--upload` uploads a session to local FS (bundle) or remote git. `--download` fetches and replays; download creates a new worktree.
- The transcript is NOT injected into the project tree. Tree checkpoints are commits at refs `{sessionid}/tree/{timestamp}/{msg-uuid}`; the transcript lives on a separate branch `{sessionid}/transcript` containing a single file.
- Multiple events within the same second are smushed into one transcript commit.
- Tree commits are based on the actual checkout; the earliest merge-base to the configured upstream is the `--not` when creating the bundle.

## Data model (git objects, in the project's own git dir)

All refs under `refs/runclaude/` in the repo from `resolveGitDir(cwd)`. v1 supports git and colocated-jj repos; pure-jj (bare store at `.jj/repo/store/git`) is deferred (needs `resolveGitDir` extension + a jj-aware base rule).

- **Tree checkpoints**: `refs/runclaude/<sid>/tree/<ts>/<msg-uuid>`, timestamp UTC `20260710T130501Z`. Snapshots are taken **immediately on seeing a mutating `tool_result`** (Bash/Edit/Write/NotebookEdit) and at turn-final text entries — not on the coalescing clock — to keep message↔tree correlation tight. Each checkpoint commit message records the transcript byte offsets spanned since the previous checkpoint, so a viewer can flag windows where background processes may have interleaved. If the tree hash is unchanged, no commit; refs for skipped uuids are not created (the transcript branch still covers them).
- **Parents**: first checkpoint's parent = HEAD at session start (unborn HEAD ⇒ parentless). Subsequent = previous checkpoint; the current HEAD is added as a second parent **only when it is not already reachable from the checkpoint chain** (avoids jj's per-command HEAD churn spraying merge parents; still pins genuinely new base commits for bundle math).
- **Snapshot mechanics**: `git add -A` + `write-tree` with `GIT_DIR`, `GIT_WORK_TREE=cwd`, and a **persistent private index** at `<cacheDir>/record/<sid>.index` (user's index untouched; incremental restats across flushes; enable untracked-cache). Exclusions: the repo's own `info/exclude` applies via the real `GIT_DIR`; runclaude's extra patterns (e.g. `.claude/settings.json` — note the container-side `vcsExcludeBinds` overlay is invisible to the host recorder) are applied via `git -c core.excludesFile=<cache>/vcs/exclude`, plus a user-configurable `recordExclude` pattern list for untracked secrets (`.env` etc.).
- **Transcript branch**: `refs/runclaude/<sid>/transcript` — linear, parentless root, single file `transcript.jsonl`, blob = full session file. One commit per coalesced batch (same-second events smushed); commit message lists covered msg-uuids **and the file offset/blob hash of the tip**, making flushes idempotent after a crash (restart compares tip metadata, not the state file). On session end: `git pack-refs --all --prune` for the runclaude namespace + `git repack -d -q` to collapse the per-batch full-blob loose objects into deltas.
- **Session metadata**: `refs/runclaude/<sid>/meta` — tiny commit with one `meta.json`: author, start time, cwd, upstream base, first user prompt (truncated). This is what discovery lists, and what scopes stickiness.

## Recorder (new `record.go`, goroutine in `mainErr`)

1. Poll (~300ms stat) `~/.claude/projects/<encoded-cwd>/*.jsonl`; on a mutating event, snapshot immediately (see above). Detect file shrink/inode change and re-baseline instead of tailing garbage (Claude Code can rewrite/fork session files on resume/compaction).
2. **Ownership**: per-sid `flock` on `<cacheDir>/record/<sid>.state`; a recorder only claims sids it can lock, and by default only sids whose file appeared after this runclaude started (multiple runclaude sessions in one cwd — a case session.go already warns about — must not double-commit or race `update-ref`).
3. State file holds offset/tips as a cache only; authoritative resume data lives in the transcript tip commit message (see above). Missing state + existing refs ⇒ rebuild and continue. On startup, clear a stale private-index `.lock` after a liveness check.
4. Git ops shell out to host `git` (dependency only when recording). If a snapshot takes longer than the coalescing window (huge worktrees), degrade to adaptive intervals rather than falling behind indefinitely.

## Stickiness

Record if `--record` was given OR `refs/runclaude/<sid>/transcript` exists **and** the meta ref's recorded cwd matches (linked worktrees share the refs store; without the cwd scope, recording in worktree A silently enables it in worktree B). Corrected claim: custom refs do **not** travel with `git clone`/default fetch — stickiness survives cache wipes and explicit ref fetches, nothing more. Sessions fetched via `--download` are marked foreign in their state file and are never extended by the local recorder.

## CLI + Config

- `--record` (bool), `--record-upstream` (json `recordUpstream`; default: resolve `refs/remotes/origin/HEAD`, falling back to `origin/master`), `recordExclude` (json list).
- `--sessions` — list recorded sessions (local or `--remote`), from meta refs: sid, author, date, first prompt.
- `--upload <dest> [--session <sid>|latest]`, `--download <src> [--session <sid>] [--at <uuid|ts>] [--dest <dir>]`, `--record-rm <sid>` (delete refs + state; print gc guidance). All host-only modes, handled before sandbox setup.

**Upload**: dest = path ⇒ `git bundle create <dest>/<sid>.bundle --glob='refs/runclaude/<sid>/*' --not <base>`; base = earliest `git merge-base <upstream> <p>` over recorded checkout parents (derivable from the checkpoint DAG). No merge-base (orphan/missing upstream) ⇒ refuse with a clear message and offer `--not <session-start-HEAD>` or a standalone bundle. dest = remote/URL ⇒ push `refs/runclaude/<sid>/*`. Before either: print a report of blobs in the recording that were never in any user commit ("these untracked files will be uploaded") + the standing transcript-sensitivity warning. Document: prefer a dedicated review remote — per-uuid refs are cheap locally (packed) but every ref is advertised on fetch to all users of a shared remote.

**Download**: fetch into `refs/runclaude/*` (`git bundle verify` first; on missing prerequisites, tell the user to fetch the upstream). `git worktree add --detach <dir> <tree commit>` (default latest; `--at` by uuid/time). Materialize the transcript into `~/.claude/projects/<encoded-worktree-path>/<sid>.jsonl`, mark the sid foreign, and print the replay command using `claude --resume <sid> --fork-session` so the replayed conversation gets a fresh sid — continuing the original sid would write onto the author's refs and produce push collisions. Risk: JSONL embeds the original cwd; if resume balks, replay degrades to inspection-only (worktree + transcript still reviewable).

## Review workflow (v1 floor for the objective)

`--sessions` for discovery; checkpoint stepping is plain git: `git diff <ref-A> <ref-B>` between consecutive tree refs, `git show refs/runclaude/<sid>/transcript:transcript.jsonl` for the conversation. A guided `--step` viewer that walks uuid→ref→diff alongside transcript entries is v2, but the data model above is designed so it needs no new recorded state.

## Testing

- Unit: coalescer, ref naming, tolerant JSONL parsing, resume idempotency (crash between update-ref and state write), shrink/inode re-baseline, exclusion layering.
- Go e2e (no claude binary): temp repo + synthetic appends ⇒ ref layout, per-uuid tree contents, bundle round-trip incl. missing-prerequisite error path, worktree checkout, two-recorder flock exclusion.
- `test-record.sh`: real claude under runclaude with `--record`; upload → download → fork-resume.

## Deferred (v2+)

Pure-jj repos; `--step` review viewer; continuation-session stickiness linkage (new sid referencing prior sid); container freeze for crash-consistent snapshots; transcript scrubbing/encryption; retention policies beyond `--record-rm`.

Build order: recorder + refs + ownership locking → stickiness + meta → upload (incl. never-committed report) → download/replay → `--sessions` → shell test.

## Post-v1 revisions (implemented)

The sections above are the original plan; the implementation superseded parts of it (CLAUDE.md stays current):

- **`/transcript` folded into `/meta`**: one parentless session branch `refs/runclaude/<sid>/meta` with a two-file tree (`meta.json` + `transcript.jsonl`); no offset/hash in commit messages — the transcript blob is truncated at the parse offset, so the tip blob's size *is* the resume offset and every committed transcript ends on a complete JSONL line.
- **Stickiness by recorder id, not cwd**: `meta.json` carries a per-clone random id (`<git-common-dir>/runclaude-recorder-id`, never transferred by clone/fetch) instead of a checkout path. The `--download` foreign marker became redundant: a reviewer's clone can never hold the author's id.
- **Sessions are repo-scoped**: all worktrees of a clone share the main worktree's `~/.claude/projects/` dir via a container bind, so any worktree lists/resumes the clone's sessions; recorder startup re-materializes missing transcripts from the session branch (survives claude's 30-day purge). Recorder state + per-sid flocks moved to `<git-common-dir>/runclaude/record/` (clone-global locking); recorders claim by the transcript entries' `cwd` field and hand off mid-session when a session migrates to another worktree. Resume of a foreign-clone session auto-appends `--fork-session`; same-clone resume continues the session from any worktree.
