package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/hanwen/runclaude/creds"
	"github.com/hanwen/runclaude/proxy"
)

const (
	prSetNoNewPrivs      = 38
	prCapBsetDrop        = 24
	prCapAmbient         = 47
	prCapAmbientClearAll = 4
)

func dropAllCaps() error {
	if _, _, e := syscall.Syscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); e != 0 {
		return fmt.Errorf("PR_SET_NO_NEW_PRIVS: %w", e)
	}
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("PR_CAP_AMBIENT_CLEAR_ALL: %w", e)
	}
	for i := uintptr(0); i < 64; i++ {
		// EINVAL means cap doesn't exist; stop iterating.
		if _, _, e := syscall.Syscall(syscall.SYS_PRCTL, prCapBsetDrop, i, 0); e == syscall.EINVAL {
			break
		}
	}
	type header struct {
		version uint32
		pid     int32
	}
	type data struct {
		effective, permitted, inheritable uint32
	}
	hdr := header{version: 0x20080522} // _LINUX_CAPABILITY_VERSION_3
	var d [2]data
	if _, _, e := syscall.Syscall(syscall.SYS_CAPSET,
		uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&d[0])), 0); e != 0 {
		return fmt.Errorf("capset: %w", e)
	}
	return nil
}

const (
	childEnv = "_RUNCLAUDE_CHILD"
	initEnv  = "_RUNCLAUDE_INIT"
)

type Config struct {
	Rootfs         string   `json:"rootfs"`
	Home           string   `json:"home"`
	CacheDir       string   `json:"cacheDir"`
	Cwd            string   `json:"cwd"`
	Binds          []Bind   `json:"binds"`
	Command        []string `json:"command"`
	RestrictNet    bool     `json:"restrictNet"`
	AllowedDomains []string `json:"allowedDomains"`
	MitmDomains    []string `json:"mitmDomains"`
	// MitmUpstream maps a MITM-intercepted host to an override upstream URL
	// (e.g., "api.anthropic.com" -> "http://127.0.0.1:8443"). Used by tests
	// to point the proxy at a fake server instead of the real provider.
	MitmUpstream map[string]string `json:"mitmUpstream,omitempty"`
	BundlePath   string            `json:"bundlePath"`
	CAPath       string            `json:"caPath"`
}

type Bind struct {
	Path string `json:"path"`
	// Dest is the in-container path; defaults to Path when empty. Use it
	// to overlay a host-side sanitized copy at a path inside an already-
	// bound tree without modifying the host original.
	Dest     string `json:"dest,omitempty"`
	ReadOnly bool   `json:"ro,omitempty"`
	// Tmpfs mounts a fresh, empty, per-container tmpfs at Dest instead of
	// bind-mounting Path. Used for ephemeral scratch dirs (see claudeScratch)
	// so each invocation gets its own copy with no sharing across concurrent
	// or sequential sessions — even when they share a per-cwd cache home.
	// When set, Path is ignored and Dest must be non-empty.
	Tmpfs bool `json:"tmpfs,omitempty"`
}

func (b Bind) dest() string {
	if b.Dest != "" {
		return b.Dest
	}
	return b.Path
}

func parseMitmUpstream(items []string) map[string]string {
	if len(items) == 0 {
		return nil
	}
	out := make(map[string]string, len(items))
	for _, kv := range items {
		host, url, ok := strings.Cut(kv, "=")
		if !ok || host == "" || url == "" {
			log.Fatalf("mitm upstream %q: expected host=url", kv)
		}
		out[host] = url
	}
	return out
}

type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// printVisibleDefaults is fs.PrintDefaults minus the test.* flags, which
// exist for the test scripts and would only clutter the help output.
func printVisibleDefaults(fs *flag.FlagSet) {
	visible := flag.NewFlagSet("", flag.ContinueOnError)
	fs.VisitAll(func(f *flag.Flag) {
		if !strings.HasPrefix(f.Name, "test.") {
			visible.Var(f.Value, f.Name, f.Usage)
		}
	})
	visible.SetOutput(fs.Output())
	visible.PrintDefaults()
}

func loadConfig(envName string) (*Config, error) {
	var c Config
	if err := json.Unmarshal([]byte(os.Getenv(envName)), &c); err != nil {
		return nil, fmt.Errorf("decode %s: %w", envName, err)
	}
	return &c, nil
}

func encodeConfig(c *Config) string {
	data, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// credentialFiles is the set of files inside ~/.claude that hold auth tokens.
// They are excluded from claudeBinds so the container can't read them.
var credentialFiles = map[string]bool{
	".credentials.json": true,
	"credentials.json":  true,
}

// claudeLiveState is the set of top-level entries inside ~/.claude that hold
// mutable runtime state (history, sessions, stats, caches). On the merged-
// .claude path these are bind-mounted directly from ~/.claude rather than
// copied into the merged dir, so writes from inside the container persist
// and the RemoveAll on each invocation doesn't lose data.
var claudeLiveState = map[string]bool{
	"history.jsonl":    true,
	"projects":         true,
	"sessions":         true,
	"file-history":     true,
	"backups":          true,
	"plans":            true,
	"todos":            true,
	"downloads":        true,
	"stats-cache.json": true,
}

// claudeScratch is the set of top-level *directories* inside ~/.claude that
// hold purely ephemeral, regenerable scratch state (caches, logs, per-session
// snapshots) with no cross-instance value. Each is backed by a fresh, private
// per-container tmpfs (see claudeScratchBinds), so every runclaude invocation
// gets its own empty copy — nothing is shared with the host, nor with another
// sandbox running in the same cwd (which would otherwise share the per-cwd
// cache home).
//
// session-env in particular is the trigger for a directory-deletion race: the
// claude CLI's retention sweep does `rm -rf` on stale per-session subdirs and
// then `rmdir`s session-env itself when it ends up empty, while another
// instance may be mid-mkdir of its own subdir there. Two claude instances
// sharing one physical session-env — host+sandbox, or two same-cwd sandboxes —
// surfaces an ENOENT on mkdir (".../session-env/<uuid>"). A per-container
// tmpfs removes all sharing; nothing here feeds the global session list (that
// lives in projects/), so isolating it costs nothing.
var claudeScratch = map[string]bool{
	"session-env":     true,
	"paste-cache":     true,
	"shell-snapshots": true,
	"cache":           true,
	"debug":           true,
}

// claudeScratchBinds returns a tmpfs Bind for each scratch directory, mounted
// at $HOME/.claude/<name>. These overlay whatever is bound at $HOME/.claude
// (the cache home on the normal path, or the merged dir), giving each session
// a private, empty scratch dir. dedup-by-dest in childMain keeps the last
// bind, so appending these after the .claude bind makes the tmpfs win.
func claudeScratchBinds(home string) []Bind {
	claudeDir := filepath.Join(home, ".claude")
	out := make([]Bind, 0, len(claudeScratch))
	for name := range claudeScratch {
		out = append(out, Bind{Dest: filepath.Join(claudeDir, name), Tmpfs: true})
	}
	return out
}

func claudeBinds(home string) ([]Bind, error) {
	var binds []string
	claudeDir := filepath.Join(home, ".claude")
	if entries, err := os.ReadDir(claudeDir); err == nil {
		for _, e := range entries {
			if credentialFiles[e.Name()] {
				continue
			}
			// Scratch dirs (e.g. session-env) are never shared with the host;
			// the container creates its own under the cache home on demand.
			if claudeScratch[e.Name()] {
				continue
			}
			binds = append(binds, filepath.Join(claudeDir, e.Name()))
		}
	}
	binds = append(binds,
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".config", "claude"),
	)
	claude, err := exec.LookPath("claude")
	if err == nil {
		binds = append(binds, filepath.Dir(claude))
		if target, err := filepath.EvalSymlinks(claude); err == nil {
			binds = append(binds, filepath.Dir(target))
		}
	}
	var out []Bind
	for _, b := range binds {
		if _, err := os.Stat(b); err == nil {
			out = append(out, Bind{Path: b})
		}
	}
	return out, nil
}

