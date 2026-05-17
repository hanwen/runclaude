package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
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
	Rootfs         string   `json:"rootfs"`
	Home           string   `json:"home"`
	CacheDir       string   `json:"cacheDir"`
	Cwd            string   `json:"cwd"`
	Binds          []string `json:"binds"`
	Command        []string `json:"command"`
	RestrictNet    bool     `json:"restrictNet"`
	AllowedDomains []string `json:"allowedDomains"`
	MitmDomains    []string `json:"mitmDomains"`
	BundlePath     string   `json:"bundlePath"`
	CAPath         string   `json:"caPath"`
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

func openProxyLogger(logPath string) (*log.Logger, error) {
	if logPath == "" {
		return log.New(io.Discard, "proxy: ", log.LstdFlags|log.Lmicroseconds), nil
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open proxy log: %w", err)
	}
	return log.New(f, "proxy: ", log.LstdFlags|log.Lmicroseconds), nil
}

func serveProxy(p *proxySetup, ln net.Listener, logger *log.Logger) {
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
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		logger.Printf("server: %v", err)
	}
}

// ---------- DNS server (in-process, minimal) ----------
//
// Serves A/AAAA queries from the container netns: parses the question,
// resolves on the host via net.DefaultResolver, returns answers. Anything
// outside the allowlist returns NXDOMAIN. Anything other than A/AAAA returns
// NOTIMP. This is intentionally not a recursive resolver -- it is the
// container's only path to name resolution, and the allowlist controls what
// the proxy will let through anyway.

func parseDNSQuery(buf []byte) (id uint16, name string, qtype uint16, err error) {
	if len(buf) < 12 {
		return 0, "", 0, fmt.Errorf("query too short")
	}
	id = binary.BigEndian.Uint16(buf[0:2])
	if qd := binary.BigEndian.Uint16(buf[4:6]); qd != 1 {
		return 0, "", 0, fmt.Errorf("qdcount %d != 1", qd)
	}
	pos := 12
	var parts []string
	for {
		if pos >= len(buf) {
			return 0, "", 0, fmt.Errorf("name truncated")
		}
		n := int(buf[pos])
		pos++
		if n == 0 {
			break
		}
		if n&0xc0 != 0 {
			return 0, "", 0, fmt.Errorf("unexpected compression in question")
		}
		if pos+n > len(buf) {
			return 0, "", 0, fmt.Errorf("label truncated")
		}
		parts = append(parts, string(buf[pos:pos+n]))
		pos += n
	}
	if pos+4 > len(buf) {
		return 0, "", 0, fmt.Errorf("qtype/qclass truncated")
	}
	qtype = binary.BigEndian.Uint16(buf[pos : pos+2])
	return id, strings.Join(parts, "."), qtype, nil
}

func questionEnd(buf []byte) int {
	pos := 12
	for buf[pos] != 0 {
		pos += 1 + int(buf[pos])
	}
	return pos + 1 + 4 // null label + qtype + qclass
}

func buildDNSAnswer(req []byte, ips []net.IP, qtype uint16) []byte {
	qend := questionEnd(req)
	out := make([]byte, 0, 512)
	out = append(out, req[:qend]...)
	binary.BigEndian.PutUint16(out[2:4], 0x8180)             // QR=1, RD=1, RA=1
	binary.BigEndian.PutUint16(out[6:8], uint16(len(ips)))   // ANCOUNT
	for _, ip := range ips {
		var rdata []byte
		var rtype uint16
		if v4 := ip.To4(); v4 != nil && qtype == 1 {
			rdata, rtype = v4, 1
		} else if v6 := ip.To16(); v6 != nil && qtype == 28 && ip.To4() == nil {
			rdata, rtype = v6, 28
		} else {
			continue
		}
		out = append(out, 0xc0, 0x0c) // name = pointer to offset 12 (start of question name)
		out = binary.BigEndian.AppendUint16(out, rtype)
		out = binary.BigEndian.AppendUint16(out, 1) // class IN
		out = binary.BigEndian.AppendUint32(out, 60)
		out = binary.BigEndian.AppendUint16(out, uint16(len(rdata)))
		out = append(out, rdata...)
	}
	return out
}

