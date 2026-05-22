#!/bin/bash

set -eux
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

go build -o "$TMP/runclaude" .

# Set up a fake HOME with a settings.json that contains AWS_PROFILE.
FAKEHOME="$TMP/home"
mkdir -p "$FAKEHOME/.claude"
cat > "$FAKEHOME/.claude/settings.json" <<'EOF'
{
  "env": {
    "AWS_PROFILE": "secret-profile",
    "SOME_OTHER_VAR": "keep-me"
  }
}
EOF

# Run inside the container with the fake HOME.
# Check that AWS_PROFILE is absent from the environment and from the
# settings.json that claude would read inside the container.
OUT=$(HOME="$FAKEHOME" "$TMP/runclaude" --claude-config -- bash -c '
  echo "=ENV="
  printenv
  echo "=SETTINGS="
  cat "$HOME/.claude/settings.json"
' 2>/dev/null)

ENV_SECTION=$(echo "$OUT" | sed -n '/^=ENV=/,/^=SETTINGS=/p')
SETTINGS_SECTION=$(echo "$OUT" | sed -n '/^=SETTINGS=/,$p')

if echo "$ENV_SECTION" | grep -q "AWS_PROFILE"; then
  echo "FAIL: AWS_PROFILE found in container environment"
  echo "$ENV_SECTION"
  exit 1
fi

if echo "$SETTINGS_SECTION" | grep -q "AWS_PROFILE"; then
  echo "FAIL: AWS_PROFILE found in container settings.json"
  echo "$SETTINGS_SECTION"
  exit 1
fi

if ! echo "$SETTINGS_SECTION" | grep -q "SOME_OTHER_VAR"; then
  echo "FAIL: SOME_OTHER_VAR missing from container settings.json (over-sanitized)"
  echo "$SETTINGS_SECTION"
  exit 1
fi

echo SUCCESS
