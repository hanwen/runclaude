package main

// Session recording (--record): correlate Claude Code transcript positions
// with filesystem snapshots so a session can be uploaded and reviewed as
// "conversation + tree state at each step" (see docs/session-recording.md).
//
// The recorder runs on the host (mainErr) and tails the session transcript
// JSONL that the sandboxed claude writes into the live-bound
// ~/.claude/projects/<encoded-cwd>/ dir. Per mutating tool result it
// snapshots the worktree as a commit at
//
//	refs/runclaude/<sid>/tree/<utc-ts>/<msg-uuid>
//
// and maintains a parentless session branch
//
//	refs/runclaude/<sid>/meta   (meta.json + transcript.jsonl)
//
// coalescing same-second events into one commit. meta.json (author,
// recorder id, start, first prompt) is written once and reused as the same
// blob in every commit; it drives --sessions discovery and scopes
// stickiness to the recording clone. transcript.jsonl is the session file
// truncated at the parse offset per flush.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const recordRefPrefix = "refs/runclaude/"

// mutatingTools are the tool names whose results trigger a tree snapshot.
var mutatingTools = map[string]bool{
	"Bash":         true,
	"Edit":         true,
	"Write":        true,
	"MultiEdit":    true,
	"NotebookEdit": true,
}

// encodeProjectDir mirrors Claude Code's encoding of a cwd into a
// ~/.claude/projects/ subdirectory name: every byte outside [A-Za-z0-9]
// becomes '-'.
func encodeProjectDir(cwd string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, cwd)
}

// transcriptDir returns the host-side directory claude writes session
// transcripts for cwd into.
func transcriptDir(home, cwd string) string {
	return filepath.Join(home, ".claude", "projects", encodeProjectDir(cwd))
}

// recordState is the cached per-session recorder state. It is advisory:
// the authoritative resume point is the size of the tip's transcript blob.
type recordState struct {
	Offset         int64  `json:"offset"`
	LastTree       string `json:"lastTree,omitempty"`
	LastCheckpoint string `json:"lastCheckpoint,omitempty"`
	Tip            string `json:"tip,omitempty"` // <sid>/meta branch tip
	// Foreign marks sessions fetched via --download; the local recorder
	// never extends them (doing so would fork the author's refs).
	Foreign bool `json:"foreign,omitempty"`
}

type sessionMeta struct {
	SessionID string `json:"sessionId"`
	// RecorderID identifies the clone that recorded the session (see
	// recorderID); deliberately not the checkout path, which changes on
	// moves and would leak the author's filesystem layout.
	RecorderID  string `json:"recorderId"`
	Author      string `json:"author,omitempty"`
	Start       string `json:"start"`
	Upstream    string `json:"upstream,omitempty"`
	FirstPrompt string `json:"firstPrompt,omitempty"`
}

// transcriptEntry is the subset of a Claude Code session JSONL line the
// recorder needs. The format is internal to Claude Code and drifts;
// unknown fields and entry types must pass through silently.
type transcriptEntry struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	Timestamp string `json:"timestamp"`
	Message   *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

// contentBlock is one element of message.content when it is an array.
type contentBlock struct {
	Type      string `json:"type"`
	Name      string `json:"name"`        // tool_use
	ID        string `json:"id"`          // tool_use
	ToolUseID string `json:"tool_use_id"` // tool_result
	Text      string `json:"text"`
}

type recorder struct {
	git         *gitRepo
	id          string // per-clone recording identity (recorderID)
	projectsDir string // host dir with <sid>.jsonl files
	stateDir    string // <cacheDir>/record
	recordAll   bool   // --record: claim sessions started under this run
	upstream    string // configured upstream for meta
	start       time.Time
	// preexisting holds session ids whose files already existed at
	// recorder start; --record only claims sessions newer than that
	// (sticky sessions are claimed regardless).
	preexisting map[string]bool
	logger      *log.Logger
	now         func() time.Time // test hook

	mu       sync.Mutex
	sessions map[string]*recSession
	done     chan struct{}
	wg       sync.WaitGroup
}