// workspaceBinds returns paths to the underlying repo storage when cwd is a
// linked git worktree or a jj workspace whose repo lives outside cwd. The
// worktree's own .git/.jj is already covered by binding cwd itself.
func workspaceBinds(cwd string) []Bind {
	var out []Bind
	if data, err := os.ReadFile(filepath.Join(cwd, ".git")); err == nil {
		s := strings.TrimSpace(string(data))
		if rest, ok := strings.CutPrefix(s, "gitdir:"); ok {
			p := strings.TrimSpace(rest)
			if !filepath.IsAbs(p) {
				p = filepath.Join(cwd, p)
			}
			p = filepath.Clean(p)
			out = append(out, Bind{Path: p})
			if cd, err := os.ReadFile(filepath.Join(p, "commondir")); err == nil {
				c := strings.TrimSpace(string(cd))
				if !filepath.IsAbs(c) {
					c = filepath.Join(p, c)
				}
				out = append(out, Bind{Path: filepath.Clean(c)})
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(cwd, ".jj", "repo")); err == nil {
		p := strings.TrimSpace(string(data))
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, ".jj", p)
		}
		p = filepath.Clean(p)
		out = append(out, Bind{Path: p})
		primary := filepath.Dir(filepath.Dir(p))
		if fi, err := os.Stat(filepath.Join(primary, ".git")); err == nil && fi.IsDir() {
			out = append(out, Bind{Path: filepath.Join(primary, ".git")})
		}
	}
	return out
}

// jjConfigBinds returns the user-level jj configuration paths so commands
// inside the container resolve the same config (user.name/email, aliases,
// signing keys, etc.) the host user already has.
func jjConfigBinds() []Bind {
	var paths []string

	if v := os.Getenv("JJ_CONFIG"); v != "" {
		paths = append(paths, v)
	}
	cfgHome := os.Getenv("XDG_CONFIG_HOME")
	userHome := os.Getenv("HOME")
	if h, err := os.UserHomeDir(); err == nil {
		userHome = h
	}
	if userHome != "" {
		paths = append(paths, filepath.Join(userHome, ".jjconfig.toml"))
	}
	if cfgHome == "" && userHome != "" {
		cfgHome = filepath.Join(userHome, ".config")
	}
	if cfgHome != "" {
		paths = append(paths, filepath.Join(cfgHome, "jj"))
	}
	var out []Bind
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, Bind{Path: p})
		}
	}
	return out
}

// pathBinds returns directories from $PATH that live under a top-level dir the
// rootfs setup does not bind-mount from / (i.e. under $HOME, /tmp, /run, /dev).
// Everything else is already reachable through the recursive root rbinds.
func pathBinds(home string) []Bind {
	homePrefix := "/" + strings.SplitN(strings.TrimPrefix(home, "/"), "/", 2)[0]
	needsBind := map[string]bool{homePrefix: true, "/tmp": true, "/run": true, "/dev": true}
	var out []Bind
	seen := map[string]bool{}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		abs := filepath.Clean(p)
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		top := "/" + strings.SplitN(strings.TrimPrefix(abs, "/"), "/", 2)[0]
		if !needsBind[top] {
			continue
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			continue
		}
		out = append(out, Bind{Path: abs, ReadOnly: true})
	}
	return out
}

// writeStubCredentials writes a fake .credentials.json into the container's
// $HOME/.claude/ so claude believes it is logged in. The real credential lives
// only in the host proxy's memory and is injected by the MITM layer.
func writeStubCredentials(claudeDir string) error {
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		return err
	}
	stub := `{
  "claudeAiOauth": {
    "accessToken": "sk-ant-oat01-runclaude-stub",
    "refreshToken": "sk-ant-ort01-runclaude-stub",
    "expiresAt": 99999999999000,
    "scopes": ["user:inference", "user:profile"],
    "subscriptionType": "pro"
  }
}
`
	return os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(stub), 0600)
}

// readClaudeAuth returns either an OAuth bearer token or an API key extracted
// from ~/.claude/.credentials.json (preferred) or $ANTHROPIC_API_KEY.
// If a bearer is returned from a credentials file, refreshToken and credPath
// are populated so the caller can rotate the token in place on a 401.
func readClaudeAuth(home string) (apiKey, bearer, refreshToken, credPath string, err error) {
	c, err := creds.LoadAnthropicCredentials(home)
	if err != nil {
		return "", "", "", "", err
	}
	return c.APIKey, c.Bearer, c.RefreshToken, c.Path, nil
}

// checkUserNS verifies that unprivileged user namespaces are permitted by the
// kernel and prints actionable instructions if they are blocked.
func checkUserNS() error {
	type sysctl struct {
		path    string
		blocked string // value that means "blocked"
		fix     string
	}
	checks := []sysctl{
		{
			path:    "/proc/sys/kernel/unprivileged_userns_clone",
			blocked: "0",
			fix:     "sudo sysctl -w kernel.unprivileged_userns_clone=1",
		},
		{
			path:    "/proc/sys/kernel/apparmor_restrict_unprivileged_userns",
			blocked: "1",
			fix:     "sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0",
		},
	}
	for _, c := range checks {
		data, err := os.ReadFile(c.path)
		if err != nil {
			continue // sysctl doesn't exist on this kernel — not a problem
		}
		if strings.TrimSpace(string(data)) == c.blocked {
			return fmt.Errorf(
				"unprivileged user namespaces are disabled (%s = %s)\n"+
					"runclaude requires them; enable with:\n\n"+
					"  %s\n\n"+
					"To persist across reboots, add the setting to /etc/sysctl.d/.",
				c.path, c.blocked, c.fix,
			)
		}
	}
	return nil
}

// cacheDirFor returns the per-cwd runclaude cache directory.
func cacheDirFor(cwd string) (string, error) {
	sum := sha256.Sum256([]byte(cwd))
	cacheBase, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheBase, "runclaude", filepath.Base(cwd)+"-"+hex.EncodeToString(sum[:])[:16]), nil
}

