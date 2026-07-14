#!/usr/bin/env bash
# End-to-end test for session recording in jj repos: a session recorded in
# a colocated jj repo produces checkpoints that are amended versions of @
# (the user's description, change id, and parents — never stacked on each
# other), and `runclaude resume` in another workspace of the same repo
# materializes the recorded tree as a jj change. A second section covers
# the hybrid case: a session recorded in a plain git repo, restored into a
# workspace of the later-colocated repo.
#
# Prerequisites: working unprivileged user namespaces, git, jj. The claude
# CLI is NOT needed. For the pure-Go equivalent see recordjj_test.go.
set -euo pipefail

cd "$(dirname "$0")"

if ! unshare -Ur true 2>/dev/null; then
    echo "skipping: unprivileged user namespaces unavailable"
    exit 0
fi
if ! command -v jj >/dev/null 2>&1; then
    echo "skipping: jj not on PATH"
    exit 0
fi

WORKDIR=$(mktemp -d)
SID="e2e-record-jj-$(date +%s)-$$"

go build -o "$WORKDIR/runclaude" .

fail() { echo "FAIL: $*" >&2; exit 1; }

# fake_claude <repo> <transcript-file>: writes the fake-claude script that
# appends transcript JSONL (as the real claude would, into the live-bound
# ~/.claude/projects/) while mutating the worktree. Lives inside the repo
# because the container cannot see host /tmp.
fake_claude() {
    cat > "$1/fake-claude.sh" <<EOF
set -e
mkdir -p "$(dirname "$2")"
echo '{"type":"user","uuid":"u1","message":{"content":"add feature"}}' >> "$2"
echo '{"type":"assistant","uuid":"u2","message":{"content":[{"type":"tool_use","id":"t1","name":"Write"}]}}' >> "$2"
echo v1 > b.txt
echo '{"type":"user","uuid":"u3","message":{"content":[{"type":"tool_result","tool_use_id":"t1"}]}}' >> "$2"
sleep 1.5
echo '{"type":"assistant","uuid":"u4","message":{"content":[{"type":"tool_use","id":"t2","name":"Edit"}]}}' >> "$2"
echo v2 > b.txt
echo '{"type":"user","uuid":"u5","message":{"content":[{"type":"tool_result","tool_use_id":"t2"}]}}' >> "$2"
sleep 1.5
EOF
}

enc() { printf %s "$1" | tr -c 'A-Za-z0-9' '-'; }

RUNCLAUDE_FLAGS=(--claude-config --claude=false --restrict-net=false --map-path=false)

## Native jj: record in a colocated repo.
REPO="$WORKDIR/repo"
mkdir -p "$REPO"
(cd "$REPO" && jj git init --colocate)
JJ="jj -R $REPO"
GIT="git -C $REPO"
echo base > "$REPO/a.txt"
$JJ commit -m 'initial'
$JJ desc -m 'add feature b'

TDIR="$HOME/.claude/projects/$(enc "$REPO")"
TDIR2="$HOME/.claude/projects/$(enc "$WORKDIR/repo-2")"
trap 'rm -rf "$WORKDIR" "$TDIR" "$TDIR2"' EXIT
fake_claude "$REPO" "$TDIR/$SID.jsonl"

(cd "$REPO" && "$WORKDIR/runclaude" new "${RUNCLAUDE_FLAGS[@]}" -- bash fake-claude.sh)

refs=$($GIT for-each-ref --format='%(refname)' "refs/runclaude/$SID")
u3ref=$(echo "$refs" | grep "/tree/.*/u3$") || fail "no u3 checkpoint; got: $refs"
u5ref=$(echo "$refs" | grep "/tree/.*/u5$") || fail "no u5 checkpoint; got: $refs"
[[ "$($GIT show "$u5ref:b.txt")" == "v2" ]] || fail "u5 checkpoint content"

