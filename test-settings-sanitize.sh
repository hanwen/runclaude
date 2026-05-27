#!/bin/bash

set -eux
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

go build -o "$TMP/runclaude" .
RUNCLAUDE="$TMP/runclaude"

# Helper: run the captured bash snippet inside the container (--claude-config
# --claude=false so runclaude skips spawning the real claude). The snippet
# must emit a single JSON object on stdout; we return that to the caller.
run_in_container() {
  local home=$1 cwd=$2 snippet=$3
  (cd "$cwd" && env -u CLAUDE_CODE_USE_BEDROCK -u AWS_PROFILE \
    HOME="$home" "$RUNCLAUDE" --claude-config --claude=false -- \
    bash -c "$snippet") 2>/dev/null
}

# ---- Case 1: AWS field in the user-level ~/.claude/settings.json ----
# Existing behavior: an overlay sanitizes it on top; host file is untouched
# but the container sees a copy without AWS_PROFILE.
FAKEHOME="$TMP/home1"
mkdir -p "$FAKEHOME/.claude"
cat > "$FAKEHOME/.claude/settings.json" <<'EOF'
{
  "env": {
    "AWS_PROFILE": "secret-profile",
    "SOME_OTHER_VAR": "keep-me"
  }
}
EOF

OUT=$(run_in_container "$FAKEHOME" "$PWD" '
  printenv > /tmp/env.txt
  jq -n \
    --rawfile settings "$HOME/.claude/settings.json" \
    --rawfile env /tmp/env.txt \
    "{settings: \$settings, env: \$env}"
')

if jq -e '.env | test("(^|\n)AWS_PROFILE=")' <<<"$OUT" >/dev/null; then
  echo "case1 FAIL: AWS_PROFILE found in container environment"
  jq -r '.env' <<<"$OUT"
  exit 1
fi
if jq -e '.settings | fromjson | .env | has("AWS_PROFILE")' <<<"$OUT" >/dev/null; then
  echo "case1 FAIL: AWS_PROFILE found in container settings.json"
  jq -r '.settings' <<<"$OUT"
  exit 1
fi
if ! jq -e '.settings | fromjson | .env.SOME_OTHER_VAR == "keep-me"' <<<"$OUT" >/dev/null; then
  echo "case1 FAIL: SOME_OTHER_VAR missing from container settings.json (over-sanitized)"
  jq -r '.settings' <<<"$OUT"
  exit 1
fi

# ---- Case 2: AWS field in the project's <cwd>/.claude/settings.json ----
# New behavior: build a merged user+project .claude/ under the cache and
# bind it as $HOME/.claude. The project's host file must remain byte-for-byte
# unchanged (so VCS doesn't see a diff), and the container's view must have
# AWS keys stripped while preserving non-AWS user/project entries plus
# subdirectories like skills/.
FAKEHOME="$TMP/home2"
PROJ="$TMP/proj"
mkdir -p "$FAKEHOME/.claude/skills" "$PROJ/.claude/skills"

cat > "$FAKEHOME/.claude/settings.json" <<'EOF'
{
  "env": {
    "USER_KEEP": "u"
  }
}
EOF
echo "user-only-skill" > "$FAKEHOME/.claude/skills/user-only.md"
echo "user-shared"     > "$FAKEHOME/.claude/skills/shared.md"

cat > "$PROJ/.claude/settings.json" <<'EOF'
{
  "env": {
    "AWS_PROFILE": "proj-profile",
    "PROJ_KEEP": "p"
  }
}
EOF
echo "proj-only-skill" > "$PROJ/.claude/skills/proj-only.md"
echo "proj-shared"     > "$PROJ/.claude/skills/shared.md"

PROJ_BEFORE=$(cat "$PROJ/.claude/settings.json")

OUT=$(run_in_container "$FAKEHOME" "$PROJ" '
  jq -n \
    --rawfile settings "$HOME/.claude/settings.json" \
    --rawfile user_only "$HOME/.claude/skills/user-only.md" \
    --rawfile proj_only "$HOME/.claude/skills/proj-only.md" \
    --rawfile shared "$HOME/.claude/skills/shared.md" \
    "{settings: \$settings, skills: {user_only: \$user_only, proj_only: \$proj_only, shared: \$shared}}"
')

# Host project file unchanged.
PROJ_AFTER=$(cat "$PROJ/.claude/settings.json")
if [ "$PROJ_BEFORE" != "$PROJ_AFTER" ]; then
  echo "case2 FAIL: host project settings.json was modified"
  diff <(echo "$PROJ_BEFORE") <(echo "$PROJ_AFTER") || true
  exit 1
fi

# Container settings.json must scrub AWS_PROFILE and merge user+proj env.
if jq -e '.settings | fromjson | .env | has("AWS_PROFILE")' <<<"$OUT" >/dev/null; then
  echo "case2 FAIL: AWS_PROFILE found in merged container settings.json"
  jq -r '.settings' <<<"$OUT"
  exit 1
fi
if ! jq -e '.settings | fromjson | .env.USER_KEEP == "u"' <<<"$OUT" >/dev/null; then
  echo "case2 FAIL: USER_KEEP missing from merged settings"
  jq -r '.settings' <<<"$OUT"
  exit 1
fi
if ! jq -e '.settings | fromjson | .env.PROJ_KEEP == "p"' <<<"$OUT" >/dev/null; then
  echo "case2 FAIL: PROJ_KEEP missing from merged settings"
  jq -r '.settings' <<<"$OUT"
  exit 1
fi

# skills/ should be the union, with project winning on conflicts.
if ! jq -e '.skills.user_only | test("user-only-skill")' <<<"$OUT" >/dev/null; then
  echo "case2 FAIL: user-only skill missing from merged dir"
  exit 1
fi
if ! jq -e '.skills.proj_only | test("proj-only-skill")' <<<"$OUT" >/dev/null; then
  echo "case2 FAIL: project-only skill missing from merged dir"
  exit 1
fi
if ! jq -e '.skills.shared | test("proj-shared")' <<<"$OUT" >/dev/null; then
  echo "case2 FAIL: project should win on skills/shared.md conflict"
  jq -r '.skills.shared' <<<"$OUT"
  exit 1
fi

# ---- Case 3: project settings without AWS fields ----
# No merge path: existing in-place sanitize behavior applies, host file is
# preserved by VCS exclude, and the project's settings.json reaches the
# container as-is (claude reads it normally).
FAKEHOME="$TMP/home3"
PROJ3="$TMP/proj3"
mkdir -p "$FAKEHOME/.claude" "$PROJ3/.claude"
cat > "$PROJ3/.claude/settings.json" <<'EOF'
{
  "env": {
    "PROJ_NO_AWS": "1"
  }
}
EOF
PROJ3_BEFORE=$(cat "$PROJ3/.claude/settings.json")

run_in_container "$FAKEHOME" "$PROJ3" 'true' >/dev/null

PROJ3_AFTER=$(cat "$PROJ3/.claude/settings.json")
if [ "$PROJ3_BEFORE" != "$PROJ3_AFTER" ]; then
  echo "case3 FAIL: host project settings.json was modified despite no AWS fields"
  diff <(echo "$PROJ3_BEFORE") <(echo "$PROJ3_AFTER") || true
  exit 1
fi

# ---- Case 4: live-state entries are bind-mounted from ~/.claude, not copied ----
# On the merged path (project settings has AWS fields), history.jsonl,
# projects/, sessions/, etc. must be bind-mounted directly from the real
# ~/.claude rather than copied into the ephemeral merged dir. Two things to
# verify: (a) pre-existing content is visible inside the container, and (b)
# writes from inside the container land in the real ~/.claude and survive
# across invocations (i.e. the RemoveAll of the merged dir doesn't lose them).
FAKEHOME="$TMP/home4"
PROJ4="$TMP/proj4"
mkdir -p "$FAKEHOME/.claude/projects" "$PROJ4/.claude"

# Project settings with AWS so the merged path fires.
cat > "$PROJ4/.claude/settings.json" <<'EOF'
{
  "env": {
    "AWS_PROFILE": "proj4-profile"
  }
}
EOF

# Pre-seed history.jsonl and a projects/ entry.
echo '{"role":"user","content":"pre-existing"}' > "$FAKEHOME/.claude/history.jsonl"
echo "pre-existing-project" > "$FAKEHOME/.claude/projects/old.txt"

# First invocation: verify pre-existing content is readable and append a new entry.
run_in_container "$FAKEHOME" "$PROJ4" '
  cat "$HOME/.claude/history.jsonl"
  echo "{\"role\":\"user\",\"content\":\"new-entry\"}" >> "$HOME/.claude/history.jsonl"
  echo "new-project" > "$HOME/.claude/projects/new.txt"
' >/dev/null

# Pre-existing history line must still be in the host file.
if ! grep -q "pre-existing" "$FAKEHOME/.claude/history.jsonl"; then
  echo "case4 FAIL: pre-existing history.jsonl content lost"
  cat "$FAKEHOME/.claude/history.jsonl"
  exit 1
fi

# Write from inside the container must have landed in the host file.
if ! grep -q "new-entry" "$FAKEHOME/.claude/history.jsonl"; then
  echo "case4 FAIL: history.jsonl write from container not persisted to ~/.claude"
  cat "$FAKEHOME/.claude/history.jsonl"
  exit 1
fi

# File created in projects/ inside container must be in the real projects/.
if [ ! -f "$FAKEHOME/.claude/projects/new.txt" ]; then
  echo "case4 FAIL: projects/ write from container not persisted to ~/.claude"
  ls "$FAKEHOME/.claude/projects/"
  exit 1
fi

# Second invocation: the merged dir is rebuilt (RemoveAll), but the host
# history.jsonl must still contain both lines — proving we don't lose
# data written in the first invocation.
OUT2=$(run_in_container "$FAKEHOME" "$PROJ4" 'cat "$HOME/.claude/history.jsonl"')
if ! echo "$OUT2" | grep -q "pre-existing"; then
  echo "case4 FAIL: pre-existing history lost after second invocation (merged dir RemoveAll)"
  echo "$OUT2"
  exit 1
fi
if ! echo "$OUT2" | grep -q "new-entry"; then
  echo "case4 FAIL: first-invocation history lost after second invocation (merged dir RemoveAll)"
  echo "$OUT2"
  exit 1
fi

echo SUCCESS
