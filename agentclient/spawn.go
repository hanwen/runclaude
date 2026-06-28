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
	// Echo each user turn back on stdout (as type:"user", isReplay:true) so the
	// session hub records prompts in the canonical transcript and every viewer
	// sees who asked what, not just the assistant's replies.
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