func mainErr() error {
	handled, verb, resumeSid, cliArgs, err := dispatchSubcommand(os.Args[1:])
	if handled {
		return err
	}

	dir, err := os.MkdirTemp("", "runclaude-")
	if err != nil {
		return err
	}
	// Per-session scratch (rootfs skeleton, merged .claude tree); the container
	// runs synchronously below, so it's safe to remove on return.
	defer os.RemoveAll(dir)
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.Mkdir(rootfs, 0755); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(home) || strings.Trim(home, "/") == "" {
		return fmt.Errorf("$HOME must be absolute and not %q, got %q", "/", home)
	}

	// Load project-local option defaults before registering flags; CLI flags override.
	fileOpts, err := loadOptionsFile(cwd)
	if err != nil {
		log.Printf("warning: .claude/runclaude.json: %v", err)
	}

	// The sandbox flag set, shared by the bare invocation and the new/resume
	// verbs (which are the same sandbox path with recording forced on); the
	// host-only verbs have their own flag sets in recordcmd.go.
	fs := flag.NewFlagSet("runclaude", flag.ExitOnError)
	var exposed stringSlice
	exposed = append(exposed, fileOpts.Expose...)
	fs.Var(&exposed, "expose", "expose host path into the container (repeatable)")
	fs.Var(&exposed, "e", "alias for --expose")
	var only stringSlice
	only = append(only, fileOpts.Only...)
	fs.Var(&only, "only", "if set, only these paths (relative to cwd or absolute) are visible from cwd inside the container; cwd is backed by the cache dir, no git/jj repos are mapped (repeatable)")
	fs.Var(&only, "o", "alias for --only")
	mapPath := fs.Bool("map-path", boolOr(fileOpts.MapPath, true), "map all directories from $PATH")
	claudeMode := fs.Bool("claude", boolOr(fileOpts.Claude, true), "bind files needed for `claude` and run it as the default command")
	claudeConfig := fs.Bool("claude-config", fileOpts.ClaudeConfig, "like --claude but do not set the command (for testing/custom commands)")
	restrictNet := fs.Bool("restrict-net", boolOr(fileOpts.RestrictNet, true), "run in a new network namespace; egress only via the in-process HTTP proxy and DNS server")
	var allowDomain domainList
	if len(fileOpts.AllowDomains) > 0 {
		allowDomain.items = append(allowDomain.items, fileOpts.AllowDomains...)
		allowDomain.set = true
	}
	mapWorkspace := fs.Bool("map-workspace", boolOr(fileOpts.MapWorkspace, true), "for git/jj workspaces map the originating repository")
	fs.Var(&allowDomain, "allow-domain",
		"allowed egress domain (repeatable); defaults to a built-in list; pass --allow-domain= to disable enforcement")
	fs.Var(&allowDomain, "a", "alias for --allow-domain")
	proxyLog := fs.String("test.proxy-log", fileOpts.ProxyLog,
		"path to write the proxy log to (default: <cache-dir>/proxy.log)")
	approveListen := fs.String("approve-listen", strOr(fileOpts.ApproveListen, "127.0.0.1:0"),
		"host address for the domain-approval web UI; the chosen address is printed on startup")
	exclusive := flag.String("exclusive", "no",
		"same-cwd concurrency control: \"no\" (default, just warn about other sessions), "+
			"\"yes\" (refuse to start if another runclaude session shares this cwd), or "+
			"\"probe\" (do not start; exit 0 if this cwd is free, nonzero if another session is live)")
	var mitmUpstream stringSlice
	mitmUpstream = append(mitmUpstream, fileOpts.MitmUpstream...)
	fs.Var(&mitmUpstream, "test.mitm-upstream",
		"override MITM upstream for a host, format host=url (repeatable). For testing: point the proxy at a fake server.")
	var claudeFlags stringSlice
	claudeFlags = append(claudeFlags, fileOpts.ClaudeFlags...)
	fs.Var(&claudeFlags, "claude-flag",
		"extra argument to pass to the in-container `claude` command (repeatable); e.g. --claude-flag=--setting-sources --claude-flag=user")
	anthropicKeyOverride := fs.String("test.anthropic-key", "",
		"override the Anthropic API key the proxy injects (bypasses ~/.claude/.credentials.json); for testing")
	anthropicBearerOverride := fs.String("test.anthropic-bearer", "",
		"override the Anthropic OAuth bearer the proxy injects (bypasses ~/.claude/.credentials.json); for testing")
	anthropicRefreshOverride := fs.String("test.anthropic-refresh", "",
		"OAuth refresh token to use with --test.anthropic-bearer for 401-triggered refresh; for testing")
	recordUpstream := fs.String("record-upstream", fileOpts.RecordUpstream,
		"upstream ref bounding session bundles (default: origin's default branch)")
	var recordExclude stringSlice
	recordExclude = append(recordExclude, fileOpts.RecordExclude...)
	fs.Var(&recordExclude, "record-exclude", "extra ignore pattern for recorded snapshots (repeatable)")
	fs.Usage = func() {
		o := fs.Output()
		switch verb {
		case "new":
			fmt.Fprint(o, "usage: runclaude new [flags] [--] [claude args...]\n\nflags:\n")
		case "resume":
			fmt.Fprint(o, "usage: runclaude resume [<sid>] [flags] [--] [claude args...]\n\nflags:\n")
		default:
			fmt.Fprint(o, `usage: runclaude [flags] [--] [command...]
       runclaude new [flags] [--] [claude args...]             start a new recorded claude session
       runclaude resume [<sid>] [flags] [--] [claude args...]  resume a session (default: latest recorded)
       runclaude list                                          list recorded sessions
       runclaude upload [--session <sid>] [--upstream <ref>] <dir|bundle|remote>
       runclaude download [--session <sid>] [--at <sel>] [--dest <dir>] <bundle|remote>
       runclaude rm <session-id>...

flags:
`)
		}
		printVisibleDefaults(fs)
	}
	fs.Parse(cliArgs)
	// Recording is turned on by the new/resume verbs or the project options
	// file; stickiness re-enables it per session regardless (see record.go).
	record := verb != "" || boolOr(fileOpts.Record, false)
	if resumeSid != "" && !*claudeMode {
		return fmt.Errorf("resume requires claude mode (--claude)")
	}

	switch *exclusive {
	case "no", "yes", "probe":
	default:
		return fmt.Errorf("--exclusive must be one of no, yes, probe; got %q", *exclusive)
	}
	// The same-cwd session registry lives under the per-cwd cache dir; compute
	// its path up front so --exclusive can consult it before any expensive
	// startup work (credential loads, AWS SSO).
	cacheDir, err := cacheDirFor(cwd)
	if err != nil {
		return err
	}
	if *exclusive != "no" {
		others := liveSessions(filepath.Join(cacheDir, "sessions"))
		switch *exclusive {
		case "probe":
			if len(others) > 0 {
				return exitError{1, fmt.Sprintf("%d other runclaude session(s) share this cwd", len(others))}
			}
			// cwd is free: report success and do not start.
			fmt.Fprintln(os.Stderr, "runclaude: cwd is free")
			return nil
		case "yes":
			if len(others) > 0 {
				var lines []string
				for _, o := range others {
					lines = append(lines, "  - "+o.describe())
				}
				return exitError{1, fmt.Sprintf("--exclusive=yes: %d other runclaude session(s) already share this cwd:\n%s",
					len(others), strings.Join(lines, "\n"))}
			}
		}
	}

	claudeLike := *claudeMode || *claudeConfig

	allowedDomains := allowDomain.items
	if !allowDomain.set {
		allowedDomains = proxy.DefaultAllowedDomains
	}

	var (
		mitmDomains  []string
		apiKey       string
		bearer       string
		refreshToken string
		credPath     string
	)
	// Pull env from .claude/settings.json into the host process. The
	// in-container claude reads settings.json itself; this lets runclaude
	// see the same effective env (CLAUDE_CODE_USE_BEDROCK, AWS_PROFILE,
	// etc.) without the user having to also export them in their shell.
	// Host env still wins — settings.json only fills unset variables.
	var settings claudeSettings
	if claudeLike {
		settings = loadClaudeSettings(home, cwd)
		applyClaudeSettingsEnv(settings.Env)
	}
	// Explicit key/bearer overrides force the Anthropic path even on a
	// Bedrock host (e.g. test-mitm.sh passing --test.anthropic-bearer).
	hasExplicitAnthropicCreds := *anthropicKeyOverride != "" || *anthropicBearerOverride != ""
	useBedrock := claudeLike && !hasExplicitAnthropicCreds && os.Getenv("CLAUDE_CODE_USE_BEDROCK") == "1"
	if hasExplicitAnthropicCreds {
		apiKey = *anthropicKeyOverride
		bearer = *anthropicBearerOverride
		refreshToken = *anthropicRefreshOverride
	} else if claudeLike && !useBedrock {
		var err error
		apiKey, bearer, refreshToken, credPath, err = readClaudeAuth(home)
		if err != nil {
			log.Printf("warning: --claude: %v", err)
		}
	}
	if apiKey != "" || bearer != "" {
		mitmDomains = append(mitmDomains, "api.anthropic.com")
		if !proxy.MatchDomain("api.anthropic.com", allowedDomains) {
			allowedDomains = append(allowedDomains, "api.anthropic.com")
		}
	}
	// Hosts mentioned via --test.mitm-upstream should also be MITM'd and on
	// the allowlist; the override is meaningless otherwise.
	for host := range parseMitmUpstream(mitmUpstream) {
		if !proxy.MatchDomain(host, mitmDomains) {
			mitmDomains = append(mitmDomains, host)
		}
		if !proxy.MatchDomain(host, allowedDomains) {
			allowedDomains = append(allowedDomains, host)
		}
	}
	var awsSigner *v4.Signer
	var awsCreds aws.CredentialsProvider
	if useBedrock {
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
		if err != nil {
			return fmt.Errorf("--claude+CLAUDE_CODE_USE_BEDROCK: load AWS config: %w", err)
		}
		provider := creds.NewRefreshingCredentialProvider(awsCfg, settings.AwsAuthRefresh, log.Default())
		// Validate credentials (running awsAuthRefresh if needed) before start.
		if _, err := provider.Retrieve(context.Background()); err != nil {
			return fmt.Errorf("--claude+CLAUDE_CODE_USE_BEDROCK: %w", err)
		}
		awsCreds = provider
		awsSigner = v4.NewSigner()
		pattern := "bedrock-runtime.*.amazonaws.com"
		mitmDomains = append(mitmDomains, pattern)
		allowedDomains = append(allowedDomains, pattern)
	}

	cacheBase, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	for _, sub := range []string{"home", "tmp", "run"} {
		if err := os.MkdirAll(filepath.Join(cacheDir, sub), 0700); err != nil {
			return err
		}
	}
	if err := materializeGitConfig(home, filepath.Join(cacheDir, "home")); err != nil {
		return err
	}

	// When --only is active, the container's cwd is backed by a fresh cache
	// subdir (not the real cwd tree). Only the explicitly listed items are
	// bind-mounted on top, and no git/jj repo storage is mapped.
	onlyMode := len(only) > 0
	cwdBind := cwd
	if onlyMode {
		cwdCacheDir := filepath.Join(cacheDir, "cwd")
		if err := os.MkdirAll(cwdCacheDir, 0700); err != nil {
			return err
		}
		cwdBind = cwdCacheDir
	}

	cfg := &Config{
		Rootfs:         rootfs,
		Home:           home,
		CacheDir:       cacheDir,
		Cwd:            cwd,
		Binds:          []Bind{{Path: cwdBind, Dest: cwd}},
		RestrictNet:    *restrictNet,
		AllowedDomains: allowedDomains,
		MitmDomains:    mitmDomains,
		MitmUpstream:   parseMitmUpstream(mitmUpstream),
	}

	// commHostEnd is our side of the socketpair used for fd passing back from
	// init. commChildFile is passed via ExtraFiles all the way to init.
	var commHostEnd *os.File
	var commChildFile *os.File
	if enforceAllowlist(cfg) {
		logPath := *proxyLog
		if logPath == "" {
			logPath = filepath.Join(cacheDir, "proxy.log")
		}
		logger, err := proxy.OpenLogger(logPath)
		if err != nil {
			return err
		}
		log.Printf("proxy log: %s", logPath)

		// A single Allowlist backs both the proxy allow decision and the DNS
		// server. mitmDomains are already contained in allowedDomains (each is
		// appended above), but Add dedups so this stays correct regardless.
		allowlist := proxy.NewAllowlist(allowedDomains)
		for _, d := range mitmDomains {
			allowlist.Add(d)
		}
		approver := proxy.NewApprover(allowlist, logger)
		if approvalLn, err := net.Listen("tcp", *approveListen); err != nil {
			log.Printf("warning: domain approval UI: %v", err)
		} else {
			log.Printf("domain approval UI: http://%s", approvalLn.Addr())
			go func() {
				if err := http.Serve(approvalLn, approver); err != nil {
					logger.Printf("approval UI: %v", err)
				}
			}()
		}

		setup := &proxy.Setup{
			Allow:    allowlist,
			Mitm:     mitmDomains,
			Inject:   func(string, *http.Request) {},
			Upstream: cfg.MitmUpstream,
			OnDeny:   approver.Record,
		}
		if len(mitmDomains) > 0 {
			ca, err := proxy.LoadOrCreateCA(filepath.Join(cacheBase, "runclaude"))
			if err != nil {
				return fmt.Errorf("load CA: %w", err)
			}
			caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Leaf.Raw})
			caCopy := filepath.Join(cacheDir, "tmp", "runclaude-ca.crt")
			if err := os.WriteFile(caCopy, caPEM, 0644); err != nil {
				return err
			}
			bundleCopy := filepath.Join(cacheDir, "tmp", "runclaude-bundle.crt")
			if err := proxy.WriteBundle(caPEM, bundleCopy); err != nil {
				return err
			}
			cfg.BundlePath = "/tmp/runclaude-bundle.crt"
			cfg.CAPath = "/tmp/runclaude-ca.crt"
			setup.Leaves = proxy.NewLeafCache(ca)
			// If we have a refresh token, install a token source so we
			// can rotate the access token on a 401. Otherwise the bearer
			// is constant for the lifetime of the process.
			var tokens *creds.TokenSource
			if bearer != "" && refreshToken != "" {
				tokens = creds.NewTokenSource(bearer, refreshToken, credPath, logger)
			}
			setup.Inject = func(host string, r *http.Request) {
				switch {
				case host == "api.anthropic.com" && (bearer != "" || apiKey != ""):
					r.Header.Del("Authorization")
					r.Header.Del("x-api-key")
					switch {
					case tokens != nil:
						r.Header.Set("Authorization", "Bearer "+tokens.Bearer())
					case bearer != "":
						r.Header.Set("Authorization", "Bearer "+bearer)
					default:
						r.Header.Set("x-api-key", apiKey)
					}
					if r.Header.Get("anthropic-version") == "" {
						r.Header.Set("anthropic-version", "2023-06-01")
					}
				case proxy.IsBedrockHost(host) && awsCreds != nil:
					proxy.InjectBedrock(host, r, awsCreds, awsSigner, logger)
				}
			}
			if tokens != nil {
				setup.Transports = map[string]http.RoundTripper{
					"api.anthropic.com": &creds.RefreshTransport{
						Base:     http.DefaultTransport,
						Tokens:   tokens,
						Reinject: func(r *http.Request) { setup.Inject("api.anthropic.com", r) },
						Logger:   logger,
					},
				}
			}
		}

		sp, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err != nil {
			return fmt.Errorf("socketpair: %w", err)
		}
		commHostEnd = os.NewFile(uintptr(sp[0]), "comm-host")
		commChildFile = os.NewFile(uintptr(sp[1]), "comm-child")

		go func() {
			defer commHostEnd.Close()
			fds, err := recvFds(commHostEnd, 3)
			if err != nil {
				logger.Printf("recvFds: %v", err)
				return
			}
			proxyFile := os.NewFile(uintptr(fds[0]), "proxy-listener")
			dnsUDPFile := os.NewFile(uintptr(fds[1]), "dns-udp")
			dnsTCPFile := os.NewFile(uintptr(fds[2]), "dns-tcp")
			proxyLn, err := net.FileListener(proxyFile)
			proxyFile.Close()
			if err != nil {
				logger.Printf("FileListener proxy: %v", err)
				return
			}
			dnsUDP, err := net.FilePacketConn(dnsUDPFile)
			dnsUDPFile.Close()
			if err != nil {
				logger.Printf("FilePacketConn dns: %v", err)
				return
			}
			dnsTCP, err := net.FileListener(dnsTCPFile)
			dnsTCPFile.Close()
			if err != nil {
				logger.Printf("FileListener dns-tcp: %v", err)
				return
			}
			// DNS shares the proxy's Allowlist so runtime approvals also make
			// the host resolvable inside the container.
			go proxy.ServeDNSUDP(dnsUDP, allowlist, logger)
			go proxy.ServeDNSTCP(dnsTCP, allowlist, logger)
			proxy.Serve(setup, proxyLn, logger)
		}()
	}
	for _, e := range exposed {
		abs, err := filepath.Abs(e)
		if err != nil {
			return err
		}
		cfg.Binds = append(cfg.Binds, Bind{Path: abs})
	}
	if onlyMode {
		for _, e := range only {
			abs := e
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(cwd, e)
			}
			abs = filepath.Clean(abs)
			cfg.Binds = append(cfg.Binds, Bind{Path: abs})
		}
	}
	if *mapPath {
		cfg.Binds = append(cfg.Binds, pathBinds(home)...)
	}
	if !onlyMode && *mapWorkspace {
		cfg.Binds = append(cfg.Binds, workspaceBinds(cwd)...)
	}

	if !onlyMode {
		if _, err := os.Stat(".jj"); err == nil {
			cfg.Binds = append(cfg.Binds, jjConfigBinds()...)
		}
	}
	if claudeLike {
		if err := ensureProjectTrusted(home, cwd); err != nil {
			log.Printf("warning: pre-trust %s in ~/.claude.json: %v", cwd, err)
		}
		extra, err := claudeBinds(home)
		if err != nil {
			return err
		}
		// When the project's .claude/settings.json carries AWS-related fields,
		// rewriting it in place fights with VCS (jj/git keep restoring the
		// original). Instead, build a merged user+project .claude/ tree under
		// the cache, mount it as the in-container ~/.claude, and tell claude
		// to ignore project settings via --setting-sources user.
		mergeProjectClaude := projectSettingsHasAWS(cwd)
		credDir := filepath.Join(cacheDir, "home", ".claude")
		if mergeProjectClaude {
			// Per-session, NOT per-cwd: buildMergedClaudeDir starts with
			// RemoveAll(mergedDir), so a per-cwd path would let a second
			// runclaude in the same cwd wipe the tree the first one has
			// bind-mounted live as $HOME/.claude — deleting it out from under
			// the running container (ENOENT on session-env mkdir, cross-session
			// leakage). dir is the per-invocation MkdirTemp dir, so each
			// session gets its own merged tree. Cleaned up when the container
			// exits (see defer below).
			mergedDir := filepath.Join(dir, "claude-merged")
			if err := buildMergedClaudeDir(home, cwd, mergedDir); err != nil {
				return fmt.Errorf("merge .claude/: %w", err)
			}
			userClaudePrefix := filepath.Join(home, ".claude") + string(filepath.Separator)
			filtered := extra[:0]
			for _, b := range extra {
				if strings.HasPrefix(b.Path, userClaudePrefix) {
					continue
				}
				filtered = append(filtered, b)
			}
			extra = append(filtered, Bind{Path: mergedDir, Dest: filepath.Join(home, ".claude")})
			credDir = mergedDir
			// Bind live-state entries directly from ~/.claude so writes
			// persist; they are not copied into mergedDir.
			for name := range claudeLiveState {
				src := filepath.Join(home, ".claude", name)
				if _, err := os.Stat(src); err == nil {
					extra = append(extra, Bind{
						Path: src,
						Dest: filepath.Join(home, ".claude", name),
					})
				}
			}
		}
		cfg.Binds = append(cfg.Binds, extra...)
		// Overlay each ephemeral scratch dir with a private per-container
		// tmpfs. Appended after the .claude bind so the parent mounts first
		// and these children land on top; not shared with the host or with
		// another sandbox in the same cwd.
		cfg.Binds = append(cfg.Binds, claudeScratchBinds(home)...)
		if *claudeMode {
			cmd := []string{"claude", "--dangerously-skip-permissions"}
			if mergeProjectClaude {
				cmd = append(cmd, "--setting-sources", "user")
			}
			cmd = append(cmd, []string(claudeFlags)...)
			claudeArgs := fs.Args()
			if resumeSid != "" {
				claudeArgs = append([]string{"--resume", resumeSid}, claudeArgs...)
			}
			cmd = append(cmd, claudeArgs...)
			// Resuming a session recorded by a different clone (fetched via
			// --download) must fork: continuing the author's session id
			// would collide with their refs. Same-clone sessions continue.
			if !onlyMode {
				cmd = append(cmd, autoForkSession(cwd, claudeArgs)...)
			}
			cfg.Command = cmd
		}
		if bearer != "" || apiKey != "" {
			if err := writeStubCredentials(credDir); err != nil {
				return err
			}
		}
		if !mergeProjectClaude {
			if !onlyMode {
				excludeBinds, err := vcsExcludeBinds(cwd, filepath.Join(cacheDir, "vcs"), ".claude/settings.json")
				if err != nil {
					return err
				}
				cfg.Binds = append(cfg.Binds, excludeBinds...)
			}
			// Overlay a sanitized settings.json into the container so claude
			// doesn't see AWS_PROFILE etc. and try to authenticate itself —
			// the host proxy handles signing.
			settingsBinds, err := materializeClaudeSettings(home, cwd, filepath.Join(cacheDir, "claude"))
			if err != nil {
				return err
			}
			cfg.Binds = append(cfg.Binds, settingsBinds...)
		}
	}
	if len(cfg.Command) == 0 {
		if args := fs.Args(); len(args) > 0 {
			cfg.Command = args
		} else {
			cfg.Command = []string{"bash"}
		}
	}

	// Write the effective options as /runclaude.json inside the container so
	// any process can inspect the session configuration.
	activeOpts := Options{
		Expose:         []string(exposed),
		Only:           []string(only),
		MapPath:        boolPtr(*mapPath),
		Claude:         boolPtr(*claudeMode),
		ClaudeConfig:   *claudeConfig,
		RestrictNet:    boolPtr(*restrictNet),
		AllowDomains:   allowedDomains,
		MapWorkspace:   boolPtr(*mapWorkspace),
		ProxyLog:       *proxyLog,
		ApproveListen:  *approveListen,
		MitmUpstream:   []string(mitmUpstream),
		ClaudeFlags:    []string(claudeFlags),
		Record:         boolPtr(record),
		RecordUpstream: *recordUpstream,
		RecordExclude:  []string(recordExclude),
	}
	activeJSON, _ := json.MarshalIndent(activeOpts, "", "\t")
	runclaudeJSONPath := filepath.Join(dir, "runclaude.json")
	if err := os.WriteFile(runclaudeJSONPath, activeJSON, 0644); err != nil {
		return err
	}
	cfg.Binds = append(cfg.Binds, Bind{Path: runclaudeJSONPath, Dest: "/runclaude.json", ReadOnly: true})

	// Record this session and warn if other runclaude sessions share this
	// cwd's cache (they share $HOME cache, /tmp and the merged ~/.claude tree).
	cleanupSession := registerSession(cacheDir, cfg.Command)
	defer cleanupSession()
	if err := checkUserNS(); err != nil {
		return err
	}

	// Session recorder: runs whenever a git dir resolves so sticky sessions
	// (previously recorded, resumed on a plain run) keep recording; it
	// claims new sessions only when recording was asked for.
	var rec *recorder
	if claudeLike && !onlyMode {
		if gitDir, ok := resolveRecordGitDir(cwd); ok {
			// Recorder output goes to a file: claude's TUI owns the
			// terminal once the container starts. Anything user-facing is
			// printed here (before) or after cmd.Run (below).
			recordLogPath := filepath.Join(cacheDir, "record.log")
			recordLogger, err := proxy.OpenLogger(recordLogPath)
			if err != nil {
				return err
			}
			r, err := newRecorder(gitDir, cwd, home, record, *recordUpstream, recordExclude, recordLogger)
			if err != nil {
				log.Printf("warning: record: %v", err)
			} else {
				rec = r
				// Sessions are repo-scoped: bind the shared dir (the main
				// worktree's project dir) over this cwd's project dir inside
				// the container, so any worktree of the clone can list and
				// resume the clone's sessions. Appended last: it must mount
				// on top of the ~/.claude binds.
				if cwdDir := transcriptDir(home, cwd); rec.projectsDir != cwdDir {
					if err := os.MkdirAll(cwdDir, 0700); err == nil {
						cfg.Binds = append(cfg.Binds, Bind{Path: rec.projectsDir, Dest: cwdDir})
					}
				}
				go rec.run()
				if record {
					log.Printf("record: on — snapshots under refs/runclaude/, log: %s", recordLogPath)
				}
			}
		} else if record {
			log.Printf("warning: record: no git or jj repository at %s", cwd)
		}
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	// Single-uid mapping: the container only ever needs the user's own uid
	// (see README). That lets us create the user+mount ns directly via
	// Cloneflags instead of shelling out to unshare(1) + newuidmap.
	// AmbientCaps preserves the caps we gain from creating the user ns
	// across execve, equivalent to `unshare --keep-caps`.
	uid := os.Getuid()
	gid := os.Getgid()
	ambient := make([]uintptr, 0, 41)
	for c := uintptr(0); c < 41; c++ {
		ambient = append(ambient, c)
	}
	cmd := exec.Command(self)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		AmbientCaps: ambient,
	}
	containerEnv := os.Environ()
	if claudeLike {
		// Strip all AWS/Bedrock vars that applyClaudeSettingsEnv may have
		// injected from settings.json; the container gets only what we
		// explicitly re-add below.
		containerEnv = scrubAWSCreds(containerEnv)
	}
	if useBedrock {
		// Re-add Bedrock routing vars and stub credentials so the in-container
		// aws-sdk constructs requests; the MITM strips these and re-signs.
		containerEnv = append(containerEnv,
			"CLAUDE_CODE_USE_BEDROCK=1",
			"AWS_ACCESS_KEY_ID=AKIA000RUNCLAUDESTUB",
			"AWS_SECRET_ACCESS_KEY=runclaude-stub-secret-do-not-use",
		)
		if r := os.Getenv("AWS_REGION"); r != "" {
			containerEnv = append(containerEnv, "AWS_REGION="+r)
		}
		if r := os.Getenv("AWS_DEFAULT_REGION"); r != "" {
			containerEnv = append(containerEnv, "AWS_DEFAULT_REGION="+r)
		}
	}
	cmd.Env = append(containerEnv, childEnv+"="+encodeConfig(cfg))
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	if commChildFile != nil {
		cmd.ExtraFiles = []*os.File{commChildFile}
	}
	err = cmd.Run()
	if commChildFile != nil {
		commChildFile.Close()
	}
	if rec != nil {
		// The terminal is ours again: safe to tell the user what was
		// recorded and how to share it.
		for _, sid := range rec.Close() {
			log.Printf("record: session %s recorded; share with: runclaude upload --session %s <dir|remote>", sid, sid)
		}
	}
	return err
}

