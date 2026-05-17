package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var defaultAllowedDomains = []string{
	"api.anthropic.com",
	"github.com", "*.github.com",
	"registry.npmjs.org",
	"pypi.org", "*.pypi.org", "files.pythonhosted.org",
	"crates.io", "static.crates.io",
	"proxy.golang.org", "sum.golang.org", "go.dev",
}

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
	Rootfs          string   `json:"rootfs"`
	Home            string   `json:"home"`
	CacheDir        string   `json:"cacheDir"`
	Cwd             string   `json:"cwd"`
	Binds           []string `json:"binds"`
	Command         []string `json:"command"`
	RestrictNet     bool     `json:"restrictNet"`
	ExposeLocalhost bool     `json:"exposeLocalhost"`
	AllowedDomains  []string `json:"allowedDomains"`
	MitmDomains     []string `json:"mitmDomains"`
	ProxyPort       int      `json:"proxyPort"`
	BundlePath      string   `json:"bundlePath"`
	CAPath          string   `json:"caPath"`
}

type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// domainList tracks whether the user explicitly passed --allow-domain
// (possibly with an empty value) so we know not to fall back to defaults.
type domainList struct {
	items []string
	set   bool
}

func (d *domainList) String() string { return strings.Join(d.items, ",") }
func (d *domainList) Set(v string) error {
	d.set = true
	if v != "" {
		d.items = append(d.items, v)
	}
	return nil
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

func enforceAllowlist(cfg *Config) bool {
	return cfg.RestrictNet && (len(cfg.AllowedDomains) > 0 || len(cfg.MitmDomains) > 0)
}

func loadOrCreateCA(dir string) (*tls.Certificate, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	crtPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if _, err := os.Stat(crtPath); err == nil {
		cert, err := tls.LoadX509KeyPair(crtPath, keyPath)
		if err != nil {
			return nil, err
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, err
		}
		cert.Leaf = leaf
		return &cert, nil
	}
	priv, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "runclaude CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}
	crtPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	if err := os.WriteFile(crtPath, crtPEM, 0644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}, nil
}

func writeBundle(caCertPEM []byte, dst string) error {
	var sys []byte
	for _, p := range []string{
		"/etc/ssl/certs/ca-bundle.crt",
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/cert.pem",
	} {
		if b, err := os.ReadFile(p); err == nil {
			sys = b
			break
		}
	}
	out := append(append([]byte(nil), sys...), '\n')
	out = append(out, caCertPEM...)
	return os.WriteFile(dst, out, 0644)
}

type leafCache struct {
	mu sync.Mutex
	m  map[string]*tls.Certificate
	ca *tls.Certificate
}

func newLeafCache(ca *tls.Certificate) *leafCache {
	return &leafCache{m: map[string]*tls.Certificate{}, ca: ca}
}

func (c *leafCache) get(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cert, ok := c.m[host]; ok {
		return cert, nil
	}
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial := big.NewInt(mathrand.Int63())
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, c.ca.Leaf, &priv.PublicKey, c.ca.PrivateKey)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{
		Certificate: [][]byte{der, c.ca.Certificate[0]},
		PrivateKey:  priv,
	}
	c.m[host] = cert
	return cert, nil
}

func matchDomain(host string, patterns []string) bool {
	host = strings.ToLower(host)
	for _, p := range patterns {
		p = strings.ToLower(p)
		if p == host {
			return true
		}
		if strings.HasPrefix(p, "*.") {
			base := p[2:]
			if host == base || strings.HasSuffix(host, "."+base) {
				return true
			}
		}
	}
	return false
}

type proxySetup struct {
	allowed []string
	mitm    []string
	leaves  *leafCache
	inject  func(host string, r *http.Request)
}

func startProxy(p *proxySetup, logPath string) (int, error) {
	logger := log.New(io.Discard, "proxy: ", log.LstdFlags|log.Lmicroseconds)
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return 0, fmt.Errorf("open proxy log: %w", err)
		}
		logger = log.New(f, "proxy: ", log.LstdFlags|log.Lmicroseconds)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		switch {
		case matchDomain(host, p.mitm):
			logger.Printf("mitm  %s %s", r.Method, host)
			if r.Method != http.MethodConnect {
				p.inject(host, r)
				proxyHTTP(w, r, logger)
				return
			}
			proxyMitm(w, r, host, p.leaves, p.inject, logger)
		case matchDomain(host, p.allowed):
			logger.Printf("allow %s %s", r.Method, host)
			if r.Method == http.MethodConnect {
				proxyConnect(w, r, logger)
			} else {
				proxyHTTP(w, r, logger)
			}
		default:
			logger.Printf("deny  %s %s", r.Method, host)
			http.Error(w, "denied by runclaude allowlist", http.StatusForbidden)
		}
	})

	srv := &http.Server{Handler: handler, ReadTimeout: 0, WriteTimeout: 0}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Printf("server: %v", err)
		}
	}()
	return port, nil
}