type recSession struct {
	sid       string
	path      string
	lockFile  *os.File // holds the flock; nil only in tests
	state     recordState
	toolNames map[string]string // tool_use id -> tool name
	// pending state for transcript coalescing
	pendingUUIDs []string
	firstPrompt  string
	lastFlush    time.Time
	metaBlob     string // write-once meta.json blob, reused per commit
	size         int64  // last observed file size
}

func newRecorder(gitDir, cwd, home, cacheDir string, recordAll bool, upstream string, extraExcludes []string, logger *log.Logger) (*recorder, error) {
	stateDir := filepath.Join(cacheDir, "record")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	// Recorder-side excludes: the container's vcsExcludeBinds overlay is
	// invisible on the host, so hide claude's runtime settings.json
	// mutations here too, plus user-configured patterns.
	patterns := append([]string{".claude/settings.json"}, extraExcludes...)
	excludesFile := filepath.Join(stateDir, "exclude")
	if err := os.WriteFile(excludesFile, []byte(strings.Join(patterns, "\n")+"\n"), 0600); err != nil {
		return nil, err
	}
	git := &gitRepo{
		gitDir:       gitDir,
		workTree:     cwd,
		excludesFile: excludesFile,
	}
	git.ensureIdent()
	id, err := recorderID(git)
	if err != nil {
		return nil, fmt.Errorf("recorder id: %w", err)
	}
	projectsDir := transcriptDir(home, cwd)
	preexisting := map[string]bool{}
	if entries, err := os.ReadDir(projectsDir); err == nil {
		for _, e := range entries {
			if name, ok := strings.CutSuffix(e.Name(), ".jsonl"); ok {
				preexisting[name] = true
			}
		}
	}
	return &recorder{
		git:         git,
		id:          id,
		preexisting: preexisting,
		projectsDir: projectsDir,
		stateDir:    stateDir,
		recordAll:   recordAll,
		upstream:    upstream,
		start:       time.Now(),
		logger:      logger,
		now:         time.Now,
		sessions:    map[string]*recSession{},
		done:        make(chan struct{}),
	}, nil
}

// run polls until Close. Call as a goroutine.
func (r *recorder) run() {
	r.wg.Add(1)
	defer r.wg.Done()
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-tick.C:
			r.scanOnce()
		}
	}
}

// Close stops polling, takes a final flush, and repacks if anything was
// recorded. It returns the recorded session ids so the caller can print a
// summary once the terminal is free again.
func (r *recorder) Close() []string {
	close(r.done)
	r.wg.Wait()
	r.scanOnce()
	var recorded []string
	r.mu.Lock()
	for _, s := range r.sessions {
		r.flushTranscript(s, true)
		r.saveState(s)
		if s.lockFile != nil {
			s.lockFile.Close()
		}
		recorded = append(recorded, s.sid)
	}
	r.mu.Unlock()
	if len(recorded) > 0 {
		if err := r.git.repack(); err != nil {
			r.logger.Printf("record: repack: %v", err)
		}
	}
	return recorded
}

// scanOnce looks for new/grown session files and processes them.
func (r *recorder) scanOnce() {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, err := os.ReadDir(r.projectsDir)
	if err != nil {
		return // dir may not exist yet
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") || e.IsDir() {
			continue
		}
		sid := strings.TrimSuffix(name, ".jsonl")
		path := filepath.Join(r.projectsDir, name)
		s := r.sessions[sid]
		if s == nil {
			s = r.maybeClaim(sid, path)
			if s == nil {
				continue
			}
			r.sessions[sid] = s
		}
		r.pollSession(s)
		// Transcript coalescing: flush when at least a second has passed
		// since the last flush and events are pending.
		r.flushTranscript(s, false)
	}
}