// scrubAWSCreds returns env without AWS credential and Bedrock-routing
// variables. Callers re-add the ones needed for the specific mode.
func scrubAWSCreds(env []string) []string {
	skip := map[string]bool{
		"AWS_ACCESS_KEY_ID":           true,
		"AWS_SECRET_ACCESS_KEY":       true,
		"AWS_SESSION_TOKEN":           true,
		"AWS_SECURITY_TOKEN":          true,
		"AWS_PROFILE":                 true,
		"AWS_CONFIG_FILE":             true,
		"AWS_SHARED_CREDENTIALS_FILE": true,
		// Bedrock routing: strip so non-Bedrock mode isn't accidentally routed
		// to Bedrock; re-added explicitly when useBedrock is true.
		"CLAUDE_CODE_USE_BEDROCK": true,
		"AWS_REGION":              true,
		"AWS_DEFAULT_REGION":      true,
	}
	out := env[:0]
	for _, e := range env {
		name := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			name = e[:i]
		}
		if skip[name] {
			continue
		}
		out = append(out, e)
	}
	return out
}

func setupDev(rootfs string) error {
	dev := filepath.Join(rootfs, "dev")
	if err := os.MkdirAll(dev, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", dev, "tmpfs",
		syscall.MS_NOSUID|syscall.MS_STRICTATIME, "mode=755"); err != nil {
		return fmt.Errorf("tmpfs %s: %w", dev, err)
	}

	for _, name := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		src := "/dev/" + name
		dst := filepath.Join(dev, name)
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			return err
		}
		f.Close()
		if err := syscall.Mount(src, dst, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind %s: %w", src, err)
		}
	}

	pts := filepath.Join(dev, "pts")
	if err := os.Mkdir(pts, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("devpts", pts, "devpts",
		syscall.MS_NOSUID|syscall.MS_NOEXEC,
		"newinstance,ptmxmode=0666,mode=0620"); err != nil {
		return fmt.Errorf("devpts: %w", err)
	}

	shm := filepath.Join(dev, "shm")
	if err := os.Mkdir(shm, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", shm, "tmpfs",
		syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, "mode=1777"); err != nil {
		return fmt.Errorf("tmpfs /dev/shm: %w", err)
	}

	for _, l := range [][2]string{
		{"pts/ptmx", "ptmx"},
		{"/proc/self/fd", "fd"},
		{"/proc/self/fd/0", "stdin"},
		{"/proc/self/fd/1", "stdout"},
		{"/proc/self/fd/2", "stderr"},
	} {
		if err := os.Symlink(l[0], filepath.Join(dev, l[1])); err != nil {
			return err
		}
	}
	return nil
}

