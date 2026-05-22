package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
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

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

var defaultAllowedDomains = []string{
	"api.anthropic.com",
	"github.com", "*.github.com",
	"registry.npmjs.org",
	"pypi.org", "*.pypi.org", "files.pythonhosted.org",
	"crates.io", "static.crates.io",
	"proxy.golang.org", "sum.golang.org", "go.dev",
}

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

func enforceAllowlist(cfg *Config) bool {
	return cfg.RestrictNet && (len(cfg.AllowedDomains) > 0 || len(cfg.MitmDomains) > 0)
}

// matchDomain reports whether host matches any of patterns.
//
//   - exact "api.anthropic.com"          matches only that hostname
//   - leading "*.github.com"             matches "github.com" and any
//                                        subdomain at any depth (legacy form)
//   - mid-label "bedrock-runtime.*.amazonaws.com"
//                                        each "*" label matches exactly one
//                                        DNS label
func matchDomain(host string, patterns []string) bool {
	host = strings.ToLower(host)
	for _, p := range patterns {
		if matchPattern(host, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func matchPattern(host, pattern string) bool {
	if pattern == host {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		base := pattern[2:]
		if host == base || strings.HasSuffix(host, "."+base) {
			return true
		}
	}
	if strings.Contains(pattern, "*") {
		pParts := strings.Split(pattern, ".")
		hParts := strings.Split(host, ".")
		if len(pParts) == len(hParts) {
			match := true
			for i := range pParts {
				if pParts[i] != "*" && pParts[i] != hParts[i] {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

// ---------- CA / leaf certs ----------

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

// ---------- HTTP proxy ----------

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

// newMitmReverseProxy builds the reverse proxy used by the MITM path. The
// Director here is the single place where runclaude rewrites outgoing
// requests; tests exercise it directly to verify no header drift.
func newMitmReverseProxy(host string, target *url.URL, inject func(string, *http.Request), logger *log.Logger) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(target)
	origDirector := rp.Director
	rp.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = host
		req.Header.Del("Proxy-Connection")
		inject(host, req)
		// Suppress httputil.ReverseProxy's automatic X-Forwarded-For
		// injection so the upstream request matches the client byte-for-byte
		// (modulo bedrock re-signing). The explicit nil signals "omit".
		// Set *after* inject so the v4 signer doesn't see (and sign) the
		// placeholder.
		req.Header["X-Forwarded-For"] = nil
		logger.Printf("mitm req  %s %s", req.Method, req.URL.String())
	}
	rp.FlushInterval = 50 * time.Millisecond
	rp.ErrorLog = logger
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Printf("mitm upstream %s %s: %v", host, r.URL.Path, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
	}
	return rp
}

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
	rp := newMitmReverseProxy(host, target, inject, logger)

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
	binary.BigEndian.PutUint16(out[2:4], 0x8180)           // QR=1, RD=1, RA=1
	binary.BigEndian.PutUint16(out[6:8], uint16(len(ips))) // ANCOUNT
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

// hopByHopHeaders are the RFC 7230 §6.1 end-to-end forbidden headers that
// httputil.ReverseProxy removes between hops. We strip them ourselves before
// re-signing so the signature covers exactly the header set the upstream
// will see.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func stripHopByHopHeaders(h http.Header) {
	if conn := h.Get("Connection"); conn != "" {
		for _, name := range strings.Split(conn, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

// ---------- AWS Bedrock (SigV4) ----------

func isBedrockHost(host string) bool {
	return strings.HasPrefix(host, "bedrock-runtime.") &&
		strings.HasSuffix(host, ".amazonaws.com")
}

func bedrockRegion(host string) string {
	if !isBedrockHost(host) {
		return ""
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(host, "bedrock-runtime."), ".amazonaws.com")
	if strings.Contains(mid, ".") {
		return ""
	}
	return mid
}

// injectBedrock re-signs an in-flight request using host-side AWS credentials.
// The client (e.g., claude code's @aws-sdk/client-bedrock-runtime) may have
// signed with stub credentials we set in the container env; we strip those and
// recompute SigV4 against the real credentials. Body is unchanged.
func injectBedrock(host string, r *http.Request, provider aws.CredentialsProvider, signer *v4.Signer, logger *log.Logger) {
	injectBedrockAt(host, r, provider, signer, time.Now(), logger)
}

func injectBedrockAt(host string, r *http.Request, provider aws.CredentialsProvider, signer *v4.Signer, now time.Time, logger *log.Logger) {
	region := bedrockRegion(host)
	if region == "" {
		logger.Printf("bedrock: malformed host %q", host)
		return
	}
	// httputil.ReverseProxy strips hop-by-hop headers after the Director
	// runs. If we re-signed with them present, our SignedHeaders list
	// would name headers the upstream never sees, and Bedrock would
	// reject the signature. Strip them first so we sign exactly what
	// goes on the wire.
	stripHopByHopHeaders(r.Header)
	for _, h := range []string{
		"Authorization",
		"X-Amz-Date",
		"X-Amz-Security-Token",
		"X-Amz-Content-Sha256",
	} {
		r.Header.Del(h)
	}
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			logger.Printf("bedrock: read body: %v", err)
			return
		}
		body = b
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	ctx := r.Context()
	creds, err := provider.Retrieve(ctx)
	if err != nil {
		logger.Printf("bedrock: retrieve credentials: %v", err)
		return
	}
	if err := signer.SignHTTP(ctx, creds, r, hash, "bedrock", region, now); err != nil {
		logger.Printf("bedrock: sign: %v", err)
		return
	}
}
