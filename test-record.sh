#!/usr/bin/env bash
# End-to-end test for session recording (--record): runs a fake "claude"
# inside the sandbox that appends transcript JSONL (as the real claude
# would, into the live-bound ~/.claude/projects/) while mutating the
# worktree, then asserts the host-side recorder produced checkpoint refs
# and exercises the --upload/--download round trip.
#
# Prerequisites: working unprivileged user namespaces, git. The claude CLI
# is NOT needed. For the pure-Go equivalent see record_test.go /
# recordcmd_test.go.
set -euo pipefail

cd "$(dirname "$0")"

if ! unshare -Ur true 2>/dev/null; then
    echo "skipping: unprivileged user namespaces unavailable"
    exit 0
fi

WORKDIR=$(mktemp -d)
SID="e2e-record-$(date +%s)-$$"

go build -o "$WORKDIR/runclaude" .

REPO="$WORKDIR/repo"
mkdir -p "$REPO"
GIT="git -C $REPO -c user.name=t -c user.email=t@t"
$GIT init -q -b main
echo base > "$REPO/a.txt"
$GIT add a.txt
$GIT commit -q -m 'initial'

# The transcript dir claude would use for this cwd (inside the container it
# is live-bound from the real ~/.claude, so the recorder sees writes).
ENC=$(printf %s "$REPO" | tr -c 'A-Za-z0-9' '-')
TDIR="$HOME/.claude/projects/$ENC"
TFILE="$TDIR/$SID.jsonl"
trap 'rm -rf "$WORKDIR" "$TDIR"' EXIT

# Fake-claude: two turns, each appends tool_use, mutates the tree, then
# appends the tool_result that triggers the snapshot. Lives inside the
# repo because the container cannot see host /tmp (so it also appears in
# the recorded snapshots; harmless).
cat > "$REPO/fake-claude.sh" <<EOF
set -e
mkdir -p "$TDIR"
echo '{"type":"user","uuid":"u1","message":{"content":"add feature"}}' >> "$TFILE"
echo '{"type":"assistant","uuid":"u2","message":{"content":[{"type":"tool_use","id":"t1","name":"Write"}]}}' >> "$TFILE"
echo v1 > b.txt
echo '{"type":"user","uuid":"u3","message":{"content":[{"type":"tool_result","tool_use_id":"t1"}]}}' >> "$TFILE"
sleep 1.5
echo '{"type":"assistant","uuid":"u4","message":{"content":[{"type":"tool_use","id":"t2","name":"Edit"}]}}' >> "$TFILE"
echo v2 > b.txt
echo '{"type":"user","uuid":"u5","message":{"content":[{"type":"tool_result","tool_use_id":"t2"}]}}' >> "$TFILE"
sleep 1.5
EOF

(cd "$REPO" && "$WORKDIR/runclaude" --record --claude-config --claude=false \
    --restrict-net=false --map-path=false -- bash fake-claude.sh)

fail() { echo "FAIL: $*" >&2; exit 1; }

refs=$($GIT for-each-ref --format='%(refname)' "refs/runclaude/$SID")
echo "$refs" | grep -q "/meta$" || fail "no session branch ref; got: $refs"
echo "$refs" | grep -q "/tree/.*/u3$" || fail "no u3 checkpoint; got: $refs"
echo "$refs" | grep -q "/tree/.*/u5$" || fail "no u5 checkpoint; got: $refs"

u5ref=$(echo "$refs" | grep "/tree/.*/u5$")
[[ "$($GIT show "$u5ref:b.txt")" == "v2" ]] || fail "u5 checkpoint content"
$GIT show "refs/runclaude/$SID/meta:transcript.jsonl" | grep -q '"uuid":"u5"' \
    || fail "transcript branch incomplete"

# Upload -> clone -> download round trip.
(cd "$REPO" && "$WORKDIR/runclaude" --upload "$WORKDIR" --session "$SID" --record-upstream main)
[[ -s "$WORKDIR/$SID.bundle" ]] || fail "bundle not written"

CLONE="$WORKDIR/clone"
git clone -q "$REPO" "$CLONE"
(cd "$CLONE" && "$WORKDIR/runclaude" --download "$WORKDIR/$SID.bundle" --dest "$WORKDIR/review")
[[ "$(cat "$WORKDIR/review/b.txt")" == "v2" ]] || fail "downloaded worktree content"

# The download materialized the transcript into the clone's repo-scoped
# sessions dir in the real home; clean it up.
rm -rf "$HOME/.claude/projects/$(printf %s "$CLONE" | tr -c 'A-Za-z0-9' '-')"
echo "PASS test-record.sh"
