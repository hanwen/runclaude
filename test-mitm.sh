#!/usr/bin/env bash
# End-to-end test for the MITM path: runs the fake-anthropic server, then
# invokes `claude -p` inside runclaude with the proxy pointed at the fake
# server, and asserts the fixed reply appears in claude's output.
#
# Tests both authentication paths:
#   1. Anthropic API key (via $ANTHROPIC_API_KEY)
#   2. AWS Bedrock SigV4 (via $CLAUDE_CODE_USE_BEDROCK=1)
#
# Prerequisites:
#   - The `claude` CLI installed and on $PATH.
#   - Working unprivileged user namespaces (newuidmap setuid-installed,
#     /etc/subuid + /etc/subgid entries for $USER).
#
# For the deterministic, sandboxed equivalent that exercises the same
# MITM + signing + fake-server stack without launching a container, see
# TestMitmE2EAnthropic / TestMitmE2EBedrock in `go test`.
set -euo pipefail

cd "$(dirname "$0")"

if ! command -v claude >/dev/null 2>&1; then
    echo "skipping: claude CLI not on PATH"
    exit 0
fi
if ! command -v newuidmap >/dev/null 2>&1; then
    echo "skipping: newuidmap not installed (required by runclaude)"
    exit 0
fi

# Extra flags forwarded to runclaude per case (set by callers below).
EXTRA_RC_FLAGS=()

EXPECTED="I am a fake anthropic API server"
WORKDIR=$(mktemp -d)
#trap 'rm -rf "$WORKDIR"; kill $(jobs -p) 2>/dev/null || true' EXIT

go build -o "$WORKDIR/runclaude" .
go build -o "$WORKDIR/fake-anthropic" ./cmd/fake-anthropic

run_case() {
    local name="$1"; shift
    # Space-separated list of hosts to route through the fake server. The
    # primary auth path uses the first one; the rest get the catch-all so
    # claude's incidental probes (control-plane, telemetry) don't deny.
    local upstreams_str="$1"; shift
    local claude_json="$1"; shift
    local upstreams=($upstreams_str)
    local fake_args=("$@")

    local portfile="$WORKDIR/$name.port"
    local out="$WORKDIR/$name.out"
    local proxylog="$WORKDIR/$name.proxy.log"
    : > "$proxylog"

    # Stream fake-anthropic stderr to our stderr with a tag prefix so the
    # interleaved logs are easy to read.
    "$WORKDIR/fake-anthropic" --addr 127.0.0.1:0 --port-file "$portfile" \
        "${fake_args[@]}" \
        > >(sed "s/^/[$name fake] /" >&2) \
        2> >(sed "s/^/[$name fake] /" >&2) &
    local fake_pid=$!

    for _ in $(seq 1 50); do
        [[ -s "$portfile" ]] && break
        sleep 0.1
    done
    local port=$(cat "$portfile")
    echo "[$name] fake-anthropic on 127.0.0.1:$port (pid $fake_pid)" >&2

    # Tail the proxy log live so a hang is visible.
    ( tail -F "$proxylog" 2>/dev/null | sed "s/^/[$name proxy] /" >&2 ) &
    local tail_pid=$!

    local errfile="$WORKDIR/$name.err"
    local rc_args=(--claude --proxy-log "$proxylog")
    for u in "${upstreams[@]}"; do
        rc_args+=(--mitm-upstream "$u=http://127.0.0.1:$port")
    done

    if [[ -n "$claude_json" ]]; then 
      mkdir -p $WORKDIR/project/.claude
      echo "$claude_json" > $WORKDIR/project/.claude/settings.json
    fi 
    set +e
    (cd $WORKDIR/project ; 
    "$WORKDIR/runclaude" "${rc_args[@]}" \
        ${EXTRA_RC_FLAGS[@]+"${EXTRA_RC_FLAGS[@]}"} \
        -- -p "say hi" \
        < /dev/null \
        > "$out" \
        2> "$errfile" )
    local rc=$?
    set -e
    kill "$fake_pid" "$tail_pid" 2>/dev/null || true
    wait "$fake_pid" 2>/dev/null || true

    echo "[$name] --- runclaude/claude stderr ---" >&2
    sed "s/^/[$name stderr] /" < "$errfile" >&2
    echo "[$name] --- claude stdout ---" >&2
    sed "s/^/[$name stdout] /" < "$out" >&2

    if [[ $rc -ne 0 ]]; then
        echo "[$name] FAIL: runclaude exited $rc" >&2
        return 1
    fi
    if ! grep -qF "$EXPECTED" "$out"; then
        echo "[$name] FAIL: expected output missing" >&2
        return 1
    fi
    echo "[$name] PASS" >&2
}