func buildDNSError(req []byte, rcode uint16) []byte {
	out := make([]byte, len(req))
	copy(out, req)
	binary.BigEndian.PutUint16(out[2:4], 0x8180|rcode)
	return out
}

func handleDNSQuery(buf []byte, allowed []string, logger *log.Logger) []byte {
	_, name, qtype, err := parseDNSQuery(buf)
	if err != nil {
		logger.Printf("dns: parse: %v", err)
		return buildDNSError(buf, 1) // FORMERR
	}
	name = strings.TrimSuffix(name, ".")
	if !matchDomain(name, allowed) {
		logger.Printf("dns: deny %s", name)
		return buildDNSError(buf, 3) // NXDOMAIN
	}
	if qtype != 1 && qtype != 28 {
		logger.Printf("dns: notimp %s type=%d", name, qtype)
		return buildDNSError(buf, 4) // NOTIMP
	}
	network := "ip4"
	if qtype == 28 {
		network = "ip6"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, network, name)
	if err != nil {
		logger.Printf("dns: lookup %s %s: %v", network, name, err)
		return buildDNSError(buf, 2) // SERVFAIL
	}
	logger.Printf("dns: %s %s -> %v", network, name, ips)
	return buildDNSAnswer(buf, ips, qtype)
}

func serveDNSUDP(conn net.PacketConn, allowed []string, logger *log.Logger) {
	defer conn.Close()
	buf := make([]byte, 1500)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		req := make([]byte, n)
		copy(req, buf[:n])
		go func() {
			resp := handleDNSQuery(req, allowed, logger)
			if resp != nil {
				conn.WriteTo(resp, addr)
			}
		}()
	}
}

func serveDNSTCP(ln net.Listener, allowed []string, logger *log.Logger) {
	defer ln.Close()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			var lb [2]byte
			if _, err := io.ReadFull(c, lb[:]); err != nil {
				return
			}
			msg := make([]byte, binary.BigEndian.Uint16(lb[:]))
			if _, err := io.ReadFull(c, msg); err != nil {
				return
			}
			resp := handleDNSQuery(msg, allowed, logger)
			if resp == nil {
				return
			}
			binary.BigEndian.PutUint16(lb[:], uint16(len(resp)))
			c.Write(lb[:])
			c.Write(resp)
		}(c)
	}
}

// ---------- fd-passing helpers ----------

func sendFds(sock *os.File, fds []int) error {
	rights := syscall.UnixRights(fds...)
	// One dummy data byte: SCM_RIGHTS without payload is allowed but some
	// libcs require at least one byte. Stay portable.
	return syscall.Sendmsg(int(sock.Fd()), []byte{0}, rights, nil, 0)
}

