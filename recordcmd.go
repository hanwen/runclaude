package main

// Host-only commands operating on recorded sessions: the list/upload/
// download/rm subcommands. These run before (instead of) any sandbox setup.

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// dispatchSubcommand handles `runclaude <verb> ...` session-management
// subcommands. The verb must be the first argument; `runclaude -- <verb>`
// escapes a sandbox command that happens to share a verb's name. Host-only
// verbs run here (handled=true, caller exits); "new" and "resume" are the
// normal sandbox path with recording forced on, so they only strip the
// verb (resume also its session id, resolved into resumeSid) and set
// record — rest feeds the regular flag parsing.
func dispatchSubcommand(args []string) (handled, record bool, resumeSid string, rest []string, err error) {
	if len(args) == 0 {
		return false, false, "", args, nil
	}
	switch args[0] {
	case "new":
		return false, true, "", args[1:], nil
	case "resume":
		sid, rest, err := resumeSubcommand(args[1:])
		if err != nil {
			return true, false, "", nil, err
		}
		return false, true, sid, rest, nil
	case "list", "upload", "download", "rm":
	default:
		return false, false, "", args, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return true, false, "", nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return true, false, "", nil, err
	}
	return true, false, "", nil, runSessionSubcommand(args[0], args[1:], cwd, home)
}

// resumeSubcommand picks the session id for the resume verb: an explicit
// first argument, or — when absent or "latest" — the newest recorded
// session of cwd's repo. An explicit id needs no repo at all: it is
// claude's to resolve.
func resumeSubcommand(args []string) (sid string, rest []string, err error) {
	rest = args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		sid, rest = rest[0], rest[1:]
	}
	if sid != "" && sid != "latest" {
		return sid, rest, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", nil, err
	}
	gitDir, ok := resolveRecordGitDir(cwd)
	if !ok {
		return "", nil, fmt.Errorf("resume: no git or jj repository at %s", cwd)
	}
	sid, err = resolveSessionID(&gitRepo{gitDir: gitDir}, "latest")
	if err != nil {
		return "", nil, err
	}
	return sid, rest, nil
}

// runSessionSubcommand executes a host-only session verb against cwd's repo.
func runSessionSubcommand(sub string, args []string, cwd, home string) error {
	gitDir, ok := resolveRecordGitDir(cwd)
	if !ok {
		return fmt.Errorf("no git or jj repository at %s", cwd)
	}
	g := &gitRepo{gitDir: gitDir}
	switch sub {
	case "list":
		if len(args) > 0 {
			return fmt.Errorf("usage: runclaude list")
		}
		return listSessions(g)
	case "rm":
		if len(args) == 0 {
			return fmt.Errorf("usage: runclaude rm <session-id>...")
		}
		for _, sid := range args {
			if err := removeSession(g, sid); err != nil {
				return err
			}
		}
		return nil
	case "upload":
		fs := flag.NewFlagSet("runclaude upload", flag.ExitOnError)
		session := fs.String("session", "", "session id (default: latest)")
		upstream := fs.String("upstream", fileRecordUpstream(cwd),
			"upstream ref bounding the bundle (default: origin's default branch)")
		fs.Parse(args)
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: runclaude upload [--session <sid>] [--upstream <ref>] <dir|bundle|remote>")
		}
		sid, err := resolveSessionID(g, *session)
		if err != nil {
			return err
		}
		return uploadSession(g, sid, fs.Arg(0), *upstream)
	case "download":
		fs := flag.NewFlagSet("runclaude download", flag.ExitOnError)
		session := fs.String("session", "", "session id (default: the newly fetched one)")
		at := fs.String("at", "", "checkpoint selector: uuid or timestamp substring (default: latest)")
		dest := fs.String("dest", "", "worktree directory (default: <repo>-review-<sid>)")
		fs.Parse(args)
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: runclaude download [--session <sid>] [--at <sel>] [--dest <dir>] <bundle|remote>")
		}
		return downloadSession(g, fs.Arg(0), *session, *at, *dest, cwd, home)
	}
	return fmt.Errorf("unknown subcommand %q", sub)
}

// fileRecordUpstream reads the project options file's recordUpstream — the
// same default the --record-upstream flag gets on the sandbox path.
func fileRecordUpstream(cwd string) string {
	opts, err := loadOptionsFile(cwd)
	if err != nil {
		return ""
	}
	return opts.RecordUpstream
}

// autoForkSession returns ["--fork-session"] when args resume a session
// that was recorded by a different clone (e.g. fetched via download):
// continuing the author's session id would collide with their refs. A
// session recorded by this clone continues — from any of its worktrees —
// and stickiness keeps recording it.
func autoForkSession(cwd string, args []string) []string {
	sid := resumeArg(args)
	if sid == "" {
		return nil
	}
	for _, a := range args {
		if a == "--fork-session" {
			return nil
		}
	}
	gitDir, ok := resolveRecordGitDir(cwd)
	if !ok {
		return nil
	}
	g := &gitRepo{gitDir: gitDir, workTree: cwd}
	m, err := readMeta(g, sid)
	if err != nil {
		return nil // not a recorded session: claude's own resume semantics
	}
	id, err := recorderID(g)
	if err != nil || m.RecorderID == "" || m.RecorderID == id {
		return nil
	}
	log.Printf("record: session %s was recorded by another clone; forking (--fork-session)", sid)
	return []string{"--fork-session"}
}

// resumeArg extracts the session id from claude CLI resume flags.
func resumeArg(args []string) string {
	for i, a := range args {
		switch {
		case a == "--resume" || a == "-r":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1]
			}
		case strings.HasPrefix(a, "--resume="):
			return strings.TrimPrefix(a, "--resume=")
		}
	}
	return ""
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
		return "", fmt.Errorf("no recorded sessions (start one with `runclaude new`)")
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

func removeSession(g *gitRepo, sid string) error {
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
	if common, err := g.commonGitDir(); err == nil {
		stateDir := filepath.Join(common, "runclaude", "record")
		os.Remove(filepath.Join(stateDir, sid+".state"))
		os.Remove(filepath.Join(stateDir, sid+".index"))
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

	// Materialize the transcript into the repo-scoped sessions dir so
	// `claude --resume` (under runclaude) finds it from any worktree of
	// this clone. Replaying under runclaude forks automatically when the
	// session was recorded by another clone (see autoForkSession); the
	// local recorder never extends foreign refs (recorder id mismatch).
	transcript, err := g.blobBytes(recordRefPrefix+sid+"/meta", "transcript.jsonl")
	if err != nil {
		return err
	}
	common, err := g.commonGitDir()
	if err != nil {
		return err
	}
	tdir := repoSessionsDir(home, destAbs, common)
	if err := os.MkdirAll(tdir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tdir, sid+".jsonl"), transcript, 0600); err != nil {
		return err
	}

	fmt.Printf("session %s checked out at %s (checkpoint %s)\n", sid, dest, strings.TrimPrefix(ref, recordRefPrefix+sid+"/tree/"))
	fmt.Printf("replay: cd %s && runclaude resume %s\n", dest, sid)
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
