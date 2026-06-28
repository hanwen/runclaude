// Command agentspike is the Phase 0 protocol spike: it drives claude headless
// over the stream-json protocol (no sandbox, no web) and prints the streamed
// transcript. It exists to validate the agentclient wire port against the real
// CLI. Each argument is sent as a prompt turn in one persistent session.
//
//	go run ./cmd/agentspike "what is 2+2?" "now multiply that by 3"
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hanwen/runclaude/agentclient"
)

func main() {
	prompts := os.Args[1:]
	if len(prompts) == 0 {
		prompts = []string{"Say hello in one short sentence."}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client, cmd, err := agentclient.Spawn(ctx, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "spawn:", err)
		os.Exit(1)
	}

	// Single consumer of the event stream. Feed prompts as turns complete:
	// send the first now, send the next each time a result event closes a
	// turn, then close stdin so claude exits cleanly.
	next := 0
	send := func() {
		fmt.Printf("\n\x1b[1m> %s\x1b[0m\n", prompts[next])
		if err := client.SendPrompt(prompts[next]); err != nil {
			fmt.Fprintln(os.Stderr, "send:", err)
		}
		next++
	}
	send()
	for ev := range client.Events() {
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" {
				fmt.Printf("[session %s]\n", ev.SessionID)
			}
		case "assistant":
			if t := ev.AssistantText(); t != "" {
				fmt.Println(t)
			}
		case "result":
			fmt.Printf("[result: %s]\n", ev.ResultText())
			if next < len(prompts) {
				send()
			} else {
				client.CloseStdin()
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		fmt.Fprintln(os.Stderr, "claude exited:", err)
		os.Exit(1)
	}
}
