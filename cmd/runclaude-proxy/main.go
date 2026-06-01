// runproxy is a standalone credential-injecting MITM HTTPS proxy. It runs on
// the host (or in a sidecar) and rewrites Anthropic / AWS Bedrock requests
// from a sandboxed client — typically a Docker container — replacing the
// stub credentials the client carries with the real ones held only by this
// process.
//
// Quickstart:
//
//	runproxy --listen 127.0.0.1:8443 --export-bundle ./ca-bundle.crt --anthropic
//	docker run -e HTTPS_PROXY=http://host.docker.internal:8443 \
//	  -e NODE_EXTRA_CA_CERTS=/etc/runproxy/ca-bundle.crt \
//	  -e SSL_CERT_FILE=/etc/runproxy/ca-bundle.crt \
//	  -v $PWD/ca-bundle.crt:/etc/runproxy/ca-bundle.crt:ro \
//	  myimage
package main

import (
	"context"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/hanwen/runclaude/creds"
	"github.com/hanwen/runclaude/proxy"
)

type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "runproxy:", err)
		os.Exit(1)
	}
}

func run() error {
	defaultCA, err := defaultCADir()
	if err != nil {
		return err
	}

	listen := flag.String("listen", "127.0.0.1:8443", "address to bind the proxy on")
	caDir := flag.String("ca-dir", defaultCA, "directory holding the CA cert+key (created on first run)")
	exportCA := flag.String("export-ca", "", "if set, write the CA certificate PEM to this path")
	exportBundle := flag.String("export-bundle", "", "if set, write a system+CA bundle PEM to this path")
	logPath := flag.String("log", "", "proxy log path (default: stderr)")
	enableAnthropic := flag.Bool("anthropic", false, "MITM api.anthropic.com using credentials from --anthropic-home or $ANTHROPIC_API_KEY")
	anthropicHome := flag.String("anthropic-home", os.Getenv("HOME"), "home directory searched for .claude/.credentials.json")
	enableBedrock := flag.Bool("bedrock", false, "MITM bedrock-runtime.<region>.amazonaws.com using the default AWS SDK credential chain")
	var allow stringSlice
	flag.Var(&allow, "allow-domain", "additional passthrough domain (repeatable); default: built-in list")
	var defaults = flag.Bool("default-allowlist", true, "include the built-in default allowlist (Anthropic, registries, etc.)")
	var mitmUpstream stringSlice
	flag.Var(&mitmUpstream, "mitm-upstream", "override MITM upstream for a host, format host=url (repeatable); for testing")
	flag.Parse()

	if !*enableAnthropic && !*enableBedrock {
		return fmt.Errorf("at least one of --anthropic or --bedrock is required")
	}

	logger, err := openLogger(*logPath)
	if err != nil {
		return err
	}

	var allowed []string
	if *defaults {
		allowed = append(allowed, proxy.DefaultAllowedDomains...)
	}
	allowed = append(allowed, allow...)
	var mitm []string

	anthropic, bedrock, err := loadCreds(*enableAnthropic, *enableBedrock, *anthropicHome)
	if err != nil {
		return err
	}

	if *enableAnthropic {
		mitm = append(mitm, "api.anthropic.com")
		if !proxy.MatchDomain("api.anthropic.com", allowed) {
			allowed = append(allowed, "api.anthropic.com")
		}
	}
	var awsSigner *v4.Signer
	if *enableBedrock {
		pattern := "bedrock-runtime.*.amazonaws.com"
		mitm = append(mitm, pattern)
		if !proxy.MatchDomain("bedrock-runtime.us-east-1.amazonaws.com", allowed) {
			allowed = append(allowed, pattern)
		}
		awsSigner = v4.NewSigner()
	}

	upstreams, err := parseMitmUpstream(mitmUpstream)
	if err != nil {
		return err
	}
	for host := range upstreams {
		if !proxy.MatchDomain(host, mitm) {
			mitm = append(mitm, host)
		}
		if !proxy.MatchDomain(host, allowed) {
			allowed = append(allowed, host)
		}
	}

	ca, err := proxy.LoadOrCreateCA(*caDir)
	if err != nil {
		return fmt.Errorf("load CA: %w", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Leaf.Raw})
	if *exportCA != "" {
		if err := os.WriteFile(*exportCA, caPEM, 0644); err != nil {
			return fmt.Errorf("write --export-ca: %w", err)
		}
	}
	if *exportBundle != "" {
		if err := proxy.WriteBundle(caPEM, *exportBundle); err != nil {
			return fmt.Errorf("write --export-bundle: %w", err)
		}
	}

	setup := &proxy.Setup{
		Allowed:  allowed,
		Mitm:     mitm,
		Leaves:   proxy.NewLeafCache(ca),
		Upstream: upstreams,
		Inject:   func(string, *http.Request) {},
	}

	var tokens *creds.TokenSource
	if anthropic.Bearer != "" && anthropic.RefreshToken != "" {
		tokens = creds.NewTokenSource(anthropic.Bearer, anthropic.RefreshToken, anthropic.Path, logger)
	}
	setup.Inject = func(host string, r *http.Request) {
		switch {
		case host == "api.anthropic.com" && (anthropic.Bearer != "" || anthropic.APIKey != ""):
			r.Header.Del("Authorization")
			r.Header.Del("x-api-key")
			switch {
			case tokens != nil:
				r.Header.Set("Authorization", "Bearer "+tokens.Bearer())
			case anthropic.Bearer != "":
				r.Header.Set("Authorization", "Bearer "+anthropic.Bearer)
			default:
				r.Header.Set("x-api-key", anthropic.APIKey)
			}
			if r.Header.Get("anthropic-version") == "" {
				r.Header.Set("anthropic-version", "2023-06-01")
			}
		case proxy.IsBedrockHost(host) && bedrock != nil:
			proxy.InjectBedrock(host, r, bedrock, awsSigner, logger)
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

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", *listen, err)
	}
	fmt.Fprintf(os.Stderr, "runproxy: listening on %s (HTTPS_PROXY=http://%s)\n",
		ln.Addr(), ln.Addr())
	if *exportCA != "" {
		fmt.Fprintf(os.Stderr, "runproxy: CA cert at %s\n", *exportCA)
	}
	if *exportBundle != "" {
		fmt.Fprintf(os.Stderr, "runproxy: CA bundle at %s\n", *exportBundle)
	}
	proxy.Serve(setup, ln, logger)
	return nil
}

func loadCreds(wantAnthropic, wantBedrock bool, home string) (creds.AnthropicCredentials, aws.CredentialsProvider, error) {
	var a creds.AnthropicCredentials
	if wantAnthropic {
		c, err := creds.LoadAnthropicCredentials(home)
		if err != nil {
			return a, nil, fmt.Errorf("--anthropic: %w", err)
		}
		a = c
	}
	var b aws.CredentialsProvider
	if wantBedrock {
		cfg, err := awsconfig.LoadDefaultConfig(context.Background())
		if err != nil {
			return a, nil, fmt.Errorf("--bedrock: load AWS config: %w", err)
		}
		if _, err := cfg.Credentials.Retrieve(context.Background()); err != nil {
			return a, nil, fmt.Errorf("--bedrock: retrieve credentials: %w", err)
		}
		b = cfg.Credentials
	}
	return a, b, nil
}

func openLogger(path string) (*log.Logger, error) {
	if path == "" {
		return log.New(os.Stderr, "proxy: ", log.LstdFlags|log.Lmicroseconds), nil
	}
	return proxy.OpenLogger(path)
}

func defaultCADir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "runproxy"), nil
}

func parseMitmUpstream(specs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, s := range specs {
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("--mitm-upstream %q: want host=url", s)
		}
		out[s[:eq]] = s[eq+1:]
	}
	return out, nil
}