func childMain() error {
	cfg, err := loadConfig(childEnv)
	if err != nil {
		return err
	}

	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("make-rprivate /: %w", err)
	}
	if err := syscall.Mount(cfg.Rootfs, cfg.Rootfs, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind rootfs onto itself: %w", err)
	}

	homePrefix := "/" + strings.SplitN(strings.TrimPrefix(cfg.Home, "/"), "/", 2)[0]
	// /mnt and /media are excluded because admins commonly mount additional
	// filesystems there, and recursive bind-mounting them risks aliasing the
	// volume that backs the cache home/tmp/run (e.g. a btrfs whose subvol=/
	// is mounted at /mnt/data and whose subvol=/cache is mounted at $HOME).
	// The post-bind containment check below catches any remaining aliases.
	excluded := map[string]bool{
		homePrefix: true, "/proc": true, "/tmp": true, "/run": true, "/dev": true,
		"/mnt": true, "/media": true,
	}
	entries, err := os.ReadDir("/")
	if err != nil {
		return err
	}
	for _, e := range entries {
		src := "/" + e.Name()
		if excluded[src] {
			continue
		}
		dest := filepath.Join(cfg.Rootfs, src)
		info, err := os.Lstat(src)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(src)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, dest); err != nil {
				return err
			}
			continue
		}
		if !info.IsDir() {
			continue
		}
		if err := os.Mkdir(dest, 0755); err != nil {
			return err
		}
		if err := syscall.Mount(src, dest, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("rbind %s -> %s: %w", src, dest, err)
		}
		if err := syscall.Mount("", dest, "", syscall.MS_SLAVE|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("make-rslave %s: %w", dest, err)
		}
		if err := syscall.Mount("", dest, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			if err != syscall.EPERM {
				// Some pseudo-filesystems (sysfs, etc.) refuse a RO remount in a
				// nested user ns. Keep them writable rather than failing.
				log.Printf("warning: remount-ro %s: %v", dest, err)
			}
		}
	}

	if err := setupDev(cfg.Rootfs); err != nil {
		return err
	}

	type cacheMount struct {
		dest, sub string
		mode      os.FileMode
	}
	for _, m := range []cacheMount{
		{cfg.Home, "home", 0700},
		{"/tmp", "tmp", 01777},
		{"/run", "run", 0755},
	} {
		src := filepath.Join(cfg.CacheDir, m.sub)
		if err := os.Chmod(src, m.mode); err != nil {
			return err
		}
		dst := filepath.Join(cfg.Rootfs, m.dest)
		if err := os.MkdirAll(dst, 0755); err != nil {
			return err
		}
		if err := syscall.Mount(src, dst, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("bind %s -> %s: %w", src, dst, err)
		}
		if err := syscall.Mount("", dst, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
			log.Printf("warning: remount-nosuid %s: %v", dst, err)
		}
	}
	runInRoot := filepath.Join(cfg.Rootfs, "run")
	runUserInRoot := filepath.Join(runInRoot, "user", fmt.Sprintf("%d", os.Getuid()))
	if err := os.MkdirAll(runUserInRoot, 0700); err != nil {
		return err
	}
	if cfg.RestrictNet {
		// /etc/resolv.conf is a symlink into /run/systemd/resolve/...; write a
		// stub there pointing at the in-process DNS server that init brings up
		// on 127.0.0.1:53 inside the netns.
		dest := filepath.Join(runInRoot, "systemd", "resolve")
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
		stub := "nameserver 127.0.0.1\noptions edns0 trust-ad\n"
		if err := os.WriteFile(filepath.Join(dest, "stub-resolv.conf"), []byte(stub), 0644); err != nil {
			return err
		}
	} else if _, err := os.Stat("/run/systemd/resolve"); err == nil {
		dest := filepath.Join(runInRoot, "systemd", "resolve")
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
		if err := syscall.Mount("/run/systemd/resolve", dest, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("rbind /run/systemd/resolve: %w", err)
		}
		if err := syscall.Mount("", dest, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			log.Printf("warning: remount-ro /run/systemd/resolve: %v", err)
		}
	}

	// Deduplicate by dest, keeping the last bind (later entries override earlier
	// ones, so overlays appended after a directory bind take effect).
	dedupedBinds := make([]Bind, 0, len(cfg.Binds))
	seenDest := map[string]int{}
	for _, d := range cfg.Binds {
		if i, ok := seenDest[d.dest()]; ok {
			dedupedBinds[i] = d
		} else {
			seenDest[d.dest()] = len(dedupedBinds)
			dedupedBinds = append(dedupedBinds, d)
		}
	}
	for _, d := range dedupedBinds {
		dest := filepath.Join(cfg.Rootfs, d.dest())
		if d.Tmpfs {
			// Fresh per-container scratch dir; no host source, nothing shared.
			if err := os.MkdirAll(dest, 0700); err != nil {
				return err
			}
			if err := syscall.Mount("tmpfs", dest, "tmpfs",
				syscall.MS_NOSUID|syscall.MS_NODEV, "mode=700"); err != nil {
				return fmt.Errorf("tmpfs %s: %w", dest, err)
			}
			continue
		}
		info, err := os.Stat(d.Path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := os.MkdirAll(dest, 0755); err != nil {
				return err
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				return err
			}
			f.Close()
		}
		if err := syscall.Mount(d.Path, dest, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("rbind %s -> %s: %w", d.Path, dest, err)
		}
		if d.ReadOnly {
			var st syscall.Statfs_t
			if err := syscall.Statfs(d.Path, &st); err != nil {
				return fmt.Errorf("statfs %s: %w", d.Path, err)
			}
			// In a user namespace, locked per-mount flags (nosuid, nodev,
			// noexec, atime variants) on the source must be preserved on
			// remount; clearing any of them returns EPERM. ST_* bit values
			// match MS_* for the bits we care about.
			const preserve = syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC |
				syscall.MS_NOATIME | syscall.MS_NODIRATIME | syscall.MS_RELATIME |
				syscall.MS_SYNCHRONOUS
			flags := uintptr(st.Flags)&preserve | syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY
			if err := syscall.Mount("", dest, "", flags, ""); err != nil {
				return fmt.Errorf("remount-ro %s: %w", dest, err)
			}
		}
	}

	if err := checkContainment(cfg.Rootfs, []string{
		filepath.Join(cfg.Rootfs, cfg.Home),
		filepath.Join(cfg.Rootfs, "tmp"),
		filepath.Join(cfg.Rootfs, "run"),
	}); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), initEnv+"="+encodeConfig(cfg))
	flags := uintptr(syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS | syscall.CLONE_NEWCGROUP)
	if cfg.RestrictNet {
		flags |= syscall.CLONE_NEWNET
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flags}
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	// fd 3 (if present) is the comm socket back to mainErr; forward it.
	if comm := os.NewFile(3, "comm"); comm != nil {
		var st syscall.Stat_t
		if err := syscall.Fstat(3, &st); err == nil {
			cmd.ExtraFiles = []*os.File{comm}
		}
	}
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

