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
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
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
	BundlePath     string   `json:"bundlePath"`
	CAPath         string   `json:"caPath"`
}

type Bind struct {
	Path     string `json:"path"`
	// Dest is the in-container path; defaults to Path when empty. Use it
	// to overlay a host-side sanitized copy at a path inside an already-
	// bound tree without modifying the host original.
	Dest     string `json:"dest,omitempty"`
	ReadOnly bool   `json:"ro,omitempty"`
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
			log.Fatalf("--mitm-upstream %q: expected host=url", kv)
		}
		out[host] = url
	}
	return out
}

type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

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

func claudeBinds(home string) ([]Bind, error) {
	var binds []string
	claudeDir := filepath.Join(home, ".claude")
	if entries, err := os.ReadDir(claudeDir); err == nil {
		for _, e := range entries {
			if credentialFiles[e.Name()] {
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
		out = append(out, jjConfigBinds()...)
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
	if cfgHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cfgHome = filepath.Join(home, ".config")
		}
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
func readClaudeAuth(home string) (apiKey, bearer string, err error) {
	for _, name := range []string{".credentials.json", "credentials.json"} {
		data, rerr := os.ReadFile(filepath.Join(home, ".claude", name))
		if rerr != nil {
			continue
		}
		var creds struct {
			ClaudeAiOauth struct {
				AccessToken string `json:"accessToken"`
			} `json:"claudeAiOauth"`
			APIKey string `json:"apiKey"`
		}
		if json.Unmarshal(data, &creds) == nil {
			if creds.ClaudeAiOauth.AccessToken != "" {
				return "", creds.ClaudeAiOauth.AccessToken, nil
			}
			if creds.APIKey != "" {
				return creds.APIKey, "", nil
			}
		}
	}
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		return k, "", nil
	}
	return "", "", fmt.Errorf("no claude credentials found (looked in ~/.claude/.credentials.json and $ANTHROPIC_API_KEY)")
}

func mainErr() error {
	dir, err := os.MkdirTemp("", "runclaude-")
	if err != nil {
		return err
	}
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

	var exposed stringSlice
	flag.Var(&exposed, "e", "expose host path into the container (repeatable)")
	mapPath := flag.Bool("map-path", true, "map all directories from $PATH")
	claudeMode := flag.Bool("claude", false, "bind files needed for `claude` and run it as the default command")
	restrictNet := flag.Bool("restrict-net", true, "run in a new network namespace; egress only via the in-process HTTP proxy and DNS server")
	var allowDomain domainList
	mapWorkspace := flag.Bool("map-workspace", true, "for git/jj workspaces map the originating repository")
	flag.Var(&allowDomain, "allow-domain",
		"allowed egress domain (repeatable); defaults to a built-in list; pass --allow-domain= to disable enforcement")
	injectAuth := flag.Bool("inject-auth", false,
		"MITM api.anthropic.com and inject $ANTHROPIC_API_KEY when the client doesn't supply credentials")
	proxyLog := flag.String("proxy-log", "",
		"path to write the proxy log to (default: <cache-dir>/proxy.log)")
	var mitmUpstream stringSlice
	flag.Var(&mitmUpstream, "mitm-upstream",
		"override MITM upstream for a host, format host=url (repeatable). For testing: point the proxy at a fake server.")
	anthropicKeyOverride := flag.String("anthropic-key", "",
		"override the Anthropic API key the proxy injects (bypasses ~/.claude/.credentials.json); for testing")
	anthropicBearerOverride := flag.String("anthropic-bearer", "",
		"override the Anthropic OAuth bearer the proxy injects (bypasses ~/.claude/.credentials.json); for testing")
	flag.Parse()

	allowedDomains := allowDomain.items
	if !allowDomain.set {
		allowedDomains = defaultAllowedDomains
	}

	var (
		mitmDomains []string
		apiKey      string
		bearer      string
	)
	// Pull env from .claude/settings.json into the host process. The
	// in-container claude reads settings.json itself; this lets runclaude
	// see the same effective env (CLAUDE_CODE_USE_BEDROCK, AWS_PROFILE,
	// etc.) without the user having to also export them in their shell.
	// Host env still wins — settings.json only fills unset variables.
	if *claudeMode {
		applyClaudeSettingsEnv(loadClaudeSettingsEnv(cwd))
	}
	useBedrock := *claudeMode && os.Getenv("CLAUDE_CODE_USE_BEDROCK") == "1"
	if *injectAuth || (*claudeMode && !useBedrock) {
		switch {
		case *anthropicKeyOverride != "" || *anthropicBearerOverride != "":
			apiKey = *anthropicKeyOverride
			bearer = *anthropicBearerOverride
		default:
			var err error
			apiKey, bearer, err = readClaudeAuth(home)
			if err != nil {
				if *injectAuth {
					return fmt.Errorf("--inject-auth: %w", err)
				}
				log.Printf("warning: --claude: %v", err)
			}
		}
		if apiKey != "" || bearer != "" {
			mitmDomains = append(mitmDomains, "api.anthropic.com")
			if !matchDomain("api.anthropic.com", allowedDomains) {
				allowedDomains = append(allowedDomains, "api.anthropic.com")
			}
		}
	}
	// Hosts mentioned via --mitm-upstream should also be MITM'd and on
	// the allowlist; the override is meaningless otherwise.
	for host := range parseMitmUpstream(mitmUpstream) {
		if !matchDomain(host, mitmDomains) {
			mitmDomains = append(mitmDomains, host)
		}
		if !matchDomain(host, allowedDomains) {
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
		if _, err := awsCfg.Credentials.Retrieve(context.Background()); err != nil {
			return fmt.Errorf("--claude+CLAUDE_CODE_USE_BEDROCK: %w (try 'aws sso login')", err)
		}
		awsCreds = awsCfg.Credentials
		awsSigner = v4.NewSigner()
		pattern := "bedrock-runtime.*.amazonaws.com"
		mitmDomains = append(mitmDomains, pattern)
		allowedDomains = append(allowedDomains, pattern)
	}

	sum := sha256.Sum256([]byte(cwd))
	cacheBase, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	cacheDir := filepath.Join(cacheBase, "runclaude", hex.EncodeToString(sum[:])[:16])
	for _, sub := range []string{"home", "tmp", "run"} {
		if err := os.MkdirAll(filepath.Join(cacheDir, sub), 0700); err != nil {
			return err
		}
	}
	if err := materializeGitConfig(home, filepath.Join(cacheDir, "home")); err != nil {
		return err
	}

	cfg := &Config{
		Rootfs:         rootfs,
		Home:           home,
		CacheDir:       cacheDir,
		Cwd:            cwd,
		Binds:          []Bind{{Path: cwd}},
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
		logger, err := openProxyLogger(logPath)
		if err != nil {
			return err
		}
		log.Printf("proxy log: %s", logPath)

		setup := &proxySetup{
			allowed:  allowedDomains,
			mitm:     mitmDomains,
			inject:   func(string, *http.Request) {},
			upstream: cfg.MitmUpstream,
		}
		if len(mitmDomains) > 0 {
			ca, err := loadOrCreateCA(filepath.Join(cacheBase, "runclaude"))
			if err != nil {
				return fmt.Errorf("load CA: %w", err)
			}
			caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Leaf.Raw})
			caCopy := filepath.Join(cacheDir, "tmp", "runclaude-ca.crt")
			if err := os.WriteFile(caCopy, caPEM, 0644); err != nil {
				return err
			}
			bundleCopy := filepath.Join(cacheDir, "tmp", "runclaude-bundle.crt")
			if err := writeBundle(caPEM, bundleCopy); err != nil {
				return err
			}
			cfg.BundlePath = "/tmp/runclaude-bundle.crt"
			cfg.CAPath = "/tmp/runclaude-ca.crt"
			setup.leaves = newLeafCache(ca)
			setup.inject = func(host string, r *http.Request) {
				switch {
				case host == "api.anthropic.com" && (bearer != "" || apiKey != ""):
					r.Header.Del("Authorization")
					r.Header.Del("x-api-key")
					if bearer != "" {
						r.Header.Set("Authorization", "Bearer "+bearer)
					} else {
						r.Header.Set("x-api-key", apiKey)
					}
					if r.Header.Get("anthropic-version") == "" {
						r.Header.Set("anthropic-version", "2023-06-01")
					}
				case isBedrockHost(host) && awsCreds != nil:
					injectBedrock(host, r, awsCreds, awsSigner, logger)
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
			// DNS allowlist covers both passthrough and mitm targets.
			dnsAllow := append([]string{}, allowedDomains...)
			dnsAllow = append(dnsAllow, mitmDomains...)
			go serveDNSUDP(dnsUDP, dnsAllow, logger)
			go serveDNSTCP(dnsTCP, dnsAllow, logger)
			serveProxy(setup, proxyLn, logger)
		}()
	}
	for _, e := range exposed {
		abs, err := filepath.Abs(e)
		if err != nil {
			return err
		}
		cfg.Binds = append(cfg.Binds, Bind{Path: abs})
	}
	if *mapPath {
		cfg.Binds = append(cfg.Binds, pathBinds(home)...)
	}
	if *mapWorkspace {
		cfg.Binds = append(cfg.Binds, workspaceBinds(cwd)...)
	}
	if *claudeMode {
		extra, err := claudeBinds(home)
		if err != nil {
			return err
		}
		cfg.Binds = append(cfg.Binds, extra...)
		cfg.Command = append([]string{"claude", "--dangerously-skip-permissions"}, flag.Args()...)
		if bearer != "" || apiKey != "" {
			if err := writeStubCredentials(filepath.Join(cacheDir, "home", ".claude")); err != nil {
				return err
			}
		}
		excludeBinds, err := vcsExcludeBinds(cwd, filepath.Join(cacheDir, "vcs"), ".claude/settings.json")
		if err != nil {
			return err
		}
		cfg.Binds = append(cfg.Binds, excludeBinds...)
		// Overlay a sanitized settings.json into the container so claude
		// doesn't see AWS_PROFILE etc. and try to authenticate itself —
		// the host proxy handles signing.
		settingsBinds, err := materializeClaudeSettings(cwd, filepath.Join(cacheDir, "claude"))
		if err != nil {
			return err
		}
		cfg.Binds = append(cfg.Binds, settingsBinds...)
	} else if args := flag.Args(); len(args) > 0 {
		cfg.Command = args
	}
	if len(cfg.Command) == 0 {
		cfg.Command = []string{"bash"}
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command("unshare",
		"--user", "--map-current-user", "--map-auto", "--keep-caps", "--mount",
		"--", self,
	)
	containerEnv := os.Environ()
	if useBedrock {
		containerEnv = scrubAWSCreds(containerEnv)
		// Stub credentials so the in-container aws-sdk doesn't refuse to
		// construct a request. The MITM strips these and signs with the real
		// host credentials before forwarding.
		containerEnv = append(containerEnv,
			"AWS_ACCESS_KEY_ID=AKIA000RUNCLAUDESTUB",
			"AWS_SECRET_ACCESS_KEY=runclaude-stub-secret-do-not-use",
		)
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
	return err
}

// scrubAWSCreds returns env without any AWS credential-bearing variables.
// AWS_REGION / AWS_PROFILE / AWS_DEFAULT_REGION are kept so the in-container
// aws-sdk still knows which region to target.
func scrubAWSCreds(env []string) []string {
	skip := map[string]bool{
		"AWS_ACCESS_KEY_ID":     true,
		"AWS_SECRET_ACCESS_KEY": true,
		"AWS_SESSION_TOKEN":     true,
		"AWS_SECURITY_TOKEN":    true,
		// AWS_PROFILE would have the SDK look in ~/.aws/credentials, which we
		// haven't bound into the container; safer to force env-only creds.
		"AWS_PROFILE":                 true,
		"AWS_CONFIG_FILE":             true,
		"AWS_SHARED_CREDENTIALS_FILE": true,
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
	entries, err := os.ReadDir("/")
	if err != nil {
		return err
	}
	for _, e := range entries {
		src := "/" + e.Name()
		if src == homePrefix || src == "/proc" || src == "/tmp" || src == "/run" || src == "/dev" {
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

	boundSeen := map[string]bool{}
	for _, d := range cfg.Binds {
		if boundSeen[d.dest()] {
			continue
		}
		boundSeen[d.dest()] = true
		dest := filepath.Join(cfg.Rootfs, d.dest())
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

	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "--clone-init")
	cmd.Env = append(os.Environ(), initEnv+"="+encodeConfig(cfg))
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

// cloneInitMain forks the container's init process into a fresh PID / IPC /
// UTS / cgroup namespace (and, when --restrict-net is on, a fresh net ns).
// The init process is what binds the proxy + DNS sockets and ships their fds
// back to mainErr via the inherited comm socket on fd 3.
func cloneInitMain() error {
	cfg, err := loadConfig(initEnv)
	if err != nil {
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
	return syscall.Exec(bin, cfg.Command, os.Environ())
}

func main() {
	cloneInit := len(os.Args) > 1 && os.Args[1] == "--clone-init"
	var err error
	switch {
	case os.Getenv(initEnv) != "" && cloneInit:
		err = cloneInitMain()
	case os.Getenv(initEnv) != "":
		err = initMain()
	case os.Getenv(childEnv) != "":
		err = childMain()
	default:
		err = mainErr()
	}
	if err != nil {
		log.Fatal(err)
	}
}
