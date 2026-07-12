package record

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEncodeProjectDir(t *testing.T) {
	got := encodeProjectDir("/home/user/my.project_x")
	want := "-home-user-my-project-x"
	if got != want {
		t.Errorf("encodeProjectDir: got %q, want %q", got, want)
	}
}

func TestRefTimestamp(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if got := refTimestamp("2026-07-05T05:55:22.026Z", now); got != "20260705T055522Z" {
		t.Errorf("refTimestamp: got %q", got)
	}
	if got := refTimestamp("garbage", now); got != "20260710T120000Z" {
		t.Errorf("refTimestamp fallback: got %q", got)
	}
}

func TestRecorderPartialLineExcludedFromBlob(t *testing.T) {
	e := newTestEnv(t)
	complete := entryLine(t, "user", "u1", "hello", nil)
	partial := `{"type":"user","uuid":"u2","message":` // torn write, no newline
	e.append(t, complete, partial)
	e.rec.scanOnce()

	// The committed transcript ends at the parse offset: the torn line is
	// not in the blob, and the blob size is the resume offset.
	got, err := e.rec.git.showBlob(recordRefPrefix+e.session+"/meta", "transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if got+"\n" != complete {
		t.Errorf("blob should hold exactly the complete line: %q", got)
	}
	tip := e.rec.git.readRef(recordRefPrefix + e.session + "/meta")
	n, err := e.rec.git.blobSize(tip, "transcript.jsonl")
	if err != nil || n != int64(len(complete)) {
		t.Errorf("blob size %d, want parse offset %d (%v)", n, len(complete), err)
	}

	// Completing the line later gets it recorded on the next flush.
	e.append(t, "{\"content\":\"done\"}}\n")
	e.clock = e.clock.Add(2 * time.Second)
	e.rec.scanOnce()
	got, err = e.rec.git.showBlob(recordRefPrefix+e.session+"/meta", "transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(e.transcript)
	if got+"\n" != string(want) {
		t.Errorf("completed line missing from blob: %d vs %d bytes", len(got), len(want))
	}
	e.rec.Close()
}

// testRepo initializes a git repo with one commit and returns its path.
func testRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-q", "-m", "initial")
	return dir
}

