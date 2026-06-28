package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/hanwen/runclaude/agentclient"
)

// runServe is the host-side session hub. It owns the agentclient talking to the
// sandboxed claude over the stream-json pipes: stdinW carries prompt turns into
// the sandbox, stdoutR carries events back out.
//
// Phase 1 is deliberately minimal — a single local user, no web: read prompt
// lines from the host terminal, print the transcript. Later phases replace the
// body with the fan-out hub + tsnet web server while keeping this seam.
func runServe(stdinW, stdoutR *os.File) {
	client := agentclient.New(stdinW, stdoutR)

	// Feed the host terminal's lines in as prompt turns; terminal EOF (^D) ends
	// the session by closing claude's stdin.
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if err := client.SendPrompt(line); err != nil {
				log.Printf("serve: send prompt: %v", err)
				return
			}
		}
		client.CloseStdin()
	}()

	for ev := range client.Events() {
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" {
				log.Printf("serve: session %s started", ev.SessionID)
			}
		case "assistant":
			if t := ev.AssistantText(); t != "" {
				fmt.Println(t)
			}
		case "result":
			fmt.Printf("[result] %s\n", ev.ResultText())
		}
	}
	log.Printf("serve: session ended")
}
