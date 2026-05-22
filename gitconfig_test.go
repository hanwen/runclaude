package main

import (
	"strings"
	"testing"
)

func TestSanitizeGitConfig(t *testing.T) {
	in := []byte(`[user]
	name = Han Wen
	email = hanwen@example.com
[core]
	editor = vim
[credential]
	helper = store
	username = secretuser
[credential "https://github.com"]
	helper = !/usr/bin/oauth-helper --token=hunter2
[http]
	extraHeader = Authorization: Bearer SECRETTOKEN
	cookieFile = /home/han/.git-cookies
	sslVerify = true
[http "https://corp.example.com/"]
	extraHeader = Authorization: Bearer CORPTOKEN
	sslVerify = true
[sendemail]
	smtpUser = alice
	smtpPass = hunter2
	smtpServer = smtp.example.com
[imap]
	host = imap.example.com
	user = alice
	pass = hunter2
[remote "origin"]
	url = https://alice:hunter2@github.com/han/repo.git
	pushurl = https://alice:hunter2@github.com/han/repo.git
	fetch = +refs/heads/*:refs/remotes/origin/*
[url "https://alice:hunter2@github.com/"]
	insteadOf = gh:
[alias]
	st = status
`)

	got, err := sanitizeGitConfig(in)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	gotStr := string(got)

	mustNotContain := []string{
		"SECRETTOKEN",
		"CORPTOKEN",
		"hunter2",
		"secretuser",
		"git-cookies",
		"oauth-helper",
		"[credential",
		"smtpPass",
		"smtpUser",
		"\tuser = alice",
		"\tpass = hunter2",
	}
	for _, s := range mustNotContain {
		if strings.Contains(gotStr, s) {
			t.Errorf("sanitized output still contains %q\n---\n%s", s, gotStr)
		}
	}

	mustContain := []string{
		"Han Wen",
		"hanwen@example.com",
		"editor = vim",
		"sslVerify = true",
		"smtpServer = smtp.example.com",
		"host = imap.example.com",
		"fetch = +refs/heads/*:refs/remotes/origin/*",
		"st = status",
		// remote URL kept, userinfo stripped
		"url = https://github.com/han/repo.git",
		"pushurl = https://github.com/han/repo.git",
	}
	for _, s := range mustContain {
		if !strings.Contains(gotStr, s) {
			t.Errorf("sanitized output missing %q\n---\n%s", s, gotStr)
		}
	}
}

func TestSanitizeGitConfigEmpty(t *testing.T) {
	out, err := sanitizeGitConfig(nil)
	if err != nil {
		t.Fatalf("sanitize empty: %v", err)
	}
	if len(strings.TrimSpace(string(out))) != 0 {
		t.Errorf("expected empty output, got %q", out)
	}
}
