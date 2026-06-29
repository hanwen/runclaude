package agentclient

import (
	"context"
	"io"
	"os/exec"
)

// StreamJSONFlags put claude into headless multi-turn stream-json mode: read
// user turns as JSON lines on stdin, emit events as JSON lines on stdout, keep
// the process (and session) alive until stdin closes. The permission flag is
// intentionally separate so each caller controls it — runclaude adds its own
// --dangerously-skip-permissions in main.go when assembling the command.
var StreamJSONFlags = []string{
	"--print",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--verbose",
	// Re-emit each user turn on stdout (type:"user", isReplay:true) carrying
	// claude's stable per-message uuid. The hub shows prompts instantly via its
	// own synthetic echo (the replay arrives ~2s later, with the reply); the
	// replay's value is the uuid, which the frontend reconciles onto the echoed
	// bubble so the same prompt dedups across a runclaude restart.
	"--replay-user-messages",
}

// Spawn launches claude in stream-json mode and returns a Client wired to its
// stdin/stdout plus the underlying *exec.Cmd (so the caller can Wait on it and
// observe the exit code). claude's stderr is sent to errw (nil discards it).
// extraArgs are appended after the stream-json flags — e.g. "--resume", id.
//
// This is the Phase 0 / standalone path. Inside the sandbox the client is built
// with New over inherited pipe fds instead, so claude runs contained.
func Spawn(ctx context.Context, errw io.Writer, extraArgs ...string) (*Client, *exec.Cmd, error) {
	args := append(append([]string{}, StreamJSONFlags...), "--dangerously-skip-permissions")
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, "claude", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stderr = errw
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return New(stdin, stdout), cmd, nil
}