type singleConnListener struct {
	conn  net.Conn
	block chan struct{}
	once  sync.Once
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.conn != nil {
		c := l.conn
		l.conn = nil
		return c, nil
	}
	<-l.block
	return nil, io.EOF
}
func (l *singleConnListener) Close() error {
	l.once.Do(func() { close(l.block) })
	return nil
}
func (l *singleConnListener) Addr() net.Addr { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tcp" }
func (dummyAddr) String() string  { return "127.0.0.1:0" }

func proxyMitm(w http.ResponseWriter, r *http.Request, host string, leaves *leafCache, inject func(string, *http.Request), logger *log.Logger) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	fmt.Fprint(client, "HTTP/1.1 200 OK\r\n\r\n")

	leaf, err := leaves.get(host)
	if err != nil {
		logger.Printf("mint leaf for %s: %v", host, err)
		client.Close()
		return
	}
	tlsConn := tls.Server(client, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		NextProtos:   []string{"http/1.1"},
	})
	if err := tlsConn.Handshake(); err != nil {
		logger.Printf("mitm handshake %s: %v", host, err)
		tlsConn.Close()
		return
	}

	target, _ := url.Parse("https://" + host)
	rp := httputil.NewSingleHostReverseProxy(target)
	origDirector := rp.Director
	rp.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = host
		req.Header.Del("Proxy-Connection")
		inject(host, req)
		logger.Printf("mitm req  %s %s", req.Method, req.URL.String())
	}
	rp.FlushInterval = 50 * time.Millisecond
	rp.ErrorLog = logger
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Printf("mitm upstream %s %s: %v", host, r.URL.Path, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
	}

	logger.Printf("mitm tls  %s alpn=%q", host, tlsConn.ConnectionState().NegotiatedProtocol)
	ln := &singleConnListener{conn: tlsConn, block: make(chan struct{})}
	done := make(chan struct{})
	var closeOnce sync.Once
	srv := &http.Server{
		Handler:  rp,
		ErrorLog: logger,
		ConnState: func(c net.Conn, s http.ConnState) {
			logger.Printf("mitm conn %s -> %s", host, s)
			if s == http.StateClosed || s == http.StateHijacked {
				closeOnce.Do(func() {
					ln.Close()
					close(done)
				})
			}
		},
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != io.EOF {
			logger.Printf("mitm serve %s: %v", host, err)
		}
	}()
	<-done
	tlsConn.Close()
}

func proxyConnect(w http.ResponseWriter, r *http.Request, logger *log.Logger) {
	upstream, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	fmt.Fprint(client, "HTTP/1.1 200 OK\r\n\r\n")
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	upstream.Close()
	client.Close()
}

