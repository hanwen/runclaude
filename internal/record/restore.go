package record

// Restoring a session's recorded tree — the FS half of `runclaude resume`
// (claude's --resume restores the conversation). The latest checkpoint
// under refs/runclaude/<sid>/tree/ is materialized into the worktree on
// the host, before the sandbox launches:
//
//   - jj workspaces: the checkpoint is made visible through a temporary
//     local git branch + `jj git import` (refs/runclaude/ is invisible to
//     jj; the explicit import is required for non-colocated stores and
//     harmless elsewhere), then `jj new <checkpoint>` puts the working
//     copy on top of it — snapshotting the previous state first, so
//     nothing is lost and no clean-tree requirement applies. The
//     change-id header on recorder-authored checkpoints makes the
//     imported commit a version of the recorded change.
//   - plain git: `git checkout --detach <checkpoint>`, only when the
//     worktree is clean.
//
// The temporary branch is removed with `jj bookmark delete` + `jj git
// export` — never by deleting the git ref directly: jj treats an
// externally deleted branch as rewritten history, and the next import
// abandons the checkpoint and rebases @ off it, reverting the restore.

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// restoreBookmark is the temporary bookmark that makes a checkpoint commit
// visible to jj for the duration of a restore.
const restoreBookmark = "runclaude/restore"

// RestoreCheckpoint restores cwd's worktree to sid's latest recorded tree
// checkpoint. Not-a-repo, not-a-recorded-session, and no-checkpoints all
// no-op: the sid is then claude's to resolve. A worktree already at the
// checkpoint tree (the same-directory resume) is left untouched.
// extraExcludes must match the recorder's --record-exclude patterns so the
// comparison sees the same tree the recorder snapshotted.
func RestoreCheckpoint(cwd, sid string, extraExcludes []string) error {
	gitDir, ok := ResolveRecordGitDir(cwd)
	if !ok {
		return nil
	}
	g := &gitRepo{gitDir: gitDir, workTree: cwd}
	if _, err := readMeta(g, sid); err != nil {
		return nil
	}
	refs, err := treeRefs(g, sid)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}
	ref := refs[len(refs)-1]
	checkpoint := g.readRef(ref)
	if checkpoint == "" {
		return fmt.Errorf("cannot resolve checkpoint ref %s", ref)
	}
	tmp, err := os.MkdirTemp("", "runclaude-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	g.excludesFile = filepath.Join(tmp, "exclude")
	if err := writeExcludesFile(g.excludesFile, extraExcludes); err != nil {
		return err
	}
	cpTree, err := g.run("rev-parse", checkpoint+"^{tree}")
	if err != nil {
		return err
	}
	// Snapshot with the recorder's exclusions into a throwaway index: a
	// worktree that already matches the checkpoint is left alone.
	snap := *g
	snap.indexFile = filepath.Join(tmp, "index")
	tree, err := snap.snapshotTree()
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(ref, recordRefPrefix+sid+"/tree/")
	if tree == cpTree {
		log.Printf("resume: worktree already at session %s checkpoint %s", sid, name)
		return nil
	}
	if _, ok := jjRepoDir(cwd); ok {
		err = restoreJJ(g, cwd, checkpoint)
	} else {
		err = restoreGit(g, checkpoint)
	}
	if err != nil {
		return err
	}
	log.Printf("resume: restored session %s checkpoint %s", sid, name)
	return nil
}

// restoreGit detaches the worktree's HEAD at the checkpoint. It refuses on
// a dirty worktree: unlike jj there is no snapshot of the current state,
// so the checkout would clobber unrecorded work.
func restoreGit(g *gitRepo, checkpoint string) error {
	status, err := g.run("status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("%s has uncommitted changes; commit or stash them, or resume without tree restore via `runclaude -- --resume <sid>`", g.workTree)
	}
	_, err = g.run("-c", "advice.detachedHead=false", "checkout", "--detach", checkpoint)
	return err
}

// restoreJJ re-parents the workspace's working copy onto the checkpoint
// with `jj new`. All cleanup goes through jj (see the file comment for why
// the git ref must never be deleted directly); a leftover bookmark from a
// failed cleanup is harmless — the next restore moves it.
func restoreJJ(g *gitRepo, cwd, checkpoint string) error {
	if _, err := exec.LookPath("jj"); err != nil {
		return fmt.Errorf("%s is a jj workspace but jj is not on PATH; cannot restore the recorded tree", cwd)
	}
	if err := g.updateRef("refs/heads/"+restoreBookmark, checkpoint); err != nil {
		return err
	}
	if err := jjExec(cwd, "git", "import"); err != nil {
		return err
	}
	newErr := jjExec(cwd, "new", checkpoint)
	// Best-effort even after failure (e.g. a stale workspace): @ has not
	// moved, so dropping the bookmark cannot orphan anything the user sees.
	if err := jjExec(cwd, "bookmark", "delete", restoreBookmark); err != nil && newErr == nil {
		log.Printf("resume: cleanup: %v", err)
	}
	if err := jjExec(cwd, "git", "export"); err != nil && newErr == nil {
		log.Printf("resume: cleanup: %v", err)
	}
	return newErr
}

// jjExec runs jj in dir with its output streamed to the user (restores run
// before claude owns the terminal, and jj's working-copy report is the
// best summary of what changed).
func jjExec(dir string, args ...string) error {
	cmd := exec.Command("jj", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("jj %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
