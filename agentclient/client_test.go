package agentclient

import (
	"bytes"
	"strings"
	"testing"
)

// canned stdout lines mirroring a real claude stream-json session (captured
// from CLI 2.1.183), trimmed to the fields the client routes on.
const cannedStdout = `{"type":"system","subtype":"init","cwd":"/tmp","session_id":"sess-123","model":"claude-opus-4-8"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hel"},{"type":"text","text":"lo"}]},"session_id":"sess-123"}
not json, should be skipped
{"type":"result","subtype":"success","result":"Hello","session_id":"sess-123"}
`

func TestReadLoopDecodesAndRoutes(t *testing.T) {
	c := New(&bytes.Buffer{}, strings.NewReader(cannedStdout))
	var got []Event
	for ev := range c.Events() {
		got = append(got, ev)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (non-JSON line skipped): %+v", len(got), got)
	}
	if got[0].Type != "system" || got[0].Subtype != "init" || got[0].SessionID != "sess-123" {
		t.Errorf("init event = %+v", got[0])
	}
	if txt := got[1].AssistantText(); txt != "Hello" {
		t.Errorf("AssistantText = %q, want concatenated %q", txt, "Hello")
	}
	if got[2].Type != "result" || got[2].ResultText() != "Hello" {
		t.Errorf("result event = %+v, ResultText=%q", got[2], got[2].ResultText())
	}
}

func TestSendPromptWireFormat(t *testing.T) {
	var buf bytes.Buffer
	c := New(&buf, strings.NewReader(""))
	<-c.Events() // wait for read goroutine to finish (empty stream closes it)
	if err := c.SendPrompt("hi there"); err != nil {
		t.Fatal(err)
	}
	want := `{"type":"user","message":{"role":"user","content":"hi there"}}` + "\n"
	if buf.String() != want {
		t.Errorf("wire format:\n got %q\nwant %q", buf.String(), want)
	}
}

// AssistantText / ResultText must be inert on the wrong event type.
func TestTextAccessorsTypeGuarded(t *testing.T) {
	res := Event{Type: "result", Raw: []byte(`{"result":"x"}`)}
	if res.AssistantText() != "" {
		t.Error("AssistantText on result event should be empty")
	}
	asst := Event{Type: "assistant", Raw: []byte(`{"message":{"content":[{"type":"text","text":"y"}]}}`)}
	if asst.ResultText() != "" {
		t.Error("ResultText on assistant event should be empty")
	}
}
