package record

import (
	"log"
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

	// Download into a fresh clone (which has the prerequisite commit):
	// fetch-only, nothing checked out.
	cloneParent := t.TempDir()
	clone := filepath.Join(cloneParent, "clone")
	cmd := exec.Command("git", "clone", "-q", e.repo, clone)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	cg := &gitRepo{gitDir: filepath.Join(clone, ".git")}
	if err := downloadSession(cg, bundle, ""); err != nil {
		t.Fatal(err)
	}
	if refs, err := treeRefs(cg, e.session); err != nil || len(refs) != 2 {
		t.Fatalf("fetched checkpoint refs: %v, %v", refs, err)
	}
	if data, err := os.ReadFile(filepath.Join(clone, "b.txt")); err == nil {
		t.Errorf("download checked out b.txt (%q); it must only fetch", data)
	}

	// Resume materializes the tree: on the clean clone the latest
	// checkpoint is checked out (detached).
	if err := RestoreCheckpoint(clone, e.session, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(clone, "b.txt"))
	if err != nil || string(data) != "v2\n" {
		t.Errorf("restored b.txt: %q, %v", data, err)
	}
	refs, _ := treeRefs(cg, e.session)
	if head := cg.headCommit(); head != cg.readRef(refs[len(refs)-1]) {
		t.Errorf("HEAD %s not at latest checkpoint %s", head, cg.readRef(refs[len(refs)-1]))
	}
	// Already at the checkpoint: a second restore is a no-op.
	if err := RestoreCheckpoint(clone, e.session, nil); err != nil {
		t.Fatal(err)
	}
	// A dirty worktree refuses to restore.
	if err := os.WriteFile(filepath.Join(clone, "b.txt"), []byte("dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := RestoreCheckpoint(clone, e.session, nil); err == nil {
		t.Error("RestoreCheckpoint on dirty worktree succeeded")
	}
	// An unrecorded sid is claude's to resolve: no-op, no error.
	if err := RestoreCheckpoint(clone, "not-recorded", nil); err != nil {
		t.Errorf("RestoreCheckpoint for unrecorded sid: %v", err)
	}

	// Recorder startup in the clone materializes the fetched transcript
	// into the repo-scoped sessions dir; its size must equal the recorded
	// blob (it is the resume offset).
	home2 := t.TempDir()
	rec2, err := NewRecorder(filepath.Join(clone, ".git"), clone, home2, false, "", nil, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	rec2.Close()
	tfile := filepath.Join(TranscriptDir(home2, clone), e.session+".jsonl")
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
	if got := AutoForkSession(clone, resume); len(got) != 1 || got[0] != "--fork-session" {
		t.Errorf("AutoForkSession in reviewer clone: %v", got)
	}
	if got := AutoForkSession(e.repo, resume); got != nil {
		t.Errorf("AutoForkSession in recording clone: %v", got)
	}
	if got := AutoForkSession(clone, []string{"--resume", e.session, "--fork-session"}); got != nil {
		t.Errorf("AutoForkSession with explicit flag: %v", got)
	}
	if got := AutoForkSession(clone, []string{"--resume", "not-recorded"}); got != nil {
		t.Errorf("AutoForkSession for unrecorded sid: %v", got)
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
	handled, verb, sid, rest, err := DispatchSubcommand([]string{"new", "--", "-p", "hi"}, nil)
	if handled || verb != "new" || sid != "" || err != nil || !reflect.DeepEqual(rest, []string{"--", "-p", "hi"}) {
		t.Errorf("new: handled=%v verb=%q sid=%q rest=%v err=%v", handled, verb, sid, rest, err)
	}
	// An explicit resume id needs no repo; remaining args feed flag parsing.
	handled, verb, sid, rest, err = DispatchSubcommand([]string{"resume", "abc", "--restrict-net=false"}, nil)
	if handled || verb != "resume" || sid != "abc" || err != nil || !reflect.DeepEqual(rest, []string{"--restrict-net=false"}) {
		t.Errorf("resume abc: handled=%v verb=%q sid=%q rest=%v err=%v", handled, verb, sid, rest, err)
	}
	// Anything else passes through untouched to regular flag parsing —
	// including `--` escaping a command named like a verb.
	for _, args := range [][]string{nil, {"--restrict-net=false"}, {"--", "list"}, {"bash", "-c", "true"}} {
		handled, verb, sid, rest, err := DispatchSubcommand(args, nil)
		if handled || verb != "" || sid != "" || err != nil || !reflect.DeepEqual(rest, args) {
			t.Errorf("DispatchSubcommand(%v): handled=%v verb=%q sid=%q rest=%v err=%v",
				args, handled, verb, sid, rest, err)
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

	bundleDir := t.TempDir()
	if err := runSessionSubcommand("upload", []string{"--upstream", "main", bundleDir}, e.repo, ""); err != nil {
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
	if err := runSessionSubcommand("download", []string{bundle}, clone, ""); err != nil {
		t.Fatal(err)
	}
	cg := &gitRepo{gitDir: filepath.Join(clone, ".git")}
	if refs, err := treeRefs(cg, e.session); err != nil || len(refs) == 0 {
		t.Errorf("downloaded checkpoint refs: %v, %v", refs, err)
	}

	if err := runSessionSubcommand("list", []string{"extra"}, e.repo, ""); err == nil {
		t.Error("list accepted positional args")
	}
	if err := runSessionSubcommand("rm", nil, e.repo, ""); err == nil {
		t.Error("rm accepted empty args")
	}
	if err := runSessionSubcommand("rm", []string{e.session}, e.repo, ""); err != nil {
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
