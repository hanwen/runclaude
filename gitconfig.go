package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	gitconfig "github.com/go-git/go-git/v6/plumbing/format/config"
)

// gitConfigCredentialKeys lists per-section keys that the gitconfig(5)
// manual documents as carrying authentication material. The "credential"
// section is dropped wholesale, so it isn't in this map.
var gitConfigCredentialKeys = map[string][]string{
	"http":      {"extraHeader", "cookieFile"},
	"sendemail": {"smtpUser", "smtpPass"},
	"imap":      {"user", "pass"},
}

// gitConfigURLKeys lists per-section keys whose values are URLs that may
// embed userinfo (https://user:pass@host/...). We strip the userinfo while
// keeping the rest of the URL intact.
var gitConfigURLKeys = map[string][]string{
	"remote": {"url", "pushurl"},
	"url":    {"insteadOf", "pushInsteadOf"},
}

// sanitizeGitConfig parses a git config file, drops credential-bearing
// fields, and re-encodes it. Unknown sections, comments-as-values, and
// non-credential options pass through unchanged.
func sanitizeGitConfig(in []byte) ([]byte, error) {
	cfg := gitconfig.New()
	if err := gitconfig.NewDecoder(bytes.NewReader(in)).Decode(cfg); err != nil {
		return nil, err
	}

	// Drop the entire "credential" section (and any "credential.<url>"
	// subsections fall with it).
	kept := cfg.Sections[:0]
	for _, s := range cfg.Sections {
		if strings.EqualFold(s.Name, "credential") {
			continue
		}
		kept = append(kept, s)
	}
	cfg.Sections = kept

	for _, s := range cfg.Sections {
		if keys, ok := gitConfigCredentialKeys[strings.ToLower(s.Name)]; ok {
			for _, k := range keys {
				s.RemoveOption(k)
			}
			for _, sub := range s.Subsections {
				for _, k := range keys {
					sub.RemoveOption(k)
				}
			}
		}
		if keys, ok := gitConfigURLKeys[strings.ToLower(s.Name)]; ok {
			for _, k := range keys {
				stripUserinfoOption(s.Options, k)
			}
			for _, sub := range s.Subsections {
				for _, k := range keys {
					stripUserinfoOption(sub.Options, k)
				}
				// In [url "<base>"] subsections the subsection name
				// itself is a URL prefix and may embed credentials.
				if strings.EqualFold(s.Name, "url") {
					if u, err := url.Parse(sub.Name); err == nil && u.User != nil {
						u.User = nil
						sub.Name = u.String()
					}
				}
			}
		}
	}

	var out bytes.Buffer
	if err := gitconfig.NewEncoder(&out).Encode(cfg); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func stripUserinfoOption(opts gitconfig.Options, key string) {
	for _, o := range opts {
		if !o.IsKey(key) {
			continue
		}
		u, err := url.Parse(o.Value)
		if err != nil || u.User == nil {
			continue
		}
		u.User = nil
		o.Value = u.String()
	}
}

// materializeGitConfig reads the host user's git config files, strips
// credential-bearing fields, and writes the result under cacheHome so the
// container (whose HOME is cacheHome) sees a credential-free copy.
func materializeGitConfig(home, cacheHome string) error {
	// Resolve $GIT_CONFIG_GLOBAL too — git honors it as an override.
	type pair struct{ src, dst string }
	var pairs []pair
	if v := os.Getenv("GIT_CONFIG_GLOBAL"); v != "" {
		// Place it at $HOME/.gitconfig inside the container; we also
		// unset GIT_CONFIG_GLOBAL in the env so the container falls
		// back to the standard search path.
		pairs = append(pairs, pair{v, filepath.Join(cacheHome, ".gitconfig")})
	} else {
		pairs = append(pairs, pair{filepath.Join(home, ".gitconfig"), filepath.Join(cacheHome, ".gitconfig")})
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	pairs = append(pairs, pair{
		filepath.Join(xdg, "git", "config"),
		filepath.Join(cacheHome, ".config", "git", "config"),
	})

	for _, p := range pairs {
		data, err := os.ReadFile(p.src)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", p.src, err)
		}
		clean, err := sanitizeGitConfig(data)
		if err != nil {
			return fmt.Errorf("sanitize %s: %w", p.src, err)
		}
		if err := os.MkdirAll(filepath.Dir(p.dst), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(p.dst, clean, 0600); err != nil {
			return fmt.Errorf("write %s: %w", p.dst, err)
		}
	}
	return nil
}
