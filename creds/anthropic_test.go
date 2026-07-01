package creds

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func gz(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestLogBody(t *testing.T) {
	const msg = `{"error":{"type":"authentication_error"}}`

	tests := []struct {
		name     string
		body     []byte
		encoding string
		want     string
	}{
		{"plain", []byte(msg), "", msg},
		{"gzip via header", gz(msg), "gzip", msg},
		{"gzip via magic", gz(msg), "", msg}, // no Content-Encoding, detected by magic
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := logBody(tc.body, tc.encoding); got != tc.want {
				t.Errorf("logBody = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLogBodyBinaryIsEscaped(t *testing.T) {
	// A body that claims gzip but isn't decodable, or raw binary, must never
	// reach the log verbatim — control bytes get quoted/escaped.
	got := logBody([]byte{0x1f, 0x8b, 0x00, 0x01, 0x02}, "gzip")
	if strings.ContainsRune(got, 0x00) {
		t.Fatalf("binary body should be escaped, got %q", got)
	}
}
