package fakeapi

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
)

// writeBedrockEventStream emits an AWS event-stream containing a single
// "chunk" message that wraps a base64-encoded Anthropic streaming
// payload. This is the minimum claude needs to see one assistant turn
// over Bedrock's invoke-with-response-stream.
//
// Wire format per AWS docs:
//
//	[total_len:4][headers_len:4][prelude_crc:4][headers][payload][message_crc:4]
//
// Each header is [name_len:1][name][value_type:1][value]. We use type 7
// (UTF-8 string) where the value is prefixed with a 2-byte length.
func writeBedrockEventStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	flusher, _ := w.(http.Flusher)

	writeMessage := func(eventType string, payload []byte) {
		var headers bytes.Buffer
		appendHeader(&headers, ":event-type", eventType)
		appendHeader(&headers, ":content-type", "application/json")
		appendHeader(&headers, ":message-type", "event")
		hdrBytes := headers.Bytes()

		totalLen := uint32(4 + 4 + 4 + len(hdrBytes) + len(payload) + 4)
		hdrLen := uint32(len(hdrBytes))

		var prelude [8]byte
		binary.BigEndian.PutUint32(prelude[0:4], totalLen)
		binary.BigEndian.PutUint32(prelude[4:8], hdrLen)
		preludeCRC := crc32.ChecksumIEEE(prelude[:])

		var msg bytes.Buffer
		msg.Write(prelude[:])
		var c [4]byte
		binary.BigEndian.PutUint32(c[:], preludeCRC)
		msg.Write(c[:])
		msg.Write(hdrBytes)
		msg.Write(payload)

		msgCRC := crc32.ChecksumIEEE(msg.Bytes())
		binary.BigEndian.PutUint32(c[:], msgCRC)
		msg.Write(c[:])

		_, _ = w.Write(msg.Bytes())
		if flusher != nil {
			flusher.Flush()
		}
	}

	send := func(eventType string, inner map[string]any) {
		innerJSON, _ := json.Marshal(inner)
		payload, _ := json.Marshal(map[string]any{
			"bytes": base64.StdEncoding.EncodeToString(innerJSON),
		})
		writeMessage(eventType, payload)
	}

	send("chunk", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_fake", "type": "message", "role": "assistant",
			"content": []any{}, "model": "claude-fake",
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 0},
		},
	})
	send("chunk", map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	send("chunk", map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": FixedReply},
	})
	send("chunk", map[string]any{"type": "content_block_stop", "index": 0})
	send("chunk", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 10},
	})
	send("chunk", map[string]any{"type": "message_stop"})
}

func appendHeader(buf *bytes.Buffer, name, value string) {
	if len(name) > 255 {
		panic(fmt.Sprintf("header name too long: %q", name))
	}
	buf.WriteByte(byte(len(name)))
	buf.WriteString(name)
	buf.WriteByte(7) // type 7 = UTF-8 string
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(value)))
	buf.Write(l[:])
	buf.WriteString(value)
}
