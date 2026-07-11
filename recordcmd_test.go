package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recordDemoSession drives the recorder through two checkpoints and
// returns the env.
func recordDemoSession(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.append(t,
		entryLine(t, "user", "u1", "add feature", nil),
		entryLine(t, "assistant", "u2", []map[string]any{
			{"type": "tool_use", "id": "t1", "name": "Write"},
		}, nil),
	)
	if err := os.WriteFile(filepath.Join(e.repo, "b.txt"), []byte("v1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e.append(t, entryLine(t, "user", "u3", []map[string]any{
		{"type": "tool_result", "tool_use_id": "t1"},
	}, nil))
	e.rec.scanOnce()

	e.clock = e.clock.Add(2 * time.Second)
	if err := os.WriteFile(filepath.Join(e.repo, "b.txt"), []byte("v2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e.append(t,
		entryLine(t, "assistant", "u4", []map[string]any{
			{"type": "tool_use", "id": "t2", "name": "Edit"},
		}, nil),
		entryLine(t, "user", "u5", []map[string]any{
			{"type": "tool_result", "tool_use_id": "t2"},
		}, nil),
	)
	e.rec.scanOnce()
	e.rec.Close()
	return e
}

func TestUploadDownloadRoundTrip(t *testing.T) {
	e := recordDemoSession(t)
	g := &gitRepo{gitDir: filepath.Join(e.repo, ".git")}

	// Upload as a bundle bounded by the local main branch.
	bundleDir := t.TempDir()
	if err := uploadSession(g, e.session, bundleDir, "main"); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(bundleDir, e.session+".bundle")
	if _, err := os.Stat(bundle); err != nil {
		t.Fatal(err)
	}

	// The bundle must not contain mainline history: its only prerequisite
	// is the base commit.
	out, err := g.run("bundle", "list-heads", bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "/meta") || !strings.Contains(out, "/tree/") {
		t.Errorf("bundle heads missing refs:\n%s", out)
	}

	// Download into a fresh clone (which has the prerequisite commit).
	cloneParent := t.TempDir()
	clone := filepath.Join(cloneParent, "clone")
	cmd := exec.Command("git", "clone", "-q", e.repo, clone)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	cg := &gitRepo{gitDir: filepath.Join(clone, ".git")}
	home2 := t.TempDir()
	dest := filepath.Join(cloneParent, "review")
	if err := downloadSession(cg, bundle, "", "", dest, clone, home2); err != nil {
		t.Fatal(err)
	}

	// Worktree has the latest checkpoint's content.
	data, err := os.ReadFile(filepath.Join(dest, "b.txt"))
	if err != nil || string(data) != "v2\n" {
		t.Errorf("worktree b.txt: %q, %v", data, err)
	}
	// Transcript materialized for claude --resume from the worktree cwd.
	destAbs, _ := filepath.Abs(dest)
	tfile := filepath.Join(transcriptDir(home2, destAbs), e.session+".jsonl")
	if _, err := os.Stat(tfile); err != nil {
		t.Errorf("transcript not materialized: %v", err)
	}
	// Foreign marker prevents the reviewer's recorder from extending the
	// author's refs.
	cacheDir, err := cacheDirFor(destAbs)
	if err != nil {
		t.Fatal(err)
	}
	stateData, err := os.ReadFile(filepath.Join(cacheDir, "record", e.session+".state"))
	if err != nil {
		t.Fatalf("foreign marker: %v", err)
	}
	var st recordState
	if json.Unmarshal(stateData, &st) != nil || !st.Foreign {
		t.Errorf("state not marked foreign: %s", stateData)
	}
	os.RemoveAll(cacheDir) // cacheDirFor points at the real user cache

	// --at selects an earlier checkpoint.
	dest2 := filepath.Join(cloneParent, "review-v1")
	if err := downloadSession(cg, bundle, e.session, "u3", dest2, clone, home2); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(dest2, "b.txt"))
	if err != nil || string(data) != "v1\n" {
		t.Errorf("--at u3 worktree b.txt: %q, %v", data, err)
	}
	destAbs2, _ := filepath.Abs(dest2)
	if cacheDir2, err := cacheDirFor(destAbs2); err == nil {
		os.RemoveAll(cacheDir2)
	}
}

func TestListAndRemoveSessions(t *testing.T) {
	e := recordDemoSession(t)
	g := &gitRepo{gitDir: filepath.Join(e.repo, ".git")}

	ids, err := sessionIDs(g)
	if err != nil || len(ids) != 1 || ids[0] != e.session {
		t.Fatalf("sessionIDs: %v, %v", ids, err)
	}
	sid, err := resolveSessionID(g, "latest")
	if err != nil || sid != e.session {
		t.Fatalf("resolveSessionID: %q, %v", sid, err)
	}

	if err := removeSession(g, e.session, e.repo); err != nil {
		t.Fatal(err)
	}
	refs, err := g.refNames(recordRefPrefix + e.session)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("refs remain after removal: %v", refs)
	}
}

func TestBundleBaseRefusesWithoutUpstream(t *testing.T) {
	e := recordDemoSession(t)
	g := &gitRepo{gitDir: filepath.Join(e.repo, ".git")}
	// No remotes and a bogus upstream: bundling must refuse, not emit an
	// unbounded bundle.
	_, err := bundleBase(g, e.session, "nonexistent-upstream")
	if err == nil {
		t.Error("bundleBase succeeded without a resolvable upstream")
	}
}
