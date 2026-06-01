// Package proxy implements the credential-injecting MITM HTTPS proxy used by
// runclaude. It is intentionally self-contained so external consumers (e.g.
// a Docker-sidecar binary) can reuse it without pulling in any of the Linux
// sandbox plumbing.
package proxy

import "strings"

// DefaultAllowedDomains is the set of egress endpoints runclaude permits by
// default when restricting the network. Callers may extend or replace this
// list.
var DefaultAllowedDomains = []string{
	"api.anthropic.com",
	"github.com", "*.github.com",
	"codeload.github.com",
	"release-assets.githubusercontent.com",
	"registry.npmjs.org",
	"pypi.org", "*.pypi.org", "files.pythonhosted.org",
	"crates.io", "static.crates.io",
	"proxy.golang.org", "sum.golang.org", "go.dev",
	"proxy.golang.org", "sum.golang.org", "go.dev", "*.go.dev",
	"dl.google.com", "storage.googleapis.com",
	"releases.bazel.build", "bcr.bazel.build",
}

// MatchDomain reports whether host matches any of patterns.
//
//   - exact "api.anthropic.com"          matches only that hostname
//   - leading "*.github.com"             matches "github.com" and any
//     subdomain at any depth (legacy form)
//   - mid-label "bedrock-runtime.*.amazonaws.com"
//     each "*" label matches exactly one
//     DNS label
func MatchDomain(host string, patterns []string) bool {
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
