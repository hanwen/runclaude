package record

// Native jj integration for session recording. In a jj workspace the
// recorder resolves the working-copy change per checkpoint so snapshots
// become native jj changes: commits carry a `change-id` header holding @'s
// change id (jj round-trips the header on import, so the checkpoint shows
// up as a version of the user's change in any clone that materializes it),
// and when jj's own snapshot already matches the worktree the checkpoint
// ref points at the jj-authored commit itself — sha-identical, genuine
// header, author's identity.
//
// Everything here is deliberately operation-free: `--ignore-working-copy`
// skips both the auto-snapshot and the stale-workspace check, so recording
// never appears in `jj op log`, never interleaves with `jj undo`, and
// keeps working while the workspace is stale (a working-copy commit
// rewritten from another workspace keeps its change id, so the header is
// correct even then). Host `jj` is a dependency only for this — recording
// itself degrades to plain git checkpoints without it.

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

// detectJJ returns the directory to run jj in for cwd's workspace, or ""
// when cwd is not a jj workspace or the jj binary is unavailable.
func detectJJ(cwd string, logger *log.Logger) string {
	if _, ok := jjRepoDir(cwd); !ok {
		return ""
	}
	if _, err := exec.LookPath("jj"); err != nil {
		logger.Printf("record: %s is a jj workspace but jj is not on PATH; checkpoints will not carry change ids", cwd)
		return ""
	}
	return cwd
}

// jjWC is the working-copy commit @ as resolved by jjWorkingCopy: what a
// recorder-authored checkpoint amends (same parents, description, and
// change id — only the tree differs).
type jjWC struct {
	commitID string
	changeID string
	// parents are @'s parent commit ids; empty when @ sits on the root
	// commit, which is virtual (no git object) and cannot be a parent.
	parents []string
	desc    string
}

// jjWorkingCopy resolves dir's workspace working-copy commit without
// creating an operation or touching the working copy.
func jjWorkingCopy(dir string) (*jjWC, error) {
	cmd := exec.Command("jj", "log", "--no-graph", "--ignore-working-copy",
		"-r", "@", "-T", `commit_id ++ " " ++ change_id ++ " " ++ parents.map(|c| c.commit_id()).join(" ") ++ "\n" ++ description`)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("jj log -r @: %v: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("jj log -r @: %w", err)
	}
	ids, desc, _ := strings.Cut(string(out), "\n")
	fields := strings.Fields(ids)
	if len(fields) < 2 || !isHex(fields[0]) || !isReverseHex(fields[1]) {
		return nil, fmt.Errorf("jj log -r @: unexpected output %q", ids)
	}
	wc := &jjWC{commitID: fields[0], changeID: fields[1], desc: desc}
	for _, p := range fields[2:] {
		if !isHex(p) {
			return nil, fmt.Errorf("jj log -r @: unexpected parent %q", p)
		}
		if strings.Trim(p, "0") == "" {
			continue // the virtual root commit
		}
		wc.parents = append(wc.parents, p)
	}
	return wc, nil
}

// isHex reports whether s is a non-empty lowercase hex string (a commit id).
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// isReverseHex reports whether s is a jj change id: the k-z alphabet
// ('0'→'z' … 'f'→'k') encoding of 16 bytes. Checked before the id is
// spliced into a raw commit object.
func isReverseHex(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		if r < 'k' || r > 'z' {
			return false
		}
	}
	return true
}
