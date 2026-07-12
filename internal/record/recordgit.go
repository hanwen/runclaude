package record

// Git plumbing helpers for session recording. All operations shell out to
// the host `git` binary with GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE set
// explicitly, so the user's index and working state are never touched.
// Host git is a dependency only when recording is active.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	// workTree is empty for ref-only use (upload/download commands);
	// GIT_WORK_TREE must then stay unset or `git worktree add` and friends
	// misbehave.
	if g.workTree != "" {
		cmd.Env = append(cmd.Env, "GIT_WORK_TREE="+g.workTree)
		cmd.Dir = g.workTree
	}
	cmd.Env = append(cmd.Env, g.identEnv...)
	if g.indexFile != "" {
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+g.indexFile)
	}
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

// commitTreeChangeID is commitTree with a jj `change-id` header, making
// the commit a native jj change (jj adopts the header on import). git
// commit-tree cannot add headers, so the raw commit object is assembled
// here and written with hash-object; --literally skips the format check,
// which predates jj's header. changeID must be pre-validated (isReverseHex).
func (g *gitRepo) commitTreeChangeID(tree string, parents []string, msg, changeID string) (string, error) {
	if changeID == "" {
		return g.commitTree(tree, parents, msg)
	}
	author, err := g.run("var", "GIT_AUTHOR_IDENT")
	if err != nil {
		return "", err
	}
	committer, err := g.run("var", "GIT_COMMITTER_IDENT")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s\ncommitter %s\nchange-id %s\n\n%s", author, committer, changeID, msg)
	if !strings.HasSuffix(msg, "\n") {
		b.WriteString("\n")
	}
	return g.runInput(b.String(), "hash-object", "-t", "commit", "-w", "--stdin", "--literally")
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

// hashFilePrefix writes the first n bytes of path as a blob object.
func (g *gitRepo) hashFilePrefix(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	cmd := g.command("hash-object", "-w", "--stdin")
	cmd.Stdin = io.LimitReader(f, n)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git hash-object --stdin: %v: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git hash-object --stdin: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// blobSize returns the size in bytes of <rev>:<path>.
func (g *gitRepo) blobSize(rev, path string) (int64, error) {
	out, err := g.run("cat-file", "-s", rev+":"+path)
	if err != nil {
		return 0, err
	}
	var n int64
	if _, err := fmt.Sscanf(out, "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

// blobTree builds a tree of plain-file entries. mktree requires input in
// tree order, hence the sort.
func (g *gitRepo) blobTree(entries map[string]string) (string, error) {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "100644 blob %s\t%s\n", entries[name], name)
	}
	return g.runInput(b.String(), "mktree")
}

// showBlob returns the content of <rev>:<path>, trimmed.
func (g *gitRepo) showBlob(rev, path string) (string, error) {
	return g.run("show", rev+":"+path)
}

// blobBytes returns the exact content of <rev>:<path>. Use this when the
// byte count matters (transcript blobs: size == resume offset).
func (g *gitRepo) blobBytes(rev, path string) ([]byte, error) {
	cmd := g.command("cat-file", "blob", rev+":"+path)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git cat-file blob %s:%s: %v: %s", rev, path, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git cat-file blob %s:%s: %w", rev, path, err)
	}
	return out, nil
}

// commonGitDir returns the repo's common git dir as an absolute path: the
// main .git dir shared by all linked worktrees.
func (g *gitRepo) commonGitDir() (string, error) {
	common, err := g.run("rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(common) {
		// rev-parse resolves relative to the process cwd (the worktree).
		base := g.workTree
		if base == "" {
			base, _ = os.Getwd()
		}
		common = filepath.Join(base, common)
	}
	return filepath.Clean(common), nil
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
