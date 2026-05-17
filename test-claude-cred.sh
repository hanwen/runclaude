#!/bin/bash

set -eux
TMP=$(mktemp -d)

go run ./ --inject-auth bash -xc './agent-claude-cred.sh' >& $TMP/log

grep '"role":"assistant"' $TMP/log
echo SUCCESS
