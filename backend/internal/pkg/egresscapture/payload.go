package egresscapture

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"
)

func decodeJSON(raw []byte) (any, bool) {
	// PostgreSQL JSONB cannot represent a decoded NUL. Fall back to preserving
	// the body as text when it contains an escaped NUL.
	if bytes.Contains(raw, []byte(`\u0000`)) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, false
	}
	var extra any
	return value, dec.Decode(&extra) == io.EOF
}

func plainBody(data []byte) (string, any) {
	if utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
		return "text", string(data)
	}
	return "base64", data
}

func describeBody(data []byte, total int64, complete bool, headers http.Header) map[string]any {
	result := map[string]any{"headers": headers, "bytes": total, "complete": complete}
	if total > maxBodyBytes {
		result["complete"], result["format"] = false, "size_limit"
		return result
	}
	if len(data) == 0 {
		result["format"], result["body"] = "empty", nil
		return result
	}
	contentType, _, _ := mime.ParseMediaType(headerValue(headers, "Content-Type"))
	switch {
	case contentType == frameMediaType:
		result["format"], result["body"] = "websocket_frames", describeFrames(data)
	case contentType == "text/event-stream" || looksLikeEventStream(bytes.TrimSpace(data)):
		if !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
			result["format"], result["body"] = plainBody(data)
			break
		}
		events := describeSSE(data)
		result["format"], result["body"] = "sse", events
		result["wire_eof"] = complete
		result["complete"] = complete || hasTerminalEvent(events)
	default:
		if value, ok := decodeJSON(data); ok {
			result["format"], result["body"] = "json", value
		} else {
			result["format"], result["body"] = plainBody(data)
		}
	}
	return result
}

func looksLikeEventStream(data []byte) bool {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if bytes.HasPrefix(data, []byte("event:")) || bytes.HasPrefix(data, []byte("data:")) {
		return true
	}
	return bytes.HasPrefix(data, []byte(":")) &&
		(bytes.Contains(data, []byte("\ndata:")) || bytes.Contains(data, []byte("\revent:")))
}

func headerValue(headers http.Header, name string) string {
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func hasTerminalEvent(events []map[string]any) bool {
	for _, event := range events {
		if event["data"] == "[DONE]" {
			return true
		}
		typ, _ := event["event"].(string)
		if payload, ok := event["data"].(map[string]any); ok && typ == "" {
			typ, _ = payload["type"].(string)
		}
		switch typ {
		case "response.completed", "response.done", "response.failed", "response.incomplete",
			"response.cancelled", "message_stop", "error":
			return true
		}
	}
	return false
}

func describeSSE(data []byte) []map[string]any {
	var events []map[string]any
	event := map[string]any{}
	var parts, comments []string
	flush := func() {
		if len(parts) != 0 {
			text := strings.Join(parts, "\n")
			if value, ok := decodeJSON([]byte(text)); ok {
				event["data"] = value
			} else {
				event["data"] = text
			}
		}
		if len(comments) != 0 {
			event["comments"] = comments
		}
		if len(event) != 0 {
			events = append(events, event)
		}
		event, parts, comments = map[string]any{}, nil, nil
	}
	// SSE accepts CR, LF, or CRLF as line separators.
	text := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	for line := range strings.SplitSeq(text, "\n") {
		if line == "" {
			flush()
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "":
			comments = append(comments, value)
		case "data":
			parts = append(parts, value)
		default:
			event[field] = value
		}
	}
	flush()
	return events
}
