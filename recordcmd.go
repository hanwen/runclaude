package main

// Host-only commands operating on recorded sessions: --upload, --download,
// --sessions, --record-rm. These run before (instead of) any sandbox setup.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type recordCmdFlags struct {
	upload   string // bundle path/dir, remote name, or URL
	download string // bundle path, remote name, or URL
	session  string // session id, or "latest"/"" for upload
	at       string // uuid or timestamp substring selecting the checkpoint
	dest     string // worktree dir for --download
	upstream string // --record-upstream
	rm       string // session id to delete
	list     bool   // --sessions
}

// runRecordCommands executes any requested record command. It returns true
// when a command ran (mainErr should exit without starting a sandbox).
func runRecordCommands(f recordCmdFlags, cwd, home string) (bool, error) {
	if f.upload == "" && f.download == "" && f.rm == "" && !f.list {
		return false, nil
	}
	gitDir, ok := resolveGitDir(cwd)
	if !ok {
		return true, fmt.Errorf("no git repository at %s", cwd)
	}
	g := &gitRepo{gitDir: gitDir}
	switch {
	case f.list:
		return true, listSessions(g)
	case f.rm != "":
		return true, removeSession(g, f.rm, cwd)
	case f.upload != "":
		sid, err := resolveSessionID(g, f.session)
		if err != nil {
			return true, err
		}
		return true, uploadSession(g, sid, f.upload, f.upstream)
	default:
		return true, downloadSession(g, f.download, f.session, f.at, f.dest, cwd, home)
	}
}

// sessionIDs returns the ids of locally recorded sessions, oldest first.
func sessionIDs(g *gitRepo) ([]string, error) {
	out, err := g.run("for-each-ref", "--sort=committerdate", "--format=%(refname)", recordRefPrefix+"*/meta")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, ref := range strings.Split(out, "\n") {
		if ref == "" {
			continue
		}
		rest := strings.TrimPrefix(ref, recordRefPrefix)
		ids = append(ids, strings.TrimSuffix(rest, "/meta"))
	}
	return ids, nil
}

func resolveSessionID(g *gitRepo, flagValue string) (string, error) {
	if flagValue != "" && flagValue != "latest" {
		return flagValue, nil
	}
	ids, err := sessionIDs(g)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("no recorded sessions (run with --record first)")
	}
	return ids[len(ids)-1], nil
}

func readMeta(g *gitRepo, sid string) (sessionMeta, error) {
	var m sessionMeta
	data, err := g.showBlob(recordRefPrefix+sid+"/meta", "meta.json")
	if err != nil {
		return m, err
	}
	err = json.Unmarshal([]byte(data), &m)
	return m, err
}

func listSessions(g *gitRepo) error {
	ids, err := sessionIDs(g)
	if err != nil {
		return err
	}
	for _, sid := range ids {
		m, err := readMeta(g, sid)
		if err != nil {
			fmt.Printf("%s\t(unreadable meta: %v)\n", sid, err)
			continue
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", sid, m.Start, m.Author, m.FirstPrompt)
	}
	return nil
}

func removeSession(g *gitRepo, sid, cwd string) error {
	refs, err := g.refNames(recordRefPrefix + sid)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return fmt.Errorf("no refs for session %s", sid)
	}
	for _, ref := range refs {
		if err := g.deleteRef(ref); err != nil {
			return err
		}
	}
	if cacheDir, err := cacheDirFor(cwd); err == nil {
		os.Remove(filepath.Join(cacheDir, "record", sid+".state"))
		os.Remove(filepath.Join(cacheDir, "record", sid+".index"))
	}
	fmt.Printf("deleted %d refs for session %s\n", len(refs), sid)
	fmt.Println("objects remain until `git gc --prune`; run it to reclaim space")
	return nil
}

// treeRefs returns the session's checkpoint refs, chronologically sorted
// (the embedded timestamp makes refname order chronological).
func treeRefs(g *gitRepo, sid string) ([]string, error) {
	return g.refNames(recordRefPrefix + sid + "/tree/")
}