func recvFds(sock *os.File, n int) ([]int, error) {
	oob := make([]byte, syscall.CmsgSpace(n*4))
	buf := make([]byte, 1)
	_, oobn, _, _, err := syscall.Recvmsg(int(sock.Fd()), buf, oob, 0)
	if err != nil {
		return nil, err
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}
	var fds []int
	for _, m := range msgs {
		fs, err := syscall.ParseUnixRights(&m)
		if err != nil {
			return nil, err
		}
		fds = append(fds, fs...)
	}
	if len(fds) != n {
		for _, fd := range fds {
			syscall.Close(fd)
		}
		return nil, fmt.Errorf("expected %d fds, got %d", n, len(fds))
	}
	return fds, nil
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
	restrictNet := flag.Bool("restrict-net", true, "run in a new network namespace; egress only via the in-process HTTP proxy and DNS server")
	var allowDomain domainList
	flag.Var(&allowDomain, "allow-domain",
		"allowed egress domain (repeatable); defaults to a built-in list; pass --allow-domain= to disable enforcement")
	injectAuth := flag.Bool("inject-auth", false,
		"MITM api.anthropic.com and inject $ANTHROPIC_API_KEY when the client doesn't supply credentials")
	proxyLog := flag.String("proxy-log", "",
		"path to write the proxy log to (default: <cache-dir>/proxy.log)")
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
		Rootfs:         rootfs,
		Home:           home,
		CacheDir:       cacheDir,
		Cwd:            cwd,
		Binds:          []string{cwd},
		RestrictNet:    *restrictNet,
		AllowedDomains: allowedDomains,
		MitmDomains:    mitmDomains,
	}

	// commHostEnd is our side of the socketpair used for fd passing back from
	// init. commChildFile is passed via ExtraFiles all the way to init.
	var commHostEnd *os.File
	var commChildFile *os.File
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
		logPath := *proxyLog
		if logPath == "" {
			logPath = filepath.Join(cacheDir, "proxy.log")
		}
		logger, err := openProxyLogger(logPath)
		if err != nil {
			return err
		}
		log.Printf("proxy log: %s", logPath)

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
	if commChildFile != nil {
		cmd.ExtraFiles = []*os.File{commChildFile}
	}
	err = cmd.Run()
	if commChildFile != nil {
		commChildFile.Close()
	}
	return err
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

// setupNetwork brings up loopback, binds the proxy + DNS listener sockets
// inside this (container) netns, ships their fds back to mainErr via the
// fd-3 comm socket inherited through the exec chain, and installs an
// nftables policy that drops everything not on lo.  Returns the bound proxy
// port for HTTPS_PROXY env construction.
func setupNetwork(cfg *Config) (int, error) {
	if out, err := exec.Command("ip", "link", "set", "lo", "up").CombinedOutput(); err != nil {
		return 0, fmt.Errorf("ip link set lo up: %w: %s", err, out)
	}

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen proxy: %w", err)
	}
	port := proxyLn.Addr().(*net.TCPAddr).Port
	proxyFile, err := proxyLn.(*net.TCPListener).File()
	if err != nil {
		return 0, fmt.Errorf("listener fd: %w", err)
	}

	udp, err := net.ListenPacket("udp", "127.0.0.1:53")
	if err != nil {
		return 0, fmt.Errorf("listen dns udp: %w", err)
	}
	dnsUDPFile, err := udp.(*net.UDPConn).File()
	if err != nil {
		return 0, fmt.Errorf("udp fd: %w", err)
	}

	tcpDNS, err := net.Listen("tcp", "127.0.0.1:53")
	if err != nil {
		return 0, fmt.Errorf("listen dns tcp: %w", err)
	}
	dnsTCPFile, err := tcpDNS.(*net.TCPListener).File()
	if err != nil {
		return 0, fmt.Errorf("dns tcp fd: %w", err)
	}

	comm := os.NewFile(3, "comm")
	if comm == nil {
		return 0, fmt.Errorf("fd 3 (comm socket) not present")
	}
	if err := sendFds(comm, []int{int(proxyFile.Fd()), int(dnsUDPFile.Fd()), int(dnsTCPFile.Fd())}); err != nil {
		return 0, fmt.Errorf("sendFds: %w", err)
	}
	// We don't need these in-process anymore; the host side serves them.
	proxyLn.Close()
	udp.Close()
	tcpDNS.Close()
	proxyFile.Close()
	dnsUDPFile.Close()
	dnsTCPFile.Close()
	comm.Close()

	rules := `
		table inet runclaude {
			chain output {
				type filter hook output priority filter; policy drop;
				oif lo accept
			}
		}`
	nft := exec.Command("nft", "-f", "-")
	nft.Stdin = strings.NewReader(rules)
	if out, err := nft.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("nft: %w: %s", err, out)
	}
	return port, nil
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
