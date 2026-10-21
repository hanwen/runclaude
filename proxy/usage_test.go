package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
)

// writeFrame encodes one AWS event-stream message wrapping a base64 payload,
// matching fakeapi/eventstream.go's format.
func writeFrame(buf *bytes.Buffer, inner map[string]any) {
	innerJSON, _ := json.Marshal(inner)
	payload, _ := json.Marshal(map[string]any{
		"bytes": base64.StdEncoding.EncodeToString(innerJSON),
	})
	var headers bytes.Buffer
	appendStringHeader(&headers, ":event-type", "chunk")
	hdr := headers.Bytes()

	totalLen := uint32(4 + 4 + 4 + len(hdr) + len(payload) + 4)
	var prelude [8]byte
	binary.BigEndian.PutUint32(prelude[0:4], totalLen)
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(hdr)))

	var msg bytes.Buffer
	msg.Write(prelude[:])
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], crc32.ChecksumIEEE(prelude[:]))
	msg.Write(c[:])
	msg.Write(hdr)
	msg.Write(payload)
	binary.BigEndian.PutUint32(c[:], crc32.ChecksumIEEE(msg.Bytes()))
	msg.Write(c[:])
	buf.Write(msg.Bytes())
}

func appendStringHeader(buf *bytes.Buffer, name, value string) {
	buf.WriteByte(byte(len(name)))
	buf.WriteString(name)
	buf.WriteByte(7)
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(value)))
	buf.Write(l[:])
	buf.WriteString(value)
}

func TestParseEventStreamUsage(t *testing.T) {
	var stream bytes.Buffer
	writeFrame(&stream, map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"usage": map[string]any{
				"input_tokens":                100,
				"output_tokens":               0,
				"cache_read_input_tokens":     40,
				"cache_creation_input_tokens": 10,
			},
		},
	})
	writeFrame(&stream, map[string]any{"type": "content_block_delta"})
	// Two message_delta events: output is cumulative, so the last wins.
	writeFrame(&stream, map[string]any{"type": "message_delta", "usage": map[string]any{"output_tokens": 25}})
	writeFrame(&stream, map[string]any{"type": "message_delta", "usage": map[string]any{"output_tokens": 55}})
	writeFrame(&stream, map[string]any{"type": "message_stop"})

	got := parseEventStreamUsage(&stream, log.New(io.Discard, "", 0))
	want := TokenUsage{Input: 100, Output: 55, CacheRead: 40, CacheWrite: 10}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseJSONUsage(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":12,"output_tokens":34}}`)
	got := parseJSONUsage(body)
	want := TokenUsage{Input: 12, Output: 34}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestModelFromBedrockPath(t *testing.T) {
	cases := map[string]string{
		"/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke":                         "anthropic.claude-3-5-sonnet-20241022-v2:0",
		"/model/us.anthropic.claude-opus-4-1-20250805-v1%3A0/invoke-with-response-stream": "us.anthropic.claude-opus-4-1-20250805-v1:0",
		"/v1/messages": "",
		"/model/":      "",
	}
	for path, want := range cases {
		if got := modelFromBedrockPath(path); got != want {
			t.Errorf("modelFromBedrockPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestPriceForSpecificity(t *testing.T) {
	cases := []struct {
		model string
		want  modelPrice
	}{
		// 5.5 entries must win over the 5 entries they contain, and 5.5
		// cache reads are 0.05x input (not 0.1x).
		{"us.anthropic.claude-opus-5-5-20260401-v1:0", modelPrice{4, 20, 5, 0.20}},
		{"us.anthropic.claude-opus-5-20251101-v1:0", modelPrice{5, 25, 6.25, 0.50}},
		{"us.anthropic.claude-sonnet-5-5-20260401-v1:0", modelPrice{2, 10, 2.50, 0.10}},
		{"us.anthropic.claude-sonnet-5-20250929-v1:0", modelPrice{2, 10, 2.50, 0.20}},
		// 3-5-haiku must not fall through to the cheaper 3-haiku entry.
		{"us.anthropic.claude-3-5-haiku-20241022-v1:0", modelPrice{0.8, 4, 1.0, 0.08}},
	}
	for _, c := range cases {
		got, ok := priceFor(c.model)
		if !ok || got != c.want {
			t.Errorf("priceFor(%q) = %+v ok=%v, want %+v", c.model, got, ok, c.want)
		}
	}
	if _, ok := priceFor("us.anthropic.claude-9-0-foo-v1:0"); ok {
		t.Error("unknown model should not resolve a price")
	}
}

func TestUsageTrackerObserveAndReport(t *testing.T) {
	tr := NewUsageTracker(log.New(io.Discard, "", 0))

	var stream bytes.Buffer
	writeFrame(&stream, map[string]any{
		"type":    "message_start",
		"message": map[string]any{"usage": map[string]any{"input_tokens": 1000, "output_tokens": 0}},
	})
	writeFrame(&stream, map[string]any{"type": "message_delta", "usage": map[string]any{"output_tokens": 500}})

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/vnd.amazon.eventstream"}},
		Body:       io.NopCloser(bytes.NewReader(stream.Bytes())),
	}
	tr.Observe("/model/us.anthropic.claude-sonnet-4-5-20250929-v1:0/invoke-with-response-stream", resp)
	// Drain the wrapped body: the parser runs as the client reads.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	report := tr.Report()
	if !strings.Contains(report, "claude-sonnet-4-5") {
		t.Errorf("report missing model line:\n%s", report)
	}
	// sonnet-4: 1000/1e6*3 + 500/1e6*15 = 0.003 + 0.0075 = 0.0105
	if !strings.Contains(report, "$0.0105") {
		t.Errorf("report missing expected cost $0.0105:\n%s", report)
	}
}

func TestParseKimiUsage(t *testing.T) {
	// Kimi/Moonshot report OpenAI-style usage: prompt_tokens includes cached
	// tokens, which we split out so input excludes cache.
	t.Run("json", func(t *testing.T) {
		body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":300}}}`)
		got := parseJSONUsage(body)
		want := TokenUsage{Input: 700, Output: 200, CacheRead: 300}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
	t.Run("stream", func(t *testing.T) {
		var stream bytes.Buffer
		writeFrame(&stream, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "hi"}}}})
		writeFrame(&stream, map[string]any{
			"usage": map[string]any{"prompt_tokens": 50, "completion_tokens": 12},
		})
		got := parseEventStreamUsage(&stream, log.New(io.Discard, "", 0))
		want := TokenUsage{Input: 50, Output: 12}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
}

func TestPriceForKimi(t *testing.T) {
	p, ok := priceFor("moonshotai.kimi-k2-instruct-0905")
	if !ok || (p != modelPrice{0.60, 2.50, 0.60, 0.15}) {
		t.Errorf("kimi-k2 price = %+v ok=%v", p, ok)
	}
}