// maybeClaim decides whether this recorder owns sid and, if so, locks and
// initializes it. Ownership rules: (a) sticky — the session branch exists,
// its meta.json recorder id is this clone's, and the session is not
// foreign; or (b) --record was
// given and the session file appeared after this recorder started.
func (r *recorder) maybeClaim(sid, path string) *recSession {
	sticky := r.isSticky(sid)
	// Fresh = the session file did not exist when this recorder started.
	// (Not an mtime comparison: file timestamps come from the kernel's
	// coarse clock and can lag time.Now() by a scheduler tick.)
	fresh := r.recordAll && !r.preexisting[sid]
	if !sticky && !fresh {
		return nil
	}
	lockPath := filepath.Join(r.stateDir, sid+".state")
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		r.logger.Printf("record: %s: %v", sid, err)
		return nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close() // another recorder owns this session
		return nil
	}
	s := &recSession{
		sid:       sid,
		path:      path,
		lockFile:  f,
		toolNames: map[string]string{},
	}
	// A recorder killed mid-`add` leaves a stale index lock that would
	// stall every future snapshot. We hold the flock, so nobody else is
	// live on this session: safe to clear.
	os.Remove(filepath.Join(r.stateDir, sid+".index.lock"))
	if data, err := os.ReadFile(lockPath); err == nil && len(data) > 0 {
		json.Unmarshal(data, &s.state)
	}
	if s.state.Foreign {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil
	}
	// The state file is a cache; the tip's transcript blob is authoritative
	// for the resume offset — its size is exactly the bytes recorded, since
	// flushes truncate at the parse point (crash between update-ref and
	// state write must not duplicate or skip content).
	if tip := r.git.readRef(recordRefPrefix + sid + "/meta"); tip != "" {
		s.state.Tip = tip
		if n, err := r.git.blobSize(tip, "transcript.jsonl"); err == nil && n > s.state.Offset {
			s.state.Offset = n
		}
		if blob, err := r.git.run("rev-parse", "--verify", "--quiet", tip+":meta.json"); err == nil {
			s.metaBlob = blob
		}
	}
	// File logger only: claude's TUI owns the terminal while sessions are
	// claimed; the recorded-session summary is printed by mainErr after
	// the container exits.
	r.logger.Printf("recording session %s", sid)
	return s
}

// isSticky reports whether sid was previously recorded by this clone.
func (r *recorder) isSticky(sid string) bool {
	metaJSON, err := r.git.showBlob(recordRefPrefix+sid+"/meta", "meta.json")
	if err != nil {
		return false // no session branch: never recorded
	}
	var m sessionMeta
	if json.Unmarshal([]byte(metaJSON), &m) != nil {
		return false
	}
	// Only the recording clone resumes a session: a reviewer's clone (refs
	// fetched, transcript materialized by --download) has a different id
	// and must not extend the author's branch.
	return m.RecorderID != "" && m.RecorderID == r.id
}

// recorderID returns this clone's stable recording identity, minting it on
// first use. It lives in the common git dir, so it survives repo moves and
// is shared by linked worktrees — but is local-only: clone/fetch never
// transfer it.
func recorderID(g *gitRepo) (string, error) {
	common, err := g.run("rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(common) {
		// rev-parse resolves relative to the process cwd (the worktree).
		common = filepath.Join(g.workTree, common)
	}
	path := filepath.Join(common, "runclaude-recorder-id")
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		return strings.TrimSpace(string(data)), nil
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(id+"\n"), 0600); err != nil {
		return "", err
	}
	return id, nil
}