func initMain() error {
	cfg, err := loadConfig(initEnv)
	if err != nil {
		return err
	}

	if err := syscall.Sethostname([]byte("runclaude")); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}

	procPath := filepath.Join(cfg.Rootfs, "proc")
	if err := os.MkdirAll(procPath, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("proc", procPath, "proc",
		syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mount proc: %w", err)
	}

	if err := os.Chdir(cfg.Rootfs); err != nil {
		return fmt.Errorf("chdir rootfs: %w", err)
	}
	if err := syscall.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	if err := os.Chdir(cfg.Cwd); err != nil {
		return fmt.Errorf("chdir %s: %w", cfg.Cwd, err)
	}

	var proxyPort int
	if enforceAllowlist(cfg) {
		port, err := setupNetwork(cfg)
		if err != nil {
			return err
		}
		proxyPort = port
	}

	if err := dropAllCaps(); err != nil {
		return err
	}

	os.Unsetenv(childEnv)
	os.Unsetenv(initEnv)
	if proxyPort > 0 {
		proxy := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
		os.Setenv("HTTP_PROXY", proxy)
		os.Setenv("HTTPS_PROXY", proxy)
		os.Setenv("http_proxy", proxy)
		os.Setenv("https_proxy", proxy)
		os.Setenv("NO_PROXY", "")
		os.Setenv("no_proxy", "")
	}
	if cfg.BundlePath != "" {
		os.Setenv("SSL_CERT_FILE", cfg.BundlePath)
		os.Setenv("REQUESTS_CA_BUNDLE", cfg.BundlePath)
		os.Setenv("CURL_CA_BUNDLE", cfg.BundlePath)
		os.Setenv("GIT_SSL_CAINFO", cfg.BundlePath)
	}
	if cfg.CAPath != "" {
		os.Setenv("NODE_EXTRA_CA_CERTS", cfg.CAPath)
	}
	bin, err := exec.LookPath(cfg.Command[0])
	if err != nil {
		return fmt.Errorf("lookup %s: %w", cfg.Command[0], err)
	}
	return runAsInit(bin, cfg.Command)
}

