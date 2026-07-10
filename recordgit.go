package main

// Git plumbing helpers for session recording. All operations shell out to
// the host `git` binary with GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE set
// explicitly, so the user's index and working state are never touched.
// Host git is a dependency only when recording is active.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type gitRepo struct {
	gitDir   string
	workTree string
	// indexFile is a private index (per recorded session) so snapshots
	// never disturb the user's staging area. Persistent across flushes so
	// `add -A` restats incrementally.
	indexFile string
	// excludesFile holds extra ignore patterns applied via
	// core.excludesFile (composes with the repo's own info/exclude, which
	// still applies through gitDir). Empty = none.
	excludesFile string
	// identEnv supplies a fallback committer identity for repos without
	// git user config (e.g. jj-only users); empty when config exists.
	identEnv []string
}

func (g *gitRepo) command(args ...string) *exec.Cmd {
	full := []string{"--git-dir", g.gitDir}
	if g.excludesFile != "" {
		full = append(full, "-c", "core.excludesFile="+g.excludesFile)
	}
	full = append(full, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_WORK_TREE="+g.workTree,
		"GIT_TERMINAL_PROMPT=0",
	)
	cmd.Env = append(cmd.Env, g.identEnv...)
	if g.indexFile != "" {
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+g.indexFile)
	}
	cmd.Dir = g.workTree
	return cmd
}

// run executes git with args and returns trimmed stdout.
func (g *gitRepo) run(args ...string) (string, error) {
	cmd := g.command(args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// runInput is run with stdin supplied.
func (g *gitRepo) runInput(input string, args ...string) (string, error) {
	cmd := g.command(args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// snapshotTree stages the entire worktree into the private index and
// returns the resulting tree hash.
func (g *gitRepo) snapshotTree() (string, error) {
	if _, err := g.run("add", "-A"); err != nil {
		return "", err
	}
	return g.run("write-tree")
}

// headCommit returns the commit HEAD points at, or "" on an unborn HEAD.
func (g *gitRepo) headCommit() string {
	sha, err := g.run("rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return ""
	}
	return sha
}

// isAncestor reports whether a is an ancestor of b.
func (g *gitRepo) isAncestor(a, b string) bool {
	err := g.command("merge-base", "--is-ancestor", a, b).Run()
	return err == nil
}

func (g *gitRepo) commitTree(tree string, parents []string, msg string) (string, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	args = append(args, "-m", msg)
	return g.run(args...)
}

func (g *gitRepo) updateRef(name, sha string) error {
	_, err := g.run("update-ref", name, sha)
	return err
}

func (g *gitRepo) deleteRef(name string) error {
	_, err := g.run("update-ref", "-d", name)
	return err
}

// readRef returns the commit a ref points at, or "" if it does not exist.
func (g *gitRepo) readRef(name string) string {
	sha, err := g.run("rev-parse", "--verify", "--quiet", name)
	if err != nil {
		return ""
	}
	return sha
}

// refNames lists full refnames under prefix, sorted by name.
func (g *gitRepo) refNames(prefix string) ([]string, error) {
	out, err := g.run("for-each-ref", "--format=%(refname)", prefix)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// hashFile writes path's content as a blob object and returns its hash.
func (g *gitRepo) hashFile(path string) (string, error) {
	return g.run("hash-object", "-w", "--", path)
}

// singleFileTree builds a tree containing exactly one file entry.
func (g *gitRepo) singleFileTree(name, blob string) (string, error) {
	return g.runInput(fmt.Sprintf("100644 blob %s\t%s\n", blob, name), "mktree")
}

// commitMessage returns the full commit message body of sha.
func (g *gitRepo) commitMessage(sha string) (string, error) {
	return g.run("log", "-1", "--format=%B", sha)
}

// showBlob returns the content of <rev>:<path>.
func (g *gitRepo) showBlob(rev, path string) (string, error) {
	return g.run("show", rev+":"+path)
}

// ensureIdent installs a fallback identity when the repo has none
// configured (commit-tree refuses to run identity-less).
func (g *gitRepo) ensureIdent() {
	if email, _ := g.run("config", "user.email"); email == "" {
		g.identEnv = []string{
			"GIT_AUTHOR_NAME=runclaude", "GIT_AUTHOR_EMAIL=runclaude@localhost",
			"GIT_COMMITTER_NAME=runclaude", "GIT_COMMITTER_EMAIL=runclaude@localhost",
		}
	}
}

// repack packs runclaude refs and loose objects; called once at session
// end to collapse per-flush transcript blobs into deltas. Errors are
// returned for logging but are not fatal to the recording.
func (g *gitRepo) repack() error {
	if _, err := g.run("pack-refs", "--all", "--prune"); err != nil {
		return err
	}
	_, err := g.run("repack", "-d", "-q")
	return err
}