// pollSession reads newly appended bytes and reacts to complete entries.
func (r *recorder) pollSession(s *recSession) {
	st, err := os.Stat(s.path)
	if err != nil {
		return
	}
	if st.Size() < s.state.Offset {
		// File shrank (rewrite/compaction): re-baseline rather than tail
		// garbage. The skipped range gets no tree refs; the next transcript
		// flush records the full new content.
		r.logger.Printf("record: %s: transcript shrank (%d -> %d), re-baselining", s.sid, s.state.Offset, st.Size())
		s.state.Offset = 0
		s.size = -1 // force a read even if the rewrite kept the size
	}
	if st.Size() == s.size {
		return // no new bytes since the last look
	}
	s.size = st.Size()

	f, err := os.Open(s.path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(s.state.Offset, 0); err != nil {
		return
	}
	buf := make([]byte, st.Size()-s.state.Offset)
	n, _ := f.Read(buf)
	// Consume only through the last newline; an incomplete trailing line
	// stays in the file past the offset and is re-read once it completes.
	end := bytes.LastIndexByte(buf[:n], '\n')
	if end < 0 {
		return
	}
	data := buf[:end+1]
	newOffset := s.state.Offset + int64(end+1)

	var snapshotUUIDs []string
	var lastTS string
	for {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			break
		}
		line := data[:nl]
		data = data[nl+1:]
		if len(line) == 0 {
			continue
		}
		uuid, ts, mutating := r.handleEntry(s, line)
		if uuid != "" {
			s.pendingUUIDs = append(s.pendingUUIDs, uuid)
		}
		if mutating {
			snapshotUUIDs = append(snapshotUUIDs, uuid)
			lastTS = ts
		}
	}

	if len(snapshotUUIDs) > 0 {
		r.snapshot(s, snapshotUUIDs, lastTS, newOffset)
	}
	s.state.Offset = newOffset
	r.saveState(s)
}

// handleEntry parses one JSONL line. It returns the entry uuid (if any),
// its timestamp, and whether it should trigger a tree snapshot.
func (r *recorder) handleEntry(s *recSession, line []byte) (uuid, ts string, mutating bool) {
	var e transcriptEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return "", "", false // unknown/partial format: pass through
	}
	if e.UUID == "" {
		return "", "", false // bookkeeping entry (mode, last-prompt, ...)
	}
	if e.Message == nil || len(e.Message.Content) == 0 {
		return e.UUID, e.Timestamp, false
	}
	var blocks []contentBlock
	if err := json.Unmarshal(e.Message.Content, &blocks); err != nil {
		// content can be a plain string (user prompt)
		var text string
		if json.Unmarshal(e.Message.Content, &text) == nil && e.Type == "user" && s.firstPrompt == "" {
			s.firstPrompt = truncate(text, 200)
		}
		return e.UUID, e.Timestamp, false
	}
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			if b.ID != "" {
				s.toolNames[b.ID] = b.Name
			}
		case "tool_result":
			if mutatingTools[s.toolNames[b.ToolUseID]] {
				mutating = true
			}
		case "text":
			if e.Type == "assistant" {
				// Turn-final text: snapshot so the turn boundary state is
				// recorded even when the last tool was non-mutating.
				mutating = true
			} else if e.Type == "user" && s.firstPrompt == "" {
				s.firstPrompt = truncate(b.Text, 200)
			}
		}
	}
	return e.UUID, e.Timestamp, mutating
}

// snapshot commits the current worktree and points one ref per triggering
// uuid at it (same-second events share the commit, mirroring transcript
// smushing). Unchanged trees create no commit.
func (r *recorder) snapshot(s *recSession, uuids []string, entryTS string, offset int64) {
	// Per-session private index: never touches the user's staging area,
	// persists across flushes for incremental restats.
	g := *r.git
	g.indexFile = filepath.Join(r.stateDir, s.sid+".index")
	tree, err := g.snapshotTree()
	if err != nil {
		r.logger.Printf("record: %s: snapshot: %v", s.sid, err)
		return
	}
	if tree == s.state.LastTree {
		return
	}
	var parents []string
	head := r.git.headCommit()
	if s.state.LastCheckpoint == "" {
		if head != "" {
			parents = append(parents, head)
		}
	} else {
		parents = append(parents, s.state.LastCheckpoint)
		// Pin a moved HEAD as second parent only when it is genuinely new
		// history (jj moves HEAD on nearly every command; reachable HEADs
		// would spray merge parents across every checkpoint).
		if head != "" && !r.git.isAncestor(head, s.state.LastCheckpoint) {
			parents = append(parents, head)
		}
	}
	msg := fmt.Sprintf("runclaude checkpoint\n\nsession: %s\nuuids: %s\nspan: %d-%d\n",
		s.sid, strings.Join(uuids, " "), s.state.Offset, offset)
	sha, err := r.git.commitTree(tree, parents, msg)
	if err != nil {
		r.logger.Printf("record: %s: commit: %v", s.sid, err)
		return
	}
	ts := refTimestamp(entryTS, r.now())
	for _, u := range uuids {
		ref := fmt.Sprintf("%s%s/tree/%s/%s", recordRefPrefix, s.sid, ts, u)
		if err := r.git.updateRef(ref, sha); err != nil {
			r.logger.Printf("record: %s: %v", s.sid, err)
		}
	}
	s.state.LastTree = tree
	s.state.LastCheckpoint = sha
}

