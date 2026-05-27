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

echo SUCCESS
