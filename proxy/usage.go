package proxy

// Token accounting for Bedrock traffic. Because the MITM already terminates
// TLS and streams every Bedrock response through this process, we tee the
// response body past a parser that extracts the Anthropic usage events and
// accumulate per-model token counts, then estimate a dollar cost on exit.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// TokenUsage counts tokens accumulated across one or more responses.
type TokenUsage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// UsageTracker accumulates per-model token usage parsed from Bedrock
// responses and estimates the session cost. It is safe for concurrent use.
type UsageTracker struct {
	mu      sync.Mutex
	byModel map[string]*TokenUsage
	// wg tracks in-flight streaming parsers so Report can wait for responses
	// that are still draining before it sums the session.
	wg     sync.WaitGroup
	logger *log.Logger
}

func NewUsageTracker(logger *log.Logger) *UsageTracker {
	return &UsageTracker{byModel: map[string]*TokenUsage{}, logger: logger}
}

func (t *UsageTracker) add(model string, u TokenUsage) {
	if u == (TokenUsage{}) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	acc := t.byModel[model]
	if acc == nil {
		acc = &TokenUsage{}
		t.byModel[model] = acc
	}
	acc.Input += u.Input
	acc.Output += u.Output
	acc.CacheRead += u.CacheRead
	acc.CacheWrite += u.CacheWrite
}

// Observe hooks a Bedrock response: it identifies the model from the request
// path and replaces resp.Body with a reader that parses token usage as the
// client streams it. Non-Bedrock requests and non-success responses are left
// untouched. It must run before the body is read and never blocks the client.
func (t *UsageTracker) Observe(reqPath string, resp *http.Response) {
	model := modelFromBedrockPath(reqPath)
	if model == "" || resp.StatusCode != http.StatusOK || resp.Body == nil {
		return
	}
	switch ct := resp.Header.Get("Content-Type"); {
	case strings.Contains(ct, "vnd.amazon.eventstream"):
		resp.Body = t.wrapStream(model, resp.Body)
	case strings.Contains(ct, "application/json"):
		resp.Body = t.wrapJSON(model, resp.Body)
	}
}

func (t *UsageTracker) wrapStream(model string, body io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.add(model, parseEventStreamUsage(bufio.NewReader(pr), t.logger))
		// Drain anything the parser stopped short of so the tee's writes
		// never block after the usage events have been seen.
		io.Copy(io.Discard, pr)
	}()
	return &teeReadCloser{body: body, pw: pw}
}

func (t *UsageTracker) wrapJSON(model string, body io.ReadCloser) io.ReadCloser {
	return &jsonUsageReader{model: model, tracker: t, body: body}
}

// teeReadCloser forwards every byte read from body into pw so a parser
// goroutine reading the other end of the pipe sees the same stream.
type teeReadCloser struct {
	body io.ReadCloser
	pw   *io.PipeWriter
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.body.Read(p)
	if n > 0 {
		t.pw.Write(p[:n])
	}
	if err != nil {
		t.pw.CloseWithError(err)
	}
	return n, err
}

func (t *teeReadCloser) Close() error {
	t.pw.Close()
	return t.body.Close()
}

// jsonUsageReader buffers a non-streaming JSON response and parses its usage
// block once the body is fully read.
type jsonUsageReader struct {
	model   string
	tracker *UsageTracker
	body    io.ReadCloser
	buf     bytes.Buffer
	done    bool
}

func (r *jsonUsageReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.buf.Write(p[:n])
	}
	if err == io.EOF {
		r.flush()
	}
	return n, err
}

func (r *jsonUsageReader) Close() error {
	r.flush()
	return r.body.Close()
}

func (r *jsonUsageReader) flush() {
	if r.done {
		return
	}
	r.done = true
	r.tracker.add(r.model, parseJSONUsage(r.buf.Bytes()))
}