// checkpointBases returns the parents of checkpoint commits that are not
// themselves checkpoints — i.e. the real-history commits the session was
// based on (session-start HEAD plus any HEAD moved mid-session).
func checkpointBases(g *gitRepo, sid string) ([]string, error) {
	refs, err := treeRefs(g, sid)
	if err != nil {
		return nil, err
	}
	checkpoints := map[string]bool{}
	for _, ref := range refs {
		checkpoints[g.readRef(ref)] = true
	}
	basesSeen := map[string]bool{}
	var bases []string
	for sha := range checkpoints {
		parents, err := g.run("log", "-1", "--format=%P", sha)
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Fields(parents) {
			if !checkpoints[p] && !basesSeen[p] {
				basesSeen[p] = true
				bases = append(bases, p)
			}
		}
	}
	return bases, nil
}

// resolveUpstream picks the ref bounding bundles: the configured value, or
// origin's default branch, falling back to origin/master.
func resolveUpstream(g *gitRepo, configured string) (string, error) {
	candidates := []string{configured}
	if head, err := g.run("symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil && head != "" {
		candidates = append(candidates, head)
	}
	candidates = append(candidates, "origin/master", "origin/main")
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := g.run("rev-parse", "--verify", "--quiet", c+"^{commit}"); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no upstream ref found (tried origin's default branch); set --record-upstream")
}

// bundleBase computes the --not for a session bundle: the earliest
// merge-base between the recorded checkout parents and upstream.
func bundleBase(g *gitRepo, sid, upstream string) (string, error) {
	bases, err := checkpointBases(g, sid)
	if err != nil {
		return "", err
	}
	if len(bases) == 0 {
		return "", nil // transcript-only session: rootless, self-contained
	}
	up, err := resolveUpstream(g, upstream)
	if err != nil {
		return "", err
	}
	earliest := ""
	for _, b := range bases {
		mb, err := g.run("merge-base", up, b)
		if err != nil {
			return "", fmt.Errorf("no merge-base between %s and session base %s: %w (use --record-upstream, or push to a remote instead of bundling)", up, b, err)
		}
		if earliest == "" || g.isAncestor(mb, earliest) {
			earliest = mb
		}
	}
	return earliest, nil
}

// looksLikeRemote reports whether dest names a git remote or URL rather
// than a filesystem path for a bundle.
func looksLikeRemote(g *gitRepo, dest string) bool {
	if strings.Contains(dest, "://") || strings.HasPrefix(dest, "git@") {
		return true
	}
	_, err := g.run("remote", "get-url", dest)
	return err == nil
}

func uploadSession(g *gitRepo, sid, dest, upstream string) error {
	refs, err := g.refNames(recordRefPrefix + sid)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return fmt.Errorf("no refs for session %s", sid)
	}
	fmt.Println("note: recordings contain the full transcript (tool output, file contents) and all touched files; treat uploads as sensitive")
	if err := reportNeverCommitted(g, sid, upstream); err != nil {
		return err
	}
	if looksLikeRemote(g, dest) {
		refspec := fmt.Sprintf("%s%s/*:%s%s/*", recordRefPrefix, sid, recordRefPrefix, sid)
		if _, err := g.run("push", dest, refspec); err != nil {
			return err
		}
		fmt.Printf("pushed session %s to %s\n", sid, dest)
		return nil
	}
	base, err := bundleBase(g, sid, upstream)
	if err != nil {
		return err
	}
	path := dest
	if fi, err := os.Stat(dest); err == nil && fi.IsDir() {
		path = filepath.Join(dest, sid+".bundle")
	}
	args := []string{"bundle", "create", path, "--glob=" + recordRefPrefix + sid + "/*"}
	if base != "" {
		args = append(args, "--not", base)
	}
	if _, err := g.run(args...); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", path)
	if base != "" {
		fmt.Printf("bundle prerequisite: %s (receivers need this commit; it is on the upstream)\n", base)
	}
	return nil
}

// reportNeverCommitted warns about files in the recording that were not in
// the session's base commit — untracked-but-not-ignored files (potential
// secrets) travel with the snapshots.
func reportNeverCommitted(g *gitRepo, sid, upstream string) error {
	refs, err := treeRefs(g, sid)
	if err != nil || len(refs) == 0 {
		return err
	}
	latest := refs[len(refs)-1]
	bases, err := checkpointBases(g, sid)
	if err != nil || len(bases) == 0 {
		return err
	}
	added, err := g.run("diff", "--name-only", "--diff-filter=A", bases[0], g.readRef(latest))
	if err != nil || added == "" {
		return err
	}
	fmt.Println("files in the recording that are not in the session's base commit:")
	for _, f := range strings.Split(added, "\n") {
		fmt.Printf("  %s\n", f)
	}
	return nil
}

