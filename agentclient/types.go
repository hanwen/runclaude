// Package agentclient is a minimal Go port of the claude stream-json
// transport (the convenience layer the TS/Python SDKs put over the CLI).
//
// runclaude is a single zero-runtime-dep Go binary, so rather than shell out
// to the Node/Python SDK we speak the stream-json wire protocol directly:
// claude is launched headless with
//
//	claude --print --input-format stream-json --output-format stream-json --verbose
//
// each user turn is one JSON line written to stdin, and each event (system
// init, assistant message, result, ...) is one JSON line read from stdout.
package agentclient

import "encoding/json"

// Event is one decoded line from claude's stream-json stdout. Raw holds the
// original bytes so higher layers (the session hub) can fan the verbatim line
// out to web subscribers and replay history without re-serializing; the typed
// fields are the minimum the control logic needs.
type Event struct {
	Type      string          // "system", "assistant", "user", "result", "rate_limit_event", ...
	Subtype   string          // system: "init"; result: "success" / "error_*"
	SessionID string          // session_id carried on every event after init
	Raw       json.RawMessage // verbatim line, for fan-out / replay
}

// envelope is the small common header decoded from every stdout line to route
// it; the full payload stays in Event.Raw.
type envelope struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// userMessage is one turn written to claude's stdin. The wire shape is
// {"type":"user","message":{"role":"user","content":"..."}}.
type userMessage struct {
	Type    string `json:"type"`
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
}

// AssistantText extracts the concatenated text blocks from an assistant event,
// ignoring tool_use and other block types. Returns "" for non-assistant events
// or events with no text. Convenience for plain-text consumers (the Phase 0
// spike, logs); the web frontend renders Raw directly.
func (e Event) AssistantText() string {
	if e.Type != "assistant" {
		return ""
	}
	var a struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(e.Raw, &a) != nil {
		return ""
	}
	var s string
	for _, b := range a.Message.Content {
		if b.Type == "text" {
			s += b.Text
		}
	}
	return s
}

// ResultText extracts the final result string from a result event ("" otherwise).
func (e Event) ResultText() string {
	if e.Type != "result" {
		return ""
	}
	var r struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(e.Raw, &r) != nil {
		return ""
	}
	return r.Result
}
