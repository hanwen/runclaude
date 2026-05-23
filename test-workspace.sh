#!/bin/sh

set -eux
TMP=$(mktemp -d)
trap 'rm -rf $TMP' EXIT

go build -o $TMP/runclaude

for tool in jj git ; do
    rm -rf $TMP/repo
    mkdir $TMP/repo
    cd $TMP/repo
    rm -rf ../ws
    case "$tool" in
     git)
	 git init
	 echo hello > file1.txt
	 git add file1.txt
	 git commit  -m message
	 git worktree add ../ws
	 (cd ../ws; $TMP/runclaude -- bash -c 'git --no-pager show HEAD')
         ;;
     jj)
	 jj git init --colocate
	 echo hello > file1.txt
	 jj commit -m message
	 jj workspace add ../ws
	 (cd ../ws; $TMP/runclaude -- bash -c 'jj status | cat')
    esac
done     
