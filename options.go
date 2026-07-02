package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Options is the JSON-serializable set of runclaude command-line flags.
// Save a subset as .claude/runclaude.json in the project directory to set
// local defaults; individual CLI flags always take precedence.
type Options struct {
	Expose        []string `json:"expose,omitempty"`
	Only          []string `json:"only,omitempty"`
	MapPath       *bool    `json:"mapPath,omitempty"`
	Claude        *bool    `json:"claude,omitempty"`
	ClaudeConfig  bool     `json:"claudeConfig,omitempty"`
	RestrictNet   *bool    `json:"restrictNet,omitempty"`
	AllowDomains  []string `json:"allowDomains,omitempty"`
	MapWorkspace  *bool    `json:"mapWorkspace,omitempty"`
	ProxyLog      string   `json:"proxyLog,omitempty"`
	ApproveListen string   `json:"approveListen,omitempty"`
	MitmUpstream  []string `json:"mitmUpstream,omitempty"`
	Serve         bool     `json:"serve,omitempty"`
	ServeAddr     string   `json:"serveAddr,omitempty"`
	ServeDev      bool     `json:"serveDev,omitempty"`
	ServeTailnet  string   `json:"serveTailnet,omitempty"`
	ServeResume   string   `json:"serveResume,omitempty"`
	ServeWriters  []string `json:"serveWriters,omitempty"`
}

// loadOptionsFile reads .claude/runclaude.json from cwd.
// Returns zero Options (and no error) when the file does not exist.
func loadOptionsFile(cwd string) (Options, error) {
	var opts Options
	data, err := os.ReadFile(filepath.Join(cwd, ".claude", "runclaude.json"))
	if os.IsNotExist(err) {
		return opts, nil
	}
	if err != nil {
		return opts, err
	}
	err = json.Unmarshal(data, &opts)
	return opts, err
}

// boolOr returns *p when p is non-nil, else def.
func boolOr(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}

func boolPtr(b bool) *bool { return &b }

// strOr returns s when non-empty, else def.
func strOr(s, def string) string {
	if s != "" {
		return s
	}
	return def
}
