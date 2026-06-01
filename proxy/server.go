package proxy

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Setup configures the proxy. Inject is called with the destination hostname
// and the in-flight request before it is forwarded upstream; this is where
// credential rewriting happens. Upstream and Transports are optional overrides
// keyed by hostname.
type Setup struct {
	// Allowed lists hostnames that may be reached via CONNECT or plain HTTP
	// without TLS interception.
	Allowed []string
	// Mitm lists hostnames whose TLS connections will be terminated with a
	// leaf cert from Leaves and re-issued upstream after Inject runs.
	Mitm []string
	// Leaves mints the server certificates used for intercepted hosts. Must
	// be non-nil whenever Mitm is non-empty.
	Leaves *LeafCache
	// Inject rewrites the outgoing request — typically swapping stub
	// credentials for real ones. Required.
	Inject func(host string, r *http.Request)
	// Upstream optionally overrides the upstream URL for a MITM host. Empty
	// or missing entries fall back to "https://<host>".
	Upstream map[string]string
	// Transports optionally installs a per-host RoundTripper around the
	// upstream call (e.g. to handle 401-driven OAuth refresh).
	Transports map[string]http.RoundTripper
}

// OpenLogger opens a proxy log file at logPath. If logPath is empty, the
// returned logger discards everything.
func OpenLogger(logPath string) (*log.Logger, error) {
	if logPath == "" {
		return log.New(io.Discard, "proxy: ", log.LstdFlags|log.Lmicroseconds), nil
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open proxy log: %w", err)
	}
	return log.New(f, "proxy: ", log.LstdFlags|log.Lmicroseconds), nil
}

// Serve runs the proxy on ln until ln is closed. Connections to hosts
// matching p.Mitm are TLS-intercepted; hosts matching p.Allowed are passed
// through; everything else returns 403.
func Serve(p *Setup, ln net.Listener, logger *log.Logger) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		switch {
		case MatchDomain(host, p.Mitm):
			logger.Printf("mitm  %s %s", r.Method, host)
			if r.Method != http.MethodConnect {
				p.Inject(host, r)
				proxyHTTP(w, r, logger)
				return
			}
			proxyMitm(w, r, host, p.Leaves, p.Inject, p.Upstream[host], p.Transports[host], logger)
		case MatchDomain(host, p.Allowed):
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

// NewMitmReverseProxy builds the reverse proxy used by the MITM path. The
// Director here is the single place where runclaude rewrites outgoing
// requests; tests exercise it directly to verify no header drift.
func NewMitmReverseProxy(host string, target *url.URL, inject func(string, *http.Request), transport http.RoundTripper, logger *log.Logger) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(target)
	if transport != nil {
		rp.Transport = transport
	}
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

func proxyMitm(w http.ResponseWriter, r *http.Request, host string, leaves *LeafCache, inject func(string, *http.Request), upstreamOverride string, transport http.RoundTripper, logger *log.Logger) {
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

	leaf, err := leaves.Get(host)
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

	targetStr := "https://" + host
	if upstreamOverride != "" {
		targetStr = upstreamOverride
	}
	target, err := url.Parse(targetStr)
	if err != nil {
		logger.Printf("mitm: bad upstream %q: %v", targetStr, err)
		return
	}
	rp := NewMitmReverseProxy(host, target, inject, transport, logger)

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

// hopByHopHeaders are the RFC 7230 §6.1 end-to-end forbidden headers that
// httputil.ReverseProxy removes between hops. Inject implementations that
// re-sign requests (e.g. SigV4) should strip them first so the signature
// covers exactly the header set the upstream will see.
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

// StripHopByHopHeaders removes RFC 7230 §6.1 hop-by-hop headers from h.
func StripHopByHopHeaders(h http.Header) {
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