# --- Anthropic API key path ---
# Pass --anthropic-bearer to runclaude so it injects this token regardless
# of what's in the developer's ~/.claude/.credentials.json. claude's own
# login state stays untouched.
TEST_BEARER="sk-ant-oat01-fake-$$-$(date +%s)"
EXTRA_RC_FLAGS=(--anthropic-bearer "$TEST_BEARER")
run_case anthropic api.anthropic.com '{}' \
    --anthropic-api-key "$TEST_BEARER"
EXTRA_RC_FLAGS=()

# --- Bedrock SigV4 path ---
REGION="us-east-1"
ACCESS_KEY="AKIAFAKE$(date +%s)"
SECRET_KEY="$(head -c 32 /dev/urandom | base64 | tr -d /+= | head -c 30)"
CLAUDE_CODE_USE_BEDROCK=1 \
AWS_ACCESS_KEY_ID="$ACCESS_KEY" \
AWS_SECRET_ACCESS_KEY="$SECRET_KEY" \
AWS_REGION="$REGION" \
    run_case bedrock \
        "bedrock-runtime.$REGION.amazonaws.com bedrock.$REGION.amazonaws.com" \
        '{}' \
        --aws-access-key-id "$ACCESS_KEY" \
        --aws-secret-access-key "$SECRET_KEY" \
        --aws-region "$REGION"

# --- Bedrock SigV4 via repo .claude/settings.json + aws sso login flow ---
# Simulates: developer puts CLAUDE_CODE_USE_BEDROCK / AWS_PROFILE in
# .claude/settings.json's env, then runs `aws sso login --profile myprofile`
# before invoking claude. The SSO login produces cached creds that the AWS
# SDK exposes via the profile.
#
# We stand in for the SSO cache with a `credential_process` profile pointing
# at a script that prints the test's fake creds — same AWS SDK code path,
# no network. runclaude must:
#   1. Read settings.json env on the host (CLAUDE_CODE_USE_BEDROCK=1).
#   2. Resolve AWS_PROFILE=myprofile via the fake AWS_CONFIG_FILE.
#   3. Rewrite settings.json for the container so the in-container claude
#      doesn't try to use AWS_PROFILE itself (which would re-trigger creds
#      lookup and IMDS fallback).
REGION="us-east-1"
ACCESS_KEY="AKIAFAKE$(date +%s)"
SECRET_KEY="$(head -c 32 /dev/urandom | base64 | tr -d /+= | head -c 30)"

AWS_DIR="$WORKDIR/aws"
mkdir -p "$AWS_DIR"
CRED_SCRIPT="$AWS_DIR/sso-cred-process.sh"
cat > "$CRED_SCRIPT" <<EOF
#!/usr/bin/env bash
printf '{"Version":1,"AccessKeyId":"$ACCESS_KEY","SecretAccessKey":"$SECRET_KEY","Expiration":"2099-01-01T00:00:00Z"}\n'
EOF
chmod +x "$CRED_SCRIPT"

cat > "$AWS_DIR/config" <<EOF
[profile myprofile]
region = $REGION
credential_process = $CRED_SCRIPT
EOF

BEDROCK_SETTINGS=$(jq -c . <<EOF
{
  "awsAuthRefresh": "aws sso login --profile myprofile",
  "env": {
    "AWS_PROFILE": "myprofile",
    "AWS_REGION": "$REGION",
    "CLAUDE_CODE_USE_BEDROCK": "1"
  }
}
EOF
)
AWS_CONFIG_FILE="$AWS_DIR/config" \
    run_case \
        bedrock-settings \
        "bedrock-runtime.$REGION.amazonaws.com bedrock.$REGION.amazonaws.com" \
        "$BEDROCK_SETTINGS" \
        --aws-access-key-id "$ACCESS_KEY" \
        --aws-secret-access-key "$SECRET_KEY" \
        --aws-region "$REGION"

echo "all MITM e2e cases passed"
