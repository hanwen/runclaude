#!/bin/bash

set -eu
TMP=$(mktemp -d)

# get AWS_PROFILE, AWS_REGION and CLAUDE_CODE_USE_BEDROCK
eval $(cat ~/.claude/settings.json | jq -r '.env | to_entries[] | "export \(.key)=\(.value | @sh)" ')

if [[ -z "$CLAUDE_CODE_USE_BEDROCK" ]]; then
    echo "not configured for bedrock." 
    exit
fi

go run ./ --proxy-log $TMP/proxy.log --claude-config \
   bash -xc './agent-claude-bedrock.sh' |& tee $TMP/log

grep '"role":"assistant"' $TMP/log
echo SUCCESS
