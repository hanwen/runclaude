#!/bin/bash

set -eux
go test ./...

for f in ./test-network.sh \
	     ./test-pathmap.sh \
	     ./test-workspace.sh \
	     ./test-settings-sanitize.sh \
	 ./test-mitm.sh ; do
    bash $f
done

if [[ -e ~/.claude/.credentials.json. ]]; then
    bash test-claude-cred.sh
fi

if [[ -e ~/.aws ]]; then
    bash test-bedrock.sh
    bash test-claude-bedrock.sh
fi 