// runAsInit forks the user command as a child of this process (which is pid
// 1 of the new PID namespace) and stays around to forward signals and reap
// zombies. Staying as pid 1 ourselves is what lets the child receive
// SIGTSTP/SIGTTIN/SIGTTOU from the controlling tty — the kernel silently
// drops those for pid 1 of a PID ns when no handler is installed, which is
// what froze claude on ^Z before this shim was added.
func runAsInit(bin string, argv []string) error {
	cmd := exec.Command(bin)
	cmd.Args = argv
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", argv[0], err)
	}
	childPid := cmd.Process.Pid

	// Forward signals delivered to us (pid 1) onto the child. ^C / ^Z from
	// the tty go to the foreground process group directly and reach the
	// child without our help; this catches the explicit `kill <pid1>` case.
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs,
		syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP,
		syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()

	// Reap the child plus any orphans that got reparented to us. waitpid(-1)
	// blocks until any child changes state; we exit when the original child
	// terminates, propagating its exit code (or 128+signo on signal death,
	// matching shell convention).
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("wait4: %w", err)
		}
		if pid != childPid {
			continue
		}
		switch {
		case ws.Exited():
			os.Exit(ws.ExitStatus())
		case ws.Signaled():
			os.Exit(128 + int(ws.Signal()))
		default:
			os.Exit(0)
		}
	}
}

// exitError carries an explicit process exit code. Unlike a plain error (which
// log.Fatal reports and exits 1), it lets --exclusive report a specific status
// without a "runclaude:" log prefix, so callers can branch on the code.
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

func main() {
	var err error
	switch {
	case os.Getenv(initEnv) != "":
		err = initMain()
	case os.Getenv(childEnv) != "":
		err = childMain()
	default:
		err = mainErr()
	}
	if ee, ok := err.(exitError); ok {
		if ee.msg != "" {
			fmt.Fprintln(os.Stderr, ee.msg)
		}
		os.Exit(ee.code)
	}
	if err != nil {
		log.Fatal(err)
	}
}
