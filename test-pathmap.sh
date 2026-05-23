#!/bin/sh

set -eux
TMP=$(mktemp -d)
mkdir $TMP/bin
echo '#!/bin/bash

echo blubblab' > $TMP/bin/test-path-map.sh
chmod +x $TMP/bin/test-path-map.sh
export PATH="$TMP/bin:$PATH"

go run ./ -- bash -c 'test-path-map.sh' |& tee $TMP/log
grep blubblab $TMP/log

go run ./ -- bash -c "ln -s /escape $TMP/bin/foo || true" |& tee $TMP/log
grep "Read-only file system" $TMP/log
