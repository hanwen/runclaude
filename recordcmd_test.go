package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	// Transcript materialized into the clone's repo-scoped sessions dir —
	// the review worktree sees it via the container bind, and its size must
	// equal the recorded blob (it is the resume offset).
	tfile := filepath.Join(transcriptDir(home2, clone), e.session+".jsonl")
	fi, err := os.Stat(tfile)
	if err != nil {
		t.Fatalf("transcript not materialized: %v", err)
	}
	tip := cg.readRef(recordRefPrefix + e.session + "/meta")
	if n, err := cg.blobSize(tip, "transcript.jsonl"); err != nil || n != fi.Size() {
		t.Errorf("materialized transcript %d bytes, blob %d (%v)", fi.Size(), n, err)
	}

	// The reviewer's clone resumes with an automatic --fork-session (its
	// recorder id cannot match the author's); the author's own clone does
	// not fork.
	resume := []string{"--resume", e.session}
	if got := autoForkSession(clone, resume); len(got) != 1 || got[0] != "--fork-session" {
		t.Errorf("autoForkSession in reviewer clone: %v", got)
	}
	if got := autoForkSession(e.repo, resume); got != nil {
		t.Errorf("autoForkSession in recording clone: %v", got)
	}
	if got := autoForkSession(clone, []string{"--resume", e.session, "--fork-session"}); got != nil {
		t.Errorf("autoForkSession with explicit flag: %v", got)
	}
	if got := autoForkSession(clone, []string{"--resume", "not-recorded"}); got != nil {
		t.Errorf("autoForkSession for unrecorded sid: %v", got)
	}

	// --at selects an earlier checkpoint.
	dest2 := filepath.Join(cloneParent, "review-v1")
	if err := downloadSession(cg, bundle, e.session, "u3", dest2, clone, home2); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(dest2, "b.txt"))
	if err != nil || string(data) != "v1\n" {
		t.Errorf("--at u3 worktree b.txt: %q, %v", data, err)
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

	if err := removeSession(g, e.session); err != nil {
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

func TestDispatchSubcommand(t *testing.T) {
	handled, record, sid, rest, err := dispatchSubcommand([]string{"new", "--", "-p", "hi"})
	if handled || !record || sid != "" || err != nil || !reflect.DeepEqual(rest, []string{"--", "-p", "hi"}) {
		t.Errorf("new: handled=%v record=%v sid=%q rest=%v err=%v", handled, record, sid, rest, err)
	}
	// An explicit resume id needs no repo; remaining args feed flag parsing.
	handled, record, sid, rest, err = dispatchSubcommand([]string{"resume", "abc", "--restrict-net=false"})
	if handled || !record || sid != "abc" || err != nil || !reflect.DeepEqual(rest, []string{"--restrict-net=false"}) {
		t.Errorf("resume abc: handled=%v record=%v sid=%q rest=%v err=%v", handled, record, sid, rest, err)
	}
	// Anything else passes through untouched to regular flag parsing —
	// including `--` escaping a command named like a verb.
	for _, args := range [][]string{nil, {"--record"}, {"--", "list"}, {"bash", "-c", "true"}} {
		handled, record, sid, rest, err := dispatchSubcommand(args)
		if handled || record || sid != "" || err != nil || !reflect.DeepEqual(rest, args) {
			t.Errorf("dispatchSubcommand(%v): handled=%v record=%v sid=%q rest=%v err=%v",
				args, handled, record, sid, rest, err)
		}
	}
}

func TestResumeSubcommandLatest(t *testing.T) {
	e := recordDemoSession(t)
	t.Chdir(e.repo)
	for _, args := range [][]string{nil, {"latest"}, {"--", "-p", "hi"}} {
		sid, _, err := resumeSubcommand(args)
		if err != nil || sid != e.session {
			t.Errorf("resumeSubcommand(%v): sid=%q err=%v, want %q", args, sid, err, e.session)
		}
	}

	t.Chdir(t.TempDir())
	if _, _, err := resumeSubcommand(nil); err == nil {
		t.Error("resumeSubcommand outside a repo succeeded")
	}
}

func TestSessionSubcommands(t *testing.T) {
	e := recordDemoSession(t)
	home := t.TempDir()

	bundleDir := t.TempDir()
	if err := runSessionSubcommand("upload", []string{"--upstream", "main", bundleDir}, e.repo, home); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(bundleDir, e.session+".bundle")
	if _, err := os.Stat(bundle); err != nil {
		t.Fatal(err)
	}

	cloneParent := t.TempDir()
	clone := filepath.Join(cloneParent, "clone")
	if out, err := exec.Command("git", "clone", "-q", e.repo, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	dest := filepath.Join(cloneParent, "review")
	if err := runSessionSubcommand("download", []string{"--dest", dest, bundle}, clone, home); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dest, "b.txt")); err != nil || string(data) != "v2\n" {
		t.Errorf("downloaded worktree b.txt: %q, %v", data, err)
	}

	if err := runSessionSubcommand("list", []string{"extra"}, e.repo, home); err == nil {
		t.Error("list accepted positional args")
	}
	if err := runSessionSubcommand("rm", nil, e.repo, home); err == nil {
		t.Error("rm accepted empty args")
	}
	if err := runSessionSubcommand("rm", []string{e.session}, e.repo, home); err != nil {
		t.Fatal(err)
	}
	g := &gitRepo{gitDir: filepath.Join(e.repo, ".git")}
	if refs, err := g.refNames(recordRefPrefix + e.session); err != nil || len(refs) != 0 {
		t.Errorf("refs remain after rm: %v, %v", refs, err)
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
