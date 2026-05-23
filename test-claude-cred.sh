#!/bin/bash

set -eux
TMP=$(mktemp -d)

go run ./ --claude-config bash -xc './agent-claude-cred.sh' >& $TMP/log

grep '"role":"assistant"' $TMP/log
echo SUCCESS