// flushTranscript commits meta.json + the transcript onto the session
// branch (<sid>/meta). Events within the same second coalesce into one
// commit; force flushes regardless (session end). The transcript blob is
// truncated at the parse offset, so it always ends on a complete JSONL
// line and its size IS the resume offset — nothing machine-readable lives
// in the commit message. (A trailing line that never completes is
// unparseable and stays out of the recording.)
func (r *recorder) flushTranscript(s *recSession, force bool) {
	if len(s.pendingUUIDs) == 0 {
		return
	}
	if !force && r.now().Sub(s.lastFlush) < time.Second {
		return
	}
	if s.metaBlob == "" {
		blob, err := r.metaJSONBlob(s)
		if err != nil {
			r.logger.Printf("record: %s: meta blob: %v", s.sid, err)
			return
		}
		s.metaBlob = blob
	}
	blob, err := r.git.hashFilePrefix(s.path, s.state.Offset)
	if err != nil {
		r.logger.Printf("record: %s: transcript blob: %v", s.sid, err)
		return
	}
	tree, err := r.git.blobTree(map[string]string{
		"meta.json":        s.metaBlob,
		"transcript.jsonl": blob,
	})
	if err != nil {
		r.logger.Printf("record: %s: %v", s.sid, err)
		return
	}
	var parents []string
	if s.state.Tip != "" {
		parents = append(parents, s.state.Tip)
	}
	sha, err := r.git.commitTree(tree, parents, "runclaude session")
	if err != nil {
		r.logger.Printf("record: %s: session commit: %v", s.sid, err)
		return
	}
	if err := r.git.updateRef(recordRefPrefix+s.sid+"/meta", sha); err != nil {
		r.logger.Printf("record: %s: %v", s.sid, err)
		return
	}
	s.state.Tip = sha
	s.pendingUUIDs = nil
	s.lastFlush = r.now()
	r.saveState(s)
}

// metaJSONBlob writes the session's write-once meta.json blob.
func (r *recorder) metaJSONBlob(s *recSession) (string, error) {
	meta := sessionMeta{
		SessionID:   s.sid,
		RecorderID:  r.id,
		Author:      gitAuthor(r.git),
		Start:       r.start.UTC().Format(time.RFC3339),
		Upstream:    r.upstream,
		FirstPrompt: s.firstPrompt,
	}
	data, _ := json.MarshalIndent(meta, "", "\t")
	tmp := filepath.Join(r.stateDir, s.sid+".meta.json")
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return "", err
	}
	return r.git.hashFile(tmp)
}

func (r *recorder) saveState(s *recSession) {
	data, _ := json.Marshal(s.state)
	if s.lockFile != nil {
		s.lockFile.Truncate(0)
		s.lockFile.WriteAt(data, 0)
	}
}

func gitAuthor(g *gitRepo) string {
	name, _ := g.run("config", "user.name")
	email, _ := g.run("config", "user.email")
	switch {
	case name != "" && email != "":
		return name + " <" + email + ">"
	case name != "":
		return name
	default:
		return email
	}
}

// refTimestamp renders an entry timestamp (RFC3339) as a refname-safe
// sortable UTC stamp, falling back to now.
func refTimestamp(entryTS string, now time.Time) string {
	t := now
	if parsed, err := time.Parse(time.RFC3339, entryTS); err == nil {
		t = parsed
	}
	return t.UTC().Format("20060102T150405Z")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
