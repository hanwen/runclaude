#!/bin/sh

set -eux
TMP=$(mktemp -d)

go run ./ --proxy-log $TMP/log bash -c 'curl https://api.anthropic.com ; curl https://slashdot.org; exit 0' 

grep 'deny *CONNECT slashdot.org' $TMP/log
grep 'allow CONNECT api.anthropic.com' $TMP/log

echo SUCCESS