// usageObject mirrors the usage block in a model response, carrying both the
// Anthropic field names and the OpenAI-style names (prompt_tokens etc.) that
// non-Anthropic Bedrock models such as Moonshot's Kimi report.
type usageObject struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`

	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u usageObject) tokens() TokenUsage {
	t := TokenUsage{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheCreationInputTokens,
	}
	// Fall back to OpenAI-style fields when the Anthropic ones are absent.
	// There, prompt_tokens includes cached tokens, so split them out to match
	// Anthropic's convention (input excludes cache) and price them correctly.
	if t.Input == 0 && u.PromptTokens > 0 {
		t.Input = u.PromptTokens - u.PromptTokensDetails.CachedTokens
		t.CacheRead = u.PromptTokensDetails.CachedTokens
	}
	if t.Output == 0 {
		t.Output = u.CompletionTokens
	}
	return t
}

// openAIStyle reports whether this usage block uses OpenAI field names, i.e.
// carries a complete per-response snapshot rather than Anthropic's split of
// input (message_start) and cumulative output (message_delta).
func (u usageObject) openAIStyle() bool {
	return u.PromptTokens > 0 || u.CompletionTokens > 0
}

func parseJSONUsage(body []byte) TokenUsage {
	var msg struct {
		Usage usageObject `json:"usage"`
	}
	if json.Unmarshal(body, &msg) != nil {
		return TokenUsage{}
	}
	return msg.Usage.tokens()
}

// parseEventStreamUsage reads an AWS event-stream and returns its usage. For
// Anthropic events, input and cache counts come from message_start and output
// from the final (cumulative) message_delta. For OpenAI-style events (Kimi),
// a single usage block carries the complete per-response snapshot, which wins.
func parseEventStreamUsage(r io.Reader, logger *log.Logger) TokenUsage {
	var u TokenUsage
	var lastOutput int64
	var snapshot TokenUsage
	var haveSnapshot bool
	for {
		payload, err := readEventStreamFrame(r)
		if err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				logger.Printf("usage: event-stream: %v", err)
			}
			break
		}
		var env struct {
			Bytes []byte `json:"bytes"`
		}
		if json.Unmarshal(payload, &env) != nil || len(env.Bytes) == 0 {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage usageObject `json:"usage"`
			} `json:"message"`
			Usage usageObject `json:"usage"`
		}
		if json.Unmarshal(env.Bytes, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			u.Input += ev.Message.Usage.InputTokens
			u.CacheRead += ev.Message.Usage.CacheReadInputTokens
			u.CacheWrite += ev.Message.Usage.CacheCreationInputTokens
		case "message_delta":
			lastOutput = ev.Usage.OutputTokens
		default:
			if ev.Usage.openAIStyle() {
				snapshot = ev.Usage.tokens()
				haveSnapshot = true
			}
		}
	}
	if haveSnapshot {
		return snapshot
	}
	u.Output += lastOutput
	return u
}

// readEventStreamFrame reads one AWS event-stream message and returns its
// payload (headers and CRCs discarded). The wire format is
// [total_len:4][headers_len:4][prelude_crc:4][headers][payload][msg_crc:4],
// all big-endian; see fakeapi/eventstream.go for the matching encoder.
func readEventStreamFrame(r io.Reader) ([]byte, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(r, prelude[:]); err != nil {
		return nil, err
	}
	totalLen := binary.BigEndian.Uint32(prelude[0:4])
	hdrLen := binary.BigEndian.Uint32(prelude[4:8])
	if totalLen < 16 || uint64(hdrLen) > uint64(totalLen)-16 {
		return nil, fmt.Errorf("bad frame lengths total=%d headers=%d", totalLen, hdrLen)
	}
	rest := make([]byte, totalLen-12)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, err
	}
	return rest[hdrLen : len(rest)-4], nil
}

// modelFromBedrockPath extracts the (URL-decoded) model id from a Bedrock
// invoke path like /model/<id>/invoke[-with-response-stream], or "" if path
// is not a Bedrock invoke path.
func modelFromBedrockPath(path string) string {
	const prefix = "/model/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := path[len(prefix):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if m, err := url.PathUnescape(rest); err == nil {
		return m
	}
	return rest
}

// modelPrice is the USD price per million tokens for base input, output,
// 5-minute cache writes, and cache reads. Cache ratios vary by model (e.g.
// cache reads are 0.05x input on the 5.5 generation but 0.1x elsewhere), so
// every field is listed explicitly rather than derived.
type modelPrice struct {
	input      float64
	output     float64
	cacheWrite float64
	cacheRead  float64
}

// bedrockPrices maps a model-family substring found in a Bedrock model id
// (e.g. "claude-opus-5-5" in "us.anthropic.claude-opus-5-5-20260401-v1:0") to
// its price. The first matching entry wins, so order from most to least
// specific. Figures are Anthropic's published per-MTok list rates (claude.com
// /pricing, 2026-10), which Bedrock on-demand matches for these models;
// adjust here if AWS's regional rates drift.
var bedrockPrices = []struct {
	match string
	price modelPrice
}{
	{"claude-opus-5-5", modelPrice{4, 20, 5, 0.20}},
	{"claude-opus-5", modelPrice{5, 25, 6.25, 0.50}},
	{"claude-opus-4-8", modelPrice{5, 25, 6.25, 0.50}},
	{"claude-opus-4-7", modelPrice{5, 25, 6.25, 0.50}},
	{"claude-opus-4-6", modelPrice{5, 25, 6.25, 0.50}},
	{"claude-opus-4-5", modelPrice{5, 25, 6.25, 0.50}},
	{"claude-opus-4-1", modelPrice{15, 75, 18.75, 1.50}},
	{"claude-opus-4", modelPrice{15, 75, 18.75, 1.50}},
	{"claude-sonnet-5-5", modelPrice{2, 10, 2.50, 0.10}},
	{"claude-sonnet-5", modelPrice{2, 10, 2.50, 0.20}},
	{"claude-sonnet-4", modelPrice{3, 15, 3.75, 0.30}},
	{"claude-haiku-4", modelPrice{1, 5, 1.25, 0.10}},
	{"claude-3-7-sonnet", modelPrice{3, 15, 3.75, 0.30}},
	{"claude-3-5-sonnet", modelPrice{3, 15, 3.75, 0.30}},
	{"claude-3-5-haiku", modelPrice{0.8, 4, 1.0, 0.08}},
	{"claude-3-opus", modelPrice{15, 75, 18.75, 1.50}},
	{"claude-3-haiku", modelPrice{0.25, 1.25, 0.30, 0.03}},
	{"claude-3-sonnet", modelPrice{3, 15, 3.75, 0.30}},
	// Moonshot Kimi (Bedrock ids like "moonshotai.kimi-k2-..."). Moonshot's
	// published K2 rates; turbo variants cost more, so adjust if used.
	{"kimi-k2", modelPrice{0.60, 2.50, 0.60, 0.15}},
	{"kimi", modelPrice{0.60, 2.50, 0.60, 0.15}},
}

func priceFor(model string) (modelPrice, bool) {
	for _, e := range bedrockPrices {
		if strings.Contains(model, e.match) {
			return e.price, true
		}
	}
	return modelPrice{}, false
}

// costs splits the estimated USD cost of u into base-input, output, and
// cache (read + write) components.
func (p modelPrice) costs(u TokenUsage) (input, output, cache float64) {
	input = float64(u.Input) / 1e6 * p.input
	output = float64(u.Output) / 1e6 * p.output
	cache = float64(u.CacheRead)/1e6*p.cacheRead + float64(u.CacheWrite)/1e6*p.cacheWrite
	return
}

// Report returns a human-readable per-model usage and estimated-cost summary,
// or "" if no usage was recorded.
func (t *UsageTracker) Report() string {
	// Wait outside the lock: parser goroutines call add, which takes it.
	t.wg.Wait()
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.byModel) == 0 {
		return ""
	}
	models := make([]string, 0, len(t.byModel))
	for m := range t.byModel {
		models = append(models, m)
	}
	sort.Strings(models)

	var b strings.Builder
	var totalIn, totalOut, totalCache float64
	allKnown := true
	b.WriteString("Bedrock usage this session (estimated cost):\n")
	for _, m := range models {
		u := t.byModel[m]
		price, ok := priceFor(m)
		if !ok {
			allKnown = false
			fmt.Fprintf(&b, "  %s  (pricing unknown)\n", m)
			continue
		}
		in, out, cache := price.costs(*u)
		totalIn, totalOut, totalCache = totalIn+in, totalOut+out, totalCache+cache
		// Hide trivial models to keep the summary short; they still count
		// toward the total below.
		if c := in + out + cache; c >= 0.01 {
			fmt.Fprintf(&b, "  %s  $%.4f\n", m, c)
		}
	}
	fmt.Fprintf(&b, "Total: $%.4f (input $%.4f, output $%.4f, cached $%.4f)",
		totalIn+totalOut+totalCache, totalIn, totalOut, totalCache)
	if !allKnown {
		b.WriteString(" (excludes models with unknown pricing)")
	}
	b.WriteString("\n")
	return b.String()
}