# Checkpoints are amended versions of @: its description, its change id in
# the commit header, and its parents — u3 and u5 share the parent instead
# of stacking.
u3sha=$($GIT rev-parse "$u3ref")
u5sha=$($GIT rev-parse "$u5ref")
[[ "$($GIT log -1 --format=%s "$u5sha")" == "add feature b" ]] || fail "checkpoint lacks the jj description"
cid=$($JJ log --no-graph --ignore-working-copy -r @ -T change_id)
$GIT cat-file commit "$u5sha" | grep -q "^change-id $cid$" || fail "checkpoint lacks @'s change id"
initial=$($JJ log --no-graph --ignore-working-copy -r @- -T commit_id)
[[ "$($GIT log -1 --format=%P "$u3sha")" == "$initial" ]] || fail "u3 not amended onto @'s parent"
[[ "$($GIT log -1 --format=%P "$u5sha")" == "$initial" ]] || fail "u5 stacked instead of amended"

## Resume in a second workspace: the recorded tree comes back as a jj
## change on top of the checkpoint.
$JJ workspace add "$WORKDIR/repo-2"
JJ2="jj -R $WORKDIR/repo-2"
[[ -f "$WORKDIR/repo-2/b.txt" ]] && fail "fresh workspace already has b.txt"
(cd "$WORKDIR/repo-2" && "$WORKDIR/runclaude" resume "$SID" "${RUNCLAUDE_FLAGS[@]}" -- bash -c 'echo nop')
[[ "$(cat "$WORKDIR/repo-2/b.txt")" == "v2" ]] || fail "resume did not restore b.txt"
[[ "$($JJ2 log --no-graph --ignore-working-copy -r @- -T commit_id)" == "$u5sha" ]] || fail "@- is not the u5 checkpoint"
[[ "$($JJ2 log --no-graph --ignore-working-copy -r '@-' -T 'description.first_line()')" == "add feature b" ]] || fail "restored change lacks the description"
$JJ bookmark list | grep -q runclaude/restore && fail "leftover restore bookmark"
$GIT show-ref --verify -q refs/heads/runclaude/restore && fail "leftover restore git ref"

# A second resume is a no-op: the worktree is already at the checkpoint.
at=$($JJ2 log --no-graph --ignore-working-copy -r @ -T commit_id)
(cd "$WORKDIR/repo-2" && "$WORKDIR/runclaude" resume "$SID" "${RUNCLAUDE_FLAGS[@]}" -- bash -c 'echo nop')
[[ "$($JJ2 log --no-graph --ignore-working-copy -r @ -T commit_id)" == "$at" ]] || fail "second resume moved @"

## Hybrid: a session recorded in a plain git repo, restored into a jj
## workspace after the repo is colocated.
HREPO="$WORKDIR/hybrid"
HSID="$SID-hybrid"
mkdir -p "$HREPO"
HGIT="git -C $HREPO -c user.name=t -c user.email=t@t"
$HGIT init -q -b main
echo base > "$HREPO/a.txt"
$HGIT add a.txt
$HGIT commit -q -m 'initial'
$HGIT branch initial

HTDIR="$HOME/.claude/projects/$(enc "$HREPO")"
HTDIR2="$HOME/.claude/projects/$(enc "$WORKDIR/hybrid-2")"
trap 'rm -rf "$WORKDIR" "$TDIR" "$TDIR2" "$HTDIR" "$HTDIR2"' EXIT
fake_claude "$HREPO" "$HTDIR/$HSID.jsonl"

(cd "$HREPO" && "$WORKDIR/runclaude" new "${RUNCLAUDE_FLAGS[@]}" -- bash fake-claude.sh)

(cd "$HREPO" && jj git init --colocate && jj workspace add "$WORKDIR/hybrid-2")
(cd "$WORKDIR/hybrid-2" &&
    jj new initial &&
    test ! -f b.txt &&
    "$WORKDIR/runclaude" resume "$HSID" "${RUNCLAUDE_FLAGS[@]}" -- bash -c 'echo nop' &&
    test -f b.txt) || fail "hybrid resume did not restore b.txt"
[[ "$(cat "$WORKDIR/hybrid-2/b.txt")" == "v2" ]] || fail "hybrid restored content"

echo "PASS test-record-jj.sh"
