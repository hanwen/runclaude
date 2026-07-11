package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// ensureProjectTrusted marks cwd as trusted in ~/.claude.json (the same
// fields accepting the "Quick safety check" dialog writes), so claude
// skips the first-run trust prompt. Inside runclaude the sandbox is the
// trust boundary — the dialog asks whether the folder can be trusted with
// host access the container doesn't grant in the first place.
//
// ~/.claude.json is live-bound into the container, so patching the host
// file before launch is exactly equivalent to the user accepting the
// dialog. A missing ~/.claude.json is left alone: first-run onboarding
// needs to happen for real.
func ensureProjectTrusted(home, cwd string) error {
	path := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// UseNumber: the file holds counters/timestamps that must round-trip
	// without float mangling.
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return err
	}
	projects, ok := root["projects"].(map[string]any)
	if !ok {
		projects = map[string]any{}
		root["projects"] = projects
	}
	proj, ok := projects[cwd].(map[string]any)
	if !ok {
		proj = map[string]any{}
		projects[cwd] = proj
	}
	if proj["hasTrustDialogAccepted"] == true && proj["hasCompletedProjectOnboarding"] == true {
		return nil // already trusted; don't rewrite the file
	}
	proj["hasTrustDialogAccepted"] = true
	proj["hasCompletedProjectOnboarding"] = true

	out, err := json.Marshal(root)
	if err != nil {
		return err
	}
	// Atomic replace: claude is not running yet, but the host claude (or
	// another runclaude) might be.
	tmp := path + ".runclaude-tmp"
	if err := os.WriteFile(tmp, out, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