// entryLine builds one transcript JSONL line.
func entryLine(t *testing.T, typ, uuid string, content any, extra map[string]any) string {
	t.Helper()
	m := map[string]any{
		"type":      typ,
		"uuid":      uuid,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"message":   map[string]any{"content": content},
	}
	for k, v := range extra {
		m[k] = v
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

type testEnv struct {
	repo, home string
	rec        *Recorder
	session    string
	transcript string
	clock      time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	repo := testRepo(t)
	home := t.TempDir()
	rec, err := NewRecorder(filepath.Join(repo, ".git"), repo, home, true, "origin/main", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{
		repo:    repo,
		home:    home,
		rec:     rec,
		session: "11111111-2222-3333-4444-555555555555",
		clock:   time.Now(),
	}
	rec.now = func() time.Time { return env.clock }
	dir := TranscriptDir(home, repo)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	env.transcript = filepath.Join(dir, env.session+".jsonl")
	return env
}

func (e *testEnv) append(t *testing.T, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(e.transcript, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *testEnv) refs(t *testing.T) []string {
	t.Helper()
	refs, err := e.rec.git.refNames(recordRefPrefix + e.session)
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

func TestRecorderCheckpointsAndTranscript(t *testing.T) {
	e := newTestEnv(t)
	sid := e.session

	// Turn 1: user prompt, Write tool creates b.txt, tool_result confirms.
	e.append(t,
		entryLine(t, "user", "u1", "please add b", nil),
		entryLine(t, "assistant", "u2", []map[string]any{
			{"type": "tool_use", "id": "t1", "name": "Write", "input": map[string]any{"file_path": "b.txt"}},
		}, nil),
	)
	if err := os.WriteFile(filepath.Join(e.repo, "b.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e.append(t,
		entryLine(t, "user", "u3", []map[string]any{
			{"type": "tool_result", "tool_use_id": "t1"},
		}, map[string]any{"toolUseResult": map[string]any{}}),
	)
	e.rec.scanOnce()

	var treeRef string
	for _, ref := range e.refs(t) {
		if strings.Contains(ref, "/tree/") && strings.HasSuffix(ref, "/u3") {
			treeRef = ref
		}
	}
	if treeRef == "" {
		t.Fatalf("no tree ref for u3; refs: %v", e.refs(t))
	}
	if out, err := e.rec.git.showBlob(treeRef, "b.txt"); err != nil || out != "hello" {
		t.Errorf("checkpoint tree b.txt: %q, %v", out, err)
	}
	// Transcript branch has the full file.
	got, err := e.rec.git.showBlob(recordRefPrefix+sid+"/meta", "transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(e.transcript)
	if got+"\n" != string(want) {
		t.Errorf("transcript blob mismatch: %d vs %d bytes", len(got), len(want))
	}
	// Meta records this clone's recorder id and the first prompt.
	metaJSON, err := e.rec.git.showBlob(recordRefPrefix+sid+"/meta", "meta.json")
	if err != nil {
		t.Fatal(err)
	}
	var meta sessionMeta
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.RecorderID != e.rec.id || meta.RecorderID == "" || meta.FirstPrompt != "please add b" {
		t.Errorf("meta: %+v (recorder id %q)", meta, e.rec.id)
	}

	// The user's real index must be untouched: b.txt stays untracked.
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = e.repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" && !strings.HasPrefix(line, "??") {
			t.Errorf("user index modified: %q", line)
		}
	}

	// Turn 2 (same-second smush): two mutating results share one commit.
	e.clock = e.clock.Add(2 * time.Second)
	if err := os.WriteFile(filepath.Join(e.repo, "b.txt"), []byte("hello2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e.append(t,
		entryLine(t, "assistant", "u4", []map[string]any{
			{"type": "tool_use", "id": "t2", "name": "Edit"},
		}, nil),
		entryLine(t, "user", "u5", []map[string]any{
			{"type": "tool_result", "tool_use_id": "t2"},
		}, nil),
		entryLine(t, "assistant", "u6", []map[string]any{
			{"type": "tool_use", "id": "t3", "name": "Bash"},
		}, nil),
		entryLine(t, "user", "u7", []map[string]any{
			{"type": "tool_result", "tool_use_id": "t3"},
		}, nil),
	)
	e.rec.scanOnce()
	var u5Ref, u7Ref string
	for _, ref := range e.refs(t) {
		if strings.HasSuffix(ref, "/u5") {
			u5Ref = ref
		}
		if strings.HasSuffix(ref, "/u7") {
			u7Ref = ref
		}
	}
	if u5Ref == "" || u7Ref == "" {
		t.Fatalf("missing batch refs: %v", e.refs(t))
	}
	if e.rec.git.readRef(u5Ref) != e.rec.git.readRef(u7Ref) {
		t.Error("same-batch uuids should share one checkpoint commit")
	}
	// Second checkpoint's parent is the first.
	parent, err := e.rec.git.run("rev-parse", e.rec.git.readRef(u5Ref)+"^")
	if err != nil || parent != e.rec.git.readRef(treeRef) {
		t.Errorf("checkpoint chain broken: parent %q, want %q (%v)", parent, e.rec.git.readRef(treeRef), err)
	}
	e.rec.Close()
}

func TestRecorderStickyResume(t *testing.T) {
	e := newTestEnv(t)
	e.append(t,
		entryLine(t, "assistant", "u1", []map[string]any{
			{"type": "tool_use", "id": "t1", "name": "Write"},
		}, nil),
		entryLine(t, "user", "u2", []map[string]any{
			{"type": "tool_result", "tool_use_id": "t1"},
		}, nil),
	)
	os.WriteFile(filepath.Join(e.repo, "b.txt"), []byte("1\n"), 0644)
	e.rec.scanOnce()
	e.rec.Close()

	// New recorder without recordAll must resume via stickiness, from the
	// recorded offset (no duplicate transcript commits).
	rec2, err := NewRecorder(filepath.Join(e.repo, ".git"), e.repo, e.home, false, "", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now().Add(time.Hour)
	rec2.now = func() time.Time { return clock }
	tipBefore := rec2.git.readRef(recordRefPrefix + e.session + "/meta")

	os.WriteFile(filepath.Join(e.repo, "b.txt"), []byte("2\n"), 0644)
	e.append(t,
		entryLine(t, "assistant", "u3", []map[string]any{
			{"type": "tool_use", "id": "t2", "name": "Edit"},
		}, nil),
		entryLine(t, "user", "u4", []map[string]any{
			{"type": "tool_result", "tool_use_id": "t2"},
		}, nil),
	)
	rec2.scanOnce()
	if len(rec2.sessions) != 1 {
		t.Fatalf("sticky session not claimed; sessions: %d", len(rec2.sessions))
	}
	tipAfter := rec2.git.readRef(recordRefPrefix + e.session + "/meta")
	if tipAfter == tipBefore {
		t.Error("transcript tip did not advance on resume")
	}
	// Exactly one new transcript commit whose parent is the old tip.
	parent, err := rec2.git.run("rev-parse", tipAfter+"^")
	if err != nil || parent != tipBefore {
		t.Errorf("resume forked transcript: parent %q, want %q (%v)", parent, tipBefore, err)
	}
	found := false
	for _, ref := range e.refs(t) {
		if strings.HasSuffix(ref, "/u4") {
			found = true
		}
	}
	if !found {
		t.Errorf("no checkpoint for resumed session; refs: %v", e.refs(t))
	}
	rec2.Close()
}

func TestRecorderNotStickyForOtherClone(t *testing.T) {
	e := newTestEnv(t)
	e.append(t,
		entryLine(t, "assistant", "u1", []map[string]any{
			{"type": "tool_use", "id": "t1", "name": "Write"},
		}, nil),
		entryLine(t, "user", "u2", []map[string]any{
			{"type": "tool_result", "tool_use_id": "t1"},
		}, nil),
	)
	e.rec.scanOnce()
	e.rec.Close()

	// Simulate a different clone (fetched refs, materialized transcript,
	// different recorder id): the session must not be sticky there.
	idPath := filepath.Join(e.repo, ".git", "runclaude-recorder-id")
	if err := os.WriteFile(idPath, []byte("other-clone\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rec2, err := NewRecorder(filepath.Join(e.repo, ".git"), e.repo, e.home, false, "", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	rec2.scanOnce()
	if len(rec2.sessions) != 0 {
		t.Error("session sticky in a clone with a different recorder id")
	}
}

func TestRecorderLockExcludesSecond(t *testing.T) {
	e := newTestEnv(t)
	e.append(t, entryLine(t, "user", "u1", "hi", nil))
	e.rec.scanOnce()
	if len(e.rec.sessions) != 1 {
		t.Fatalf("first recorder did not claim: %d", len(e.rec.sessions))
	}
	// Same cacheDir (same cwd) — second recorder must not claim the locked session.
	rec2, err := NewRecorder(filepath.Join(e.repo, ".git"), e.repo, e.home, true, "", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	rec2.scanOnce()
	if len(rec2.sessions) != 0 {
		t.Error("second recorder claimed a locked session")
	}
	e.rec.Close()
}

func TestRecorderIgnoresPreexistingSessions(t *testing.T) {
	e := newTestEnv(t)
	// A file that predates the recorder and has no refs: not claimed even
	// with recordAll.
	e.append(t, entryLine(t, "user", "u1", "old", nil))
	rec2, err := NewRecorder(filepath.Join(e.repo, ".git"), e.repo, e.home, true, "", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	rec2.scanOnce()
	if len(rec2.sessions) != 0 {
		t.Error("claimed a session that predates the recorder")
	}
}

// addWorktree creates a linked git worktree of e.repo and returns its path.
func addWorktree(t *testing.T, repo string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	cmd := exec.Command("git", "-C", repo, "worktree", "add", "-q", "--detach", wt)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v: %s", err, out)
	}
	return wt
}

func TestRecorderRepoScopedSessionsDir(t *testing.T) {
	e := newTestEnv(t)
	wt := addWorktree(t, e.repo)
	rec2, err := NewRecorder(filepath.Join(wt, ".git"), wt, e.home, true, "", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	// Both worktrees watch the main worktree's project dir and share the
	// clone-global state dir (per-sid flocks exclude across worktrees).
	if rec2.ProjectsDir != e.rec.ProjectsDir {
		t.Errorf("projectsDir differs across worktrees: %q vs %q", rec2.ProjectsDir, e.rec.ProjectsDir)
	}
	if rec2.stateDir != e.rec.stateDir {
		t.Errorf("stateDir differs across worktrees: %q vs %q", rec2.stateDir, e.rec.stateDir)
	}
	if rec2.id != e.rec.id {
		t.Errorf("recorder id differs across worktrees: %q vs %q", rec2.id, e.rec.id)
	}
}

// addJJWorkspace fabricates jj metadata (no jj binary needed): it marks
// repo as a colocated jj main workspace and returns a sibling linked
// workspace whose .jj/repo is the pointer file `jj workspace add` writes,
// holding the main repo dir's path relative to the workspace's .jj.
func addJJWorkspace(t *testing.T, repo string) string {
	t.Helper()
	store := filepath.Join(repo, ".jj", "repo", "store")
	if err := os.MkdirAll(store, 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(store, "type"):             "git\n",
		filepath.Join(store, "git_target"):       "../../../.git",
		filepath.Join(repo, ".jj", ".gitignore"): "/*\n",
	}
	ws := filepath.Join(filepath.Dir(repo), "jjws")
	if err := os.MkdirAll(filepath.Join(ws, ".jj", "working_copy"), 0755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Join(ws, ".jj"), filepath.Join(repo, ".jj", "repo"))
	if err != nil {
		t.Fatal(err)
	}
	files[filepath.Join(ws, ".jj", "repo")] = rel
	files[filepath.Join(ws, ".jj", "working_copy", "tree_state")] = "opaque jj state"
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func TestRecorderInJJWorkspace(t *testing.T) {
	e := newTestEnv(t)
	ws := addJJWorkspace(t, e.repo)

	gitDir, ok := ResolveRecordGitDir(ws)
	if !ok || gitDir != filepath.Join(e.repo, ".git") {
		t.Fatalf("ResolveRecordGitDir(%s) = %q, %v; want the main workspace's .git", ws, gitDir, ok)
	}
	rec2, err := NewRecorder(gitDir, ws, e.home, true, "", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer rec2.Close()
	// Sessions and state are shared with recorders in the main workspace.
	if rec2.ProjectsDir != e.rec.ProjectsDir {
		t.Errorf("projectsDir differs: %q vs %q", rec2.ProjectsDir, e.rec.ProjectsDir)
	}
	if rec2.id != e.rec.id {
		t.Errorf("recorder id differs: %q vs %q", rec2.id, e.rec.id)
	}

	// A checkpoint recorded from the workspace snapshots the workspace's
	// tree and never ingests its .jj/ state (jj writes no .jj/.gitignore in
	// linked workspaces).
	sid := "99999999-8888-7777-6666-555555555555"
	tf := filepath.Join(rec2.ProjectsDir, sid+".jsonl")
	lines := entryLine(t, "assistant", "w1", []map[string]any{
		{"type": "tool_use", "id": "t1", "name": "Write"},
	}, map[string]any{"cwd": ws}) + entryLine(t, "user", "w2", []map[string]any{
		{"type": "tool_result", "tool_use_id": "t1"},
	}, map[string]any{"cwd": ws})
	if err := os.WriteFile(tf, []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "ws.txt"), []byte("from ws\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rec2.scanOnce()
	refs, err := rec2.git.refNames(recordRefPrefix + sid + "/tree/")
	if err != nil || len(refs) != 1 {
		t.Fatalf("checkpoint refs: %v, %v", refs, err)
	}
	tree, err := rec2.git.run("ls-tree", "-r", "--name-only", refs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tree, "ws.txt") {
		t.Errorf("snapshot missing workspace file:\n%s", tree)
	}
	if strings.Contains(tree, ".jj/") {
		t.Errorf("snapshot ingested .jj state:\n%s", tree)
	}
}

func TestResolveRecordGitDirNonColocatedJJ(t *testing.T) {
	// Non-colocated jj repo: the git store is internal, .jj/repo/store/git.
	main := filepath.Join(t.TempDir(), "main")
	store := filepath.Join(main, ".jj", "repo", "store")
	if err := os.MkdirAll(store, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(store, "type"), []byte("git\n"), 0644)
	os.WriteFile(filepath.Join(store, "git_target"), []byte("git"), 0644)
	gitDir, ok := ResolveRecordGitDir(main)
	if want := filepath.Join(store, "git"); !ok || gitDir != want {
		t.Fatalf("ResolveRecordGitDir = %q, %v; want %q", gitDir, ok, want)
	}
	// Sessions key to the main workspace root, matching the colocated case.
	home := t.TempDir()
	if got, want := repoSessionsDir(home, "/elsewhere/ws", gitDir), TranscriptDir(home, main); got != want {
		t.Errorf("repoSessionsDir = %q, want %q", got, want)
	}
}

func TestRecorderClaimGateByCwd(t *testing.T) {
	e := newTestEnv(t)
	// Entries written from another worktree: this recorder must not claim,
	// even with recordAll.
	other := filepath.Join(t.TempDir(), "elsewhere")
	e.append(t, entryLine(t, "user", "u1", "hi", map[string]any{"cwd": other}))
	e.rec.scanOnce()
	if len(e.rec.sessions) != 0 {
		t.Fatal("claimed a session running in another worktree")
	}
	// The session returns to this worktree: now it is ours. (A subdirectory
	// cwd still counts as within the worktree.)
	e.append(t, entryLine(t, "user", "u2", "back", map[string]any{"cwd": filepath.Join(e.repo, "sub")}))
	e.rec.scanOnce()
	if len(e.rec.sessions) != 1 {
		t.Fatal("did not claim after the session returned to this worktree")
	}
	e.rec.Close()
}

func TestRecorderHandoffOnCwdChange(t *testing.T) {
	e := newTestEnv(t)
	e.append(t,
		entryLine(t, "assistant", "u1", []map[string]any{
			{"type": "tool_use", "id": "t1", "name": "Write"},
		}, map[string]any{"cwd": e.repo}),
		entryLine(t, "user", "u2", []map[string]any{
			{"type": "tool_result", "tool_use_id": "t1"},
		}, map[string]any{"cwd": e.repo}),
	)
	os.WriteFile(filepath.Join(e.repo, "b.txt"), []byte("1\n"), 0644)
	e.rec.scanOnce()
	if len(e.rec.sessions) != 1 {
		t.Fatal("session not claimed")
	}
	ours, _ := os.Stat(e.transcript)

	// The session continues from another worktree: release without
	// consuming the foreign entries, so that worktree's recorder resumes
	// exactly at the blob offset.
	wt := addWorktree(t, e.repo)
	e.clock = e.clock.Add(2 * time.Second)
	e.append(t, entryLine(t, "user", "u3", "over there", map[string]any{"cwd": wt}))
	e.rec.scanOnce()
	if len(e.rec.sessions) != 0 {
		t.Fatal("session not released after moving to another worktree")
	}
	tip := e.rec.git.readRef(recordRefPrefix + e.session + "/meta")
	n, err := e.rec.git.blobSize(tip, "transcript.jsonl")
	if err != nil || n != ours.Size() {
		t.Errorf("blob covers %d bytes, want %d (up to the foreign entry; %v)", n, ours.Size(), err)
	}
	// No tree ref for the foreign uuid.
	for _, ref := range e.refs(t) {
		if strings.HasSuffix(ref, "/u3") {
			t.Errorf("foreign entry got a tree ref: %s", ref)
		}
	}

	// The other worktree's recorder claims it (sticky: same clone) and
	// resumes from the blob offset.
	rec2, err := NewRecorder(filepath.Join(wt, ".git"), wt, e.home, false, "", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now().Add(time.Hour)
	rec2.now = func() time.Time { return clock }
	os.WriteFile(filepath.Join(wt, "c.txt"), []byte("wt\n"), 0644)
	e.append(t,
		entryLine(t, "assistant", "u4", []map[string]any{
			{"type": "tool_use", "id": "t2", "name": "Write"},
		}, map[string]any{"cwd": wt}),
		entryLine(t, "user", "u5", []map[string]any{
			{"type": "tool_result", "tool_use_id": "t2"},
		}, map[string]any{"cwd": wt}),
	)
	rec2.scanOnce()
	if len(rec2.sessions) != 1 {
		t.Fatal("other worktree's recorder did not claim the handed-off session")
	}
	s := rec2.sessions[e.session]
	if s.state.Offset <= n {
		t.Errorf("offset %d did not advance past handoff point %d", s.state.Offset, n)
	}
	var u5 string
	for _, ref := range e.refs(t) {
		if strings.HasSuffix(ref, "/u5") {
			u5 = ref
		}
	}
	if u5 == "" {
		t.Fatalf("no checkpoint from the new worktree; refs: %v", e.refs(t))
	}
	// The checkpoint snapshots the new worktree's tree.
	if out, err := rec2.git.showBlob(u5, "c.txt"); err != nil || out != "wt" {
		t.Errorf("checkpoint c.txt from new worktree: %q, %v", out, err)
	}
	rec2.Close()
}

func TestRecorderMaterializesTranscript(t *testing.T) {
	e := newTestEnv(t)
	e.append(t, entryLine(t, "user", "u1", "hello", nil))
	e.rec.scanOnce()
	e.rec.Close()

	// Claude purged the transcript (cleanupPeriodDays): a new recorder
	// restores it from the session branch, byte-identical to the blob.
	if err := os.Remove(e.transcript); err != nil {
		t.Fatal(err)
	}
	rec2, err := NewRecorder(filepath.Join(e.repo, ".git"), e.repo, e.home, false, "", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(e.transcript)
	if err != nil {
		t.Fatalf("transcript not materialized: %v", err)
	}
	tip := rec2.git.readRef(recordRefPrefix + e.session + "/meta")
	if n, err := rec2.git.blobSize(tip, "transcript.jsonl"); err != nil || n != int64(len(data)) {
		t.Errorf("materialized %d bytes, blob %d (%v)", len(data), n, err)
	}
}

func TestRecorderFlushesTrailingBookkeeping(t *testing.T) {
	e := newTestEnv(t)
	e.append(t, entryLine(t, "user", "u1", "hello", nil))
	e.rec.scanOnce()
	e.clock = e.clock.Add(2 * time.Second)
	// ai-title updates carry no uuid and arrive after the last real entry;
	// the final forced flush must still commit them.
	e.append(t, `{"type":"ai-title","aiTitle":"Greet the user","sessionId":"`+e.session+`"}`+"\n")
	e.rec.scanOnce()
	e.rec.Close()

	got, err := e.rec.git.showBlob(recordRefPrefix+e.session+"/meta", "transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ai-title") {
		t.Errorf("trailing bookkeeping line missing from final blob:\n%s", got)
	}
}

func TestRecorderShrinkRebaseline(t *testing.T) {
	e := newTestEnv(t)
	var lines []string
	for i := 0; i < 5; i++ {
		lines = append(lines, entryLine(t, "user", fmt.Sprintf("u%d", i), "x", nil))
	}
	e.append(t, lines...)
	e.rec.scanOnce()

	// Rewrite the file shorter (compaction): recorder must re-baseline.
	if err := os.WriteFile(e.transcript, []byte(entryLine(t, "user", "v1", "y", nil)), 0644); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(2 * time.Second)
	e.rec.scanOnce()
	s := e.rec.sessions[e.session]
	if s == nil {
		t.Fatal("session lost after shrink")
	}
	st, _ := os.Stat(e.transcript)
	if s.state.Offset != st.Size() {
		t.Errorf("offset after re-baseline: %d, want %d", s.state.Offset, st.Size())
	}
	e.rec.Close()
}