func downloadSession(g *gitRepo, src, sid, at, dest, cwd, home string) error {
	if fi, err := os.Stat(src); err == nil && !fi.IsDir() {
		if _, err := g.run("bundle", "verify", src); err != nil {
			return fmt.Errorf("%w\n(missing prerequisites? fetch the upstream branch first)", err)
		}
	}
	before, err := g.refNames(recordRefPrefix)
	if err != nil {
		return err
	}
	if _, err := g.run("fetch", src, recordRefPrefix+"*:"+recordRefPrefix+"*"); err != nil {
		return err
	}
	sid, err = pickDownloadedSession(g, sid, before)
	if err != nil {
		return err
	}
	refs, err := treeRefs(g, sid)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return fmt.Errorf("session %s has no tree checkpoints", sid)
	}
	ref := refs[len(refs)-1]
	if at != "" {
		ref = ""
		for _, r := range refs {
			if strings.Contains(strings.TrimPrefix(r, recordRefPrefix+sid+"/tree/"), at) {
				ref = r
			}
		}
		if ref == "" {
			return fmt.Errorf("no checkpoint matching --at %q; available:\n  %s", at, strings.Join(refs, "\n  "))
		}
	}
	if dest == "" {
		short := sid
		if len(short) > 8 {
			short = short[:8]
		}
		dest = filepath.Join(filepath.Dir(cwd), filepath.Base(cwd)+"-review-"+short)
	}
	if _, err := g.run("worktree", "add", "--detach", dest, g.readRef(ref)); err != nil {
		return err
	}
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}

	// Materialize the transcript so `claude --resume` finds it from the
	// new worktree. --fork-session is mandatory on replay: continuing the
	// original session id would record onto the author's refs.
	transcript, err := g.showBlob(recordRefPrefix+sid+"/transcript", "transcript.jsonl")
	if err != nil {
		return err
	}
	tdir := transcriptDir(home, destAbs)
	if err := os.MkdirAll(tdir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tdir, sid+".jsonl"), []byte(transcript+"\n"), 0600); err != nil {
		return err
	}
	// Mark the session foreign in the worktree's cache so a recorder there
	// never extends the author's refs.
	if cacheDir, err := cacheDirFor(destAbs); err == nil {
		recDir := filepath.Join(cacheDir, "record")
		if err := os.MkdirAll(recDir, 0700); err == nil {
			data, _ := json.Marshal(recordState{Foreign: true})
			os.WriteFile(filepath.Join(recDir, sid+".state"), data, 0600)
		}
	}

	fmt.Printf("session %s checked out at %s (checkpoint %s)\n", sid, dest, strings.TrimPrefix(ref, recordRefPrefix+sid+"/tree/"))
	fmt.Printf("replay: cd %s && runclaude -- claude --resume %s --fork-session\n", dest, sid)
	fmt.Printf("step checkpoints: git diff <ref-A> <ref-B> over refs under %s%s/tree/\n", recordRefPrefix, sid)
	return nil
}

// pickDownloadedSession resolves which session to check out: the explicit
// flag, a single newly fetched session, or a single session overall.
func pickDownloadedSession(g *gitRepo, sid string, beforeRefs []string) (string, error) {
	if sid != "" {
		return sid, nil
	}
	before := map[string]bool{}
	for _, r := range beforeRefs {
		before[r] = true
	}
	after, err := g.refNames(recordRefPrefix)
	if err != nil {
		return "", err
	}
	newSids := map[string]bool{}
	allSids := map[string]bool{}
	for _, r := range after {
		rest := strings.TrimPrefix(r, recordRefPrefix)
		s, _, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		allSids[s] = true
		if !before[r] {
			newSids[s] = true
		}
	}
	pick := newSids
	if len(pick) == 0 {
		pick = allSids
	}
	if len(pick) != 1 {
		var ids []string
		for s := range pick {
			ids = append(ids, s)
		}
		return "", fmt.Errorf("multiple sessions available, pass --session: %s", strings.Join(ids, " "))
	}
	for s := range pick {
		return s, nil
	}
	return "", fmt.Errorf("no sessions found")
}
