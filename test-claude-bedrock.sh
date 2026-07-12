#!/bin/bash

set -eu
TMP=$(mktemp -d)

# get AWS_PROFILE, AWS_REGION and CLAUDE_CODE_USE_BEDROCK
eval $(cat ~/.claude/settings.json | jq -r '.env | to_entries[] | "export \(.key)=\(.value | @sh)" ')

if [[ -z "$CLAUDE_CODE_USE_BEDROCK" ]]; then
    echo "not configured for bedrock." 
    exit
fi

go run ./ --test.proxy-log $TMP/proxy.log --claude -- -p 'Repeat precisely: "bonanzablub"' |& tee $TMP/claude.log

got="$(tail -1 $TMP/claude.log)"
if [[ "$got" != "bonanzablub" ]] ; then
    echo "got $got want bonanzablub"
    exit 2
fi

echo SUCCESS