func proxyHTTP(w http.ResponseWriter, r *http.Request, logger *log.Logger) {
	r.RequestURI = ""
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// credentialFiles is the set of files inside ~/.claude that hold auth tokens.
// They are excluded from claudeBinds so the container can't read them.
var credentialFiles = map[string]bool{
	".credentials.json": true,
	"credentials.json":  true,
}

func claudeBinds(home string) ([]string, error) {
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
	var out []string
	for _, b := range binds {
		if _, err := os.Stat(b); err == nil {
			out = append(out, b)
		}
	}
	return out, nil
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
	claudeMode := flag.Bool("claude", false, "bind files needed for `claude` and run it as the default command")
	restrictNet := flag.Bool("restrict-net", true, "run in a new network namespace with pasta providing user-mode networking")
	exposeLocalhost := flag.Bool("expose-localhost", true, "expose host listening ports inside the netns (requires --restrict-net)")
	var allowDomain domainList
	flag.Var(&allowDomain, "allow-domain",
		"allowed egress domain (repeatable); defaults to a built-in list; pass --allow-domain= to disable enforcement")
	injectAuth := flag.Bool("inject-auth", false,
		"MITM api.anthropic.com and inject $ANTHROPIC_API_KEY when the client doesn't supply credentials")
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
	if *injectAuth || *claudeMode {
		var err error
		apiKey, bearer, err = readClaudeAuth(home)
		if err != nil {
			if *injectAuth {
				return fmt.Errorf("--inject-auth: %w", err)
			}
			// --claude without credentials: continue without injection.
			log.Printf("warning: --claude: %v", err)
		} else {
			mitmDomains = append(mitmDomains, "api.anthropic.com")
			if !matchDomain("api.anthropic.com", allowedDomains) {
				allowedDomains = append(allowedDomains, "api.anthropic.com")
			}
		}
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

	cfg := &Config{
		Rootfs:          rootfs,
		Home:            home,
		CacheDir:        cacheDir,
		Cwd:             cwd,
		Binds:           []string{cwd},
		RestrictNet:     *restrictNet,
		ExposeLocalhost: *exposeLocalhost,
		AllowedDomains:  allowedDomains,
		MitmDomains:     mitmDomains,
	}
	if enforceAllowlist(cfg) {
		setup := &proxySetup{
			allowed: allowedDomains,
			mitm:    mitmDomains,
			inject:  func(string, *http.Request) {},
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
				if host != "api.anthropic.com" {
					return
				}
				// Always overwrite: the client may have a stub or expired token
				// from a credentials file we declined to bind into the container.
				r.Header.Del("Authorization")
				r.Header.Del("x-api-key")
				switch {
				case bearer != "":
					r.Header.Set("Authorization", "Bearer "+bearer)
				case apiKey != "":
					r.Header.Set("x-api-key", apiKey)
				}
				if r.Header.Get("anthropic-version") == "" {
					r.Header.Set("anthropic-version", "2023-06-01")
				}
			}
		}
		logPath := filepath.Join(cacheDir, "proxy.log")
		port, err := startProxy(setup, logPath)
		if err != nil {
			return fmt.Errorf("start proxy: %w", err)
		}
		cfg.ProxyPort = port
		log.Printf("proxy log: %s", logPath)
	}
	for _, e := range exposed {
		abs, err := filepath.Abs(e)
		if err != nil {
			return err
		}
		cfg.Binds = append(cfg.Binds, abs)
	}
	if *claudeMode {
		extra, err := claudeBinds(home)
		if err != nil {
			return err
		}
		cfg.Binds = append(cfg.Binds, extra...)
		cfg.Command = []string{"claude", "--dangerously-skip-permissions"}
		if bearer != "" || apiKey != "" {
			if err := writeStubCredentials(filepath.Join(cacheDir, "home", ".claude")); err != nil {
				return err
			}
		}
	}
	if args := flag.Args(); len(args) > 0 {
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
	cmd.Env = append(os.Environ(), childEnv+"="+encodeConfig(cfg))
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	return cmd.Run()
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
		// Host's resolver (typically 127.0.0.53) isn't reachable from the
		// netns. Provide a stub-resolv.conf pointing at public resolvers
		// that pasta will proxy via the host stack.
		dest := filepath.Join(runInRoot, "systemd", "resolve")
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
		stub := "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"
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

	for _, d := range cfg.Binds {
		dest := filepath.Join(cfg.Rootfs, d)
		info, err := os.Stat(d)
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
		if err := syscall.Mount(d, dest, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("rbind %s -> %s: %w", d, dest, err)
		}
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	if cfg.RestrictNet {
		hostToNs := "none"
		if cfg.ExposeLocalhost {
			hostToNs = "auto"
		}
		nsToHostTCP := "none"
		if enforceAllowlist(cfg) {
			nsToHostTCP = fmt.Sprintf("%d", cfg.ProxyPort)
		}
		cmd = exec.Command("pasta",
			"--config-net", "--no-map-gw", "--ipv4-only", "--netns-only",
			"--quiet", "--log-file=/dev/null",
			"-a", "100.64.0.2", "-n", "24", "-g", "100.64.0.1",
			"-t", hostToNs, "-u", hostToNs, "-T", nsToHostTCP, "-U", "none",
			"--runas", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			"--", self, "--clone-init",
		)
	} else {
		cmd = exec.Command(self, "--clone-init")
	}
	cmd.Env = append(os.Environ(), initEnv+"="+encodeConfig(cfg))
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

// cloneInitMain runs after pasta (or directly) inside the netns, then clones
// into a fresh PID/IPC/UTS/cgroup namespace to become the container's init.
func cloneInitMain() error {
	cfg, err := loadConfig(initEnv)
	if err != nil {
		return err
	}
	if cfg.RestrictNet {
		blackhole := []string{
			"10.0.0.0/8",
			"172.16.0.0/12",
			"192.168.0.0/16",
			"169.254.169.254/32",
		}
		for _, r := range blackhole {
			out, err := exec.Command("ip", "-4", "route", "add", "blackhole", r).CombinedOutput()
			if err != nil {
				return fmt.Errorf("ip route add blackhole %s: %w: %s", r, err, out)
			}
		}
	}
	if enforceAllowlist(cfg) {
		rules := fmt.Sprintf(`
			table inet runclaude {
				chain output {
					type filter hook output priority filter; policy drop;
					oif lo accept
					ip daddr 100.64.0.0/24 tcp dport %d accept
					ip daddr {1.1.1.1, 8.8.8.8} udp dport 53 accept
					ip daddr {1.1.1.1, 8.8.8.8} tcp dport 53 accept
				}
			}`, cfg.ProxyPort)
		nft := exec.Command("nft", "-f", "-")
		nft.Stdin = strings.NewReader(rules)
		if out, err := nft.CombinedOutput(); err != nil {
			return fmt.Errorf("nft: %w: %s", err, out)
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), initEnv+"="+encodeConfig(cfg))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS | syscall.CLONE_NEWCGROUP,
	}
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
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

	if err := dropAllCaps(); err != nil {
		return err
	}

	os.Unsetenv(childEnv)
	os.Unsetenv(initEnv)
	if enforceAllowlist(cfg) {
		proxy := fmt.Sprintf("http://127.0.0.1:%d", cfg.ProxyPort)
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
		// Node wants only the *extra* CAs, not the whole system bundle.
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
