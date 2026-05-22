package fakeapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// FixedReply is the text the fake server returns for every prompt. Tests
// grep for this exact string in claude's stdout.
const FixedReply = "I am a fake anthropic API server"

// Config controls credential validation. Set APIKey to enable the
// Anthropic native API route; set AWSAccessKey/AWSSecretKey to enable the
// Bedrock InvokeModel route. Both may be enabled simultaneously.
type Config struct {
	APIKey          string
	AWSAccessKey    string
	AWSSecretKey    string
	AWSSessionToken string
	AWSRegion       string
	Logger          *log.Logger
}

// Handler returns an http.Handler implementing a minimal Anthropic /
// Bedrock-compatible API. It validates credentials per Config and replies
// with FixedReply for every accepted request.
func Handler(cfg Config) http.Handler {
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", anthropicMessages(cfg))
	mux.HandleFunc("/model/", bedrockInvoke(cfg))
	mux.HandleFunc("/", catchAll(cfg))
	return mux
}

// catchAll responds to the misc endpoints the claude CLI pings during
// startup. Some endpoints have schema expectations (claude crashes on
// unexpected JSON shape), so we hand-roll responses for the known ones
// and fall back to {"ok":true} for the rest.
func catchAll(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg.Logger.Printf("catchall: %s %s", r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		body := catchAllBody(r.URL.Path)
		_, _ = w.Write(body)
	}
}

func catchAllBody(path string) []byte {
	switch {
	case strings.HasPrefix(path, "/mcp-registry/"):
		// Claude expects an array of servers; an empty list is fine.
		return []byte(`{"servers":[]}`)
	case strings.HasPrefix(path, "/api/oauth/profile"),
		strings.HasPrefix(path, "/api/account"):
		return []byte(`{"uuid":"fake-uuid","email_address":"fake@example.com","full_name":"Fake User","organization":{"uuid":"fake-org","name":"Fake Org"}}`)
	case strings.HasPrefix(path, "/api/organizations"):
		return []byte(`[{"uuid":"fake-org","name":"Fake Org"}]`)
	case strings.HasPrefix(path, "/api/event_logging"):
		return []byte(`{"ok":true}`)
	default:
		return []byte(`{"ok":true}`)
	}
}

func anthropicMessages(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.APIKey == "" {
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"server not configured for anthropic api"}}`, http.StatusInternalServerError)
			return
		}
		got := r.Header.Get("x-api-key")
		if got == "" {
			if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
				got = b
			}
		}
		if got != cfg.APIKey {
			cfg.Logger.Printf("anthropic: auth fail (got %q)", got)
			http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`, http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		stream := bodyIsStreaming(body)
		cfg.Logger.Printf("anthropic: ok stream=%v", stream)
		if stream {
			writeAnthropicStream(w)
		} else {
			writeAnthropicJSON(w)
		}
	}
}

func bedrockInvoke(cfg Config) http.HandlerFunc {
	signer := v4.NewSigner()
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.AWSSecretKey == "" {
			http.Error(w, `{"message":"server not configured for bedrock"}`, http.StatusInternalServerError)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if err := verifySigV4(r, signer, cfg, body); err != nil {
			cfg.Logger.Printf("bedrock: %v", err)
			http.Error(w, fmt.Sprintf(`{"message":"signature verification failed: %s"}`, err), http.StatusForbidden)
			return
		}
		cfg.Logger.Printf("bedrock: ok path=%s", r.URL.Path)
		// invoke-with-response-stream uses the AWS event-stream binary
		// protocol; encode our reply as a single payload event.
		if strings.HasSuffix(r.URL.Path, "/invoke-with-response-stream") {
			writeBedrockEventStream(w)
		} else {
			writeBedrockJSON(w)
		}
	}
}

func verifySigV4(r *http.Request, signer *v4.Signer, cfg Config, body []byte) error {
	auth := r.Header.Get("Authorization")
	dateHdr := r.Header.Get("X-Amz-Date")
	if auth == "" || dateHdr == "" {
		return fmt.Errorf("missing Authorization or X-Amz-Date")
	}
	t, err := time.Parse("20060102T150405Z", dateHdr)
	if err != nil {
		return fmt.Errorf("bad X-Amz-Date %q: %w", dateHdr, err)
	}

	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])

	// Build a request for re-signing whose URL/Host match what the client
	// signed. The proxy preserves Host=bedrock-runtime.<region>.amazonaws.com,
	// so r.Host already has that value even though the TCP conn landed on us.
	signReq := &http.Request{
		Method:        r.Method,
		URL:           &url.URL{Scheme: "https", Host: r.Host, Path: r.URL.Path, RawQuery: r.URL.RawQuery},
		Host:          r.Host,
		Header:        r.Header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	signReq.Header.Del("Authorization")
	signReq.Header.Del("X-Amz-Content-Sha256")

	creds := aws.Credentials{
		AccessKeyID:     cfg.AWSAccessKey,
		SecretAccessKey: cfg.AWSSecretKey,
		SessionToken:    cfg.AWSSessionToken,
	}
	if err := signer.SignHTTP(context.Background(), creds, signReq, hash, "bedrock", cfg.AWSRegion, t); err != nil {
		return fmt.Errorf("re-sign: %w", err)
	}
	if signReq.Header.Get("Authorization") != auth {
		return fmt.Errorf("signature mismatch\nwant: %s\ngot:  %s", signReq.Header.Get("Authorization"), auth)
	}
	return nil
}

func bodyIsStreaming(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Stream
}

func writeAnthropicJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(anthropicReply())
}

func writeAnthropicStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_fake", "type": "message", "role": "assistant",
			"content": []any{}, "model": "claude-fake",
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 0},
		},
	})
	send("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	send("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": FixedReply},
	})
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 10},
	})
	send("message_stop", map[string]any{"type": "message_stop"})
}

func writeBedrockJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(anthropicReply())
}

func anthropicReply() map[string]any {
	return map[string]any{
		"id":            "msg_fake",
		"type":          "message",
		"role":          "assistant",
		"model":         "claude-fake",
		"content":       []any{map[string]any{"type": "text", "text": FixedReply}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 1, "output_tokens": 10},
	}
}
