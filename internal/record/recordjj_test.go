package record

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsReverseHex(t *testing.T) {
	for s, want := range map[string]bool{
		"nzuzulkrnpxzqmzqtpuukmvzwyksvost": true,
		"nzuzulkrnpxzqmzqtpuukmvzwyksvos":  false, // short
		"nzuzulkrnpxzqmzqtpuukmvzwyksvosa": false, // 'a' outside k-z
		"":                                 false,
	} {
		if got := isReverseHex(s); got != want {
			t.Errorf("isReverseHex(%q) = %v, want %v", s, got, want)
		}
	}
}

// testJJRepo initializes a real jj repo with a snapshotted file and returns
// its path. Skips when jj is not on PATH. JJ_CONFIG is pointed at a
// hermetic config for the whole test (jjWorkingCopy inherits the process
// env).
func testJJRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "jjconfig.toml")
	if err := os.WriteFile(cfg, []byte("[user]\nname = \"t\"\nemail = \"t@t\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JJ_CONFIG", cfg)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0755); err != nil {
		t.Fatal(err)
	}
	runJJ(t, repo, "git", "init")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runJJ(t, repo, "util", "snapshot")
	return repo
}

func runJJ(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("jj", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jj %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRecorderJJNativeCheckpoints(t *testing.T) {
	repo := testJJRepo(t)
	gitDir, ok := ResolveRecordGitDir(repo)
	if !ok {
		t.Fatalf("ResolveRecordGitDir(%s) failed", repo)
	}
	home := t.TempDir()
	rec, err := NewRecorder(gitDir, repo, home, true, "", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	if rec.jjDir != repo {
		t.Fatalf("jj workspace not detected: jjDir=%q", rec.jjDir)
	}
	wc, err := jjWorkingCopy(repo)
	if err != nil {
		t.Fatal(err)
	}
	opsBefore := runJJ(t, repo, "op", "log", "--no-graph", "--ignore-working-copy", "-T", `id.short() ++ "\n"`)

	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	transcript := filepath.Join(rec.ProjectsDir, sid+".jsonl")
	appendEntries := func(useUUID, resultUUID string) {
		t.Helper()
		lines := entryLine(t, "assistant", useUUID, []map[string]any{
			{"type": "tool_use", "id": "t-" + useUUID, "name": "Write"},
		}, map[string]any{"cwd": repo}) + entryLine(t, "user", resultUUID, []map[string]any{
			{"type": "tool_result", "tool_use_id": "t-" + useUUID},
		}, map[string]any{"cwd": repo})
		f, err := os.OpenFile(transcript, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(lines); err != nil {
			t.Fatal(err)
		}
	}
	checkpoint := func(uuid string) string {
		t.Helper()
		ref := ""
		refs, err := rec.git.refNames(recordRefPrefix + sid + "/tree/")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range refs {
			if strings.HasSuffix(r, "/"+uuid) {
				ref = r
			}
		}
		if ref == "" {
			t.Fatalf("no checkpoint ref for %s; refs: %v", uuid, refs)
		}
		return rec.git.readRef(ref)
	}

	// The tree is unchanged since `jj util snapshot`: the checkpoint reuses
	// the jj-authored working-copy commit verbatim.
	appendEntries("u1", "u2")
	rec.scanOnce()
	if sha := checkpoint("u2"); sha != wc.commitID {
		t.Errorf("clean-tree checkpoint = %s, want jj working-copy commit %s", sha, wc.commitID)
	}

	// The tree drifted since jj's snapshot (no jj command ran): the recorder
	// authors an amended version of @ — its change id in the header, its
	// parents (here none: @ sits on the virtual root), its (empty)
	// description — never a commit stacked on the previous checkpoint.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("edited\n"), 0644); err != nil {
		t.Fatal(err)
	}
	appendEntries("u3", "u4")
	rec.scanOnce()
	sha := checkpoint("u4")
	if sha == wc.commitID {
		t.Fatal("drifted tree reused the jj commit")
	}
	raw, err := rec.git.run("cat-file", "commit", sha)
	if err != nil {
		t.Fatal(err)
	}
	// No trailing \n in the probe: run() trims output, and with an empty
	// message the header is the last line.
	if !strings.Contains(raw, "\nchange-id "+wc.changeID) {
		t.Errorf("checkpoint lacks change-id header for %s:\n%s", wc.changeID, raw)
	}
	if strings.Contains(raw, "\nparent ") {
		t.Errorf("root-based checkpoint grew a parent:\n%s", raw)
	}
	if msg, err := rec.git.run("log", "-1", "--format=%B", sha); err != nil || msg != "" {
		t.Errorf("checkpoint of an undescribed change has message %q, %v", msg, err)
	}

	// Recording must be invisible to jj: no operations were created.
	if opsAfter := runJJ(t, repo, "op", "log", "--no-graph", "--ignore-working-copy", "-T", `id.short() ++ "\n"`); opsAfter != opsBefore {
		t.Errorf("recording created jj operations:\nbefore:\n%s\nafter:\n%s", opsBefore, opsAfter)
	}

	// After the user commits and describes, checkpoints amend the new @:
	// same parent, the user's description — and successive checkpoints
	// still share that parent instead of stacking.
	runJJ(t, repo, "commit", "-m", "base work")
	base := runJJ(t, repo, "log", "--no-graph", "--ignore-working-copy", "-r", "@-", "-T", "commit_id")
	runJJ(t, repo, "describe", "-m", "user description")
	for i, uuids := range [][2]string{{"u5", "u6"}, {"u7", "u8"}} {
		if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte(fmt.Sprintf("v%d\n", i)), 0644); err != nil {
			t.Fatal(err)
		}
		appendEntries(uuids[0], uuids[1])
		rec.scanOnce()
		sha := checkpoint(uuids[1])
		if parents, err := rec.git.run("log", "-1", "--format=%P", sha); err != nil || parents != base {
			t.Errorf("%s: parents %q, want @'s parent %q (%v)", uuids[1], parents, base, err)
		}
		if msg, err := rec.git.run("log", "-1", "--format=%B", sha); err != nil || msg != "user description" {
			t.Errorf("%s: message %q, want the user's jj description (%v)", uuids[1], msg, err)
		}
	}
}

// A workspace made stale by another workspace rewriting an ancestor must
// keep recording: change-id resolution is --ignore-working-copy and the
// rewritten working-copy commit keeps its change id.
func TestRecorderJJStaleWorkspace(t *testing.T) {
	repo := testJJRepo(t)
	runJJ(t, repo, "describe", "-m", "base")
	runJJ(t, repo, "new", "-m", "child")
	changeID := runJJ(t, repo, "log", "--no-graph", "--ignore-working-copy", "-r", "@", "-T", "change_id")

	gitDir, ok := ResolveRecordGitDir(repo)
	if !ok {
		t.Fatalf("ResolveRecordGitDir(%s) failed", repo)
	}
	home := t.TempDir()
	rec, err := NewRecorder(gitDir, repo, home, true, "", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	// From a second workspace, amend the ancestor: @'s commit is rebased by
	// an operation the main workspace did not participate in — stale.
	ws2 := filepath.Join(filepath.Dir(repo), "ws2")
	runJJ(t, repo, "workspace", "add", ws2)
	runJJ(t, ws2, "edit", runJJ(t, repo, "log", "--no-graph", "--ignore-working-copy", "-r", "@-", "-T", "change_id"))
	if err := os.WriteFile(filepath.Join(ws2, "a.txt"), []byte("rewritten\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runJJ(t, ws2, "util", "snapshot")
	stale := exec.Command("jj", "util", "snapshot")
	stale.Dir = repo
	if out, err := stale.CombinedOutput(); err == nil || !strings.Contains(string(out), "stale") {
		t.Fatalf("main workspace should be stale: err=%v out=%s", err, out)
	}

	if err := os.WriteFile(filepath.Join(repo, "edited.txt"), []byte("while stale\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sid := "fefefefe-0101-2323-4545-676767676767"
	lines := entryLine(t, "assistant", "s1", []map[string]any{
		{"type": "tool_use", "id": "t1", "name": "Write"},
	}, map[string]any{"cwd": repo}) + entryLine(t, "user", "s2", []map[string]any{
		{"type": "tool_result", "tool_use_id": "t1"},
	}, map[string]any{"cwd": repo})
	if err := os.WriteFile(filepath.Join(rec.ProjectsDir, sid+".jsonl"), []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}
	rec.scanOnce()
	refs, err := rec.git.refNames(recordRefPrefix + sid + "/tree/")
	if err != nil || len(refs) != 1 {
		t.Fatalf("stale workspace produced no checkpoint: %v, %v", refs, err)
	}
	if rec.jjDir == "" {
		t.Error("staleness disabled jj integration")
	}
	raw, err := rec.git.run("cat-file", "commit", rec.git.readRef(refs[0]))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "\nchange-id "+changeID+"\n") {
		t.Errorf("stale-workspace checkpoint lacks @'s change id %s:\n%s", changeID, raw)
	}
}

// A broken jj workspace (fabricated metadata, no real store) must degrade
// to plain git checkpoints after logging once, not fail recording.
func TestRecorderJJBrokenFallsBack(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not on PATH")
	}
	e := newTestEnv(t)
	ws := addJJWorkspace(t, e.repo)
	rec, err := NewRecorder(filepath.Join(e.repo, ".git"), ws, e.home, true, "", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	if rec.jjDir == "" {
		t.Skip("jj workspace not detected") // LookPath raced; nothing to test
	}
	sid := "12121212-3434-5656-7878-909090909090"
	lines := entryLine(t, "assistant", "b1", []map[string]any{
		{"type": "tool_use", "id": "t1", "name": "Write"},
	}, map[string]any{"cwd": ws}) + entryLine(t, "user", "b2", []map[string]any{
		{"type": "tool_result", "tool_use_id": "t1"},
	}, map[string]any{"cwd": ws})
	if err := os.WriteFile(filepath.Join(rec.ProjectsDir, sid+".jsonl"), []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rec.scanOnce()
	refs, err := rec.git.refNames(recordRefPrefix + sid + "/tree/")
	if err != nil || len(refs) != 1 {
		t.Fatalf("checkpoint refs: %v, %v", refs, err)
	}
	if rec.jjDir != "" {
		t.Error("broken jj workspace did not disable jj integration")
	}
	raw, err := rec.git.run("cat-file", "commit", rec.git.readRef(refs[0]))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "change-id") {
		t.Errorf("fallback checkpoint carries a change-id header:\n%s", raw)
	}
}

// RestoreCheckpoint in a jj repo: a same-directory resume leaves the
// working copy alone; a resume in another workspace of the clone imports
// the checkpoint (keeping its change id) and puts @ on top of it, with the
// temporary bookmark cleaned up so a later import cannot abandon it.
func TestRestoreCheckpointJJWorkspace(t *testing.T) {
	repo := testJJRepo(t)
	gitDir, ok := ResolveRecordGitDir(repo)
	if !ok {
		t.Fatalf("ResolveRecordGitDir(%s) failed", repo)
	}
	home := t.TempDir()
	rec, err := NewRecorder(gitDir, repo, home, true, "", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("v2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sid := "abababab-0101-2323-4545-676767676767"
	lines := entryLine(t, "assistant", "s1", []map[string]any{
		{"type": "tool_use", "id": "t1", "name": "Write"},
	}, map[string]any{"cwd": repo}) + entryLine(t, "user", "s2", []map[string]any{
		{"type": "tool_result", "tool_use_id": "t1"},
	}, map[string]any{"cwd": repo})
	if err := os.WriteFile(filepath.Join(rec.ProjectsDir, sid+".jsonl"), []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}
	rec.scanOnce()
	rec.Close()
	wc, err := jjWorkingCopy(repo)
	if err != nil {
		t.Fatal(err)
	}
	changeID := wc.changeID

	// Same directory: the worktree already matches the checkpoint.
	before := runJJ(t, repo, "log", "--no-graph", "--ignore-working-copy", "-r", "@", "-T", "commit_id")
	if err := RestoreCheckpoint(repo, sid, nil); err != nil {
		t.Fatal(err)
	}
	if after := runJJ(t, repo, "log", "--no-graph", "--ignore-working-copy", "-r", "@", "-T", "commit_id"); after != before {
		t.Errorf("same-directory restore moved @: %s -> %s", before, after)
	}

	// Another workspace of the same repo.
	ws2 := filepath.Join(filepath.Dir(repo), "restore-ws2")
	runJJ(t, repo, "workspace", "add", ws2)
	if err := RestoreCheckpoint(ws2, sid, nil); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(ws2, "b.txt")); err != nil || string(data) != "v2\n" {
		t.Errorf("restored b.txt in workspace: %q, %v", data, err)
	}
	if got := runJJ(t, ws2, "log", "--no-graph", "--ignore-working-copy", "-r", "@-", "-T", "change_id"); got != changeID {
		t.Errorf("restored parent change id %s, want the recorded change %s", got, changeID)
	}
	if out := runJJ(t, ws2, "bookmark", "list"); strings.Contains(out, restoreBookmark) {
		t.Errorf("temporary bookmark not cleaned up:\n%s", out)
	}
	g := &gitRepo{gitDir: gitDir}
	if sha := g.readRef("refs/heads/" + restoreBookmark); sha != "" {
		t.Errorf("temporary git ref remains at %s", sha)
	}
	// A later import must not undo the restore (cleanup went through jj,
	// not an external git ref deletion).
	runJJ(t, ws2, "git", "import")
	if data, err := os.ReadFile(filepath.Join(ws2, "b.txt")); err != nil || string(data) != "v2\n" {
		t.Errorf("import after restore reverted the tree: %q, %v", data, err)
	}
}
