package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const maxAnthropicTraceEvents = 4000

var anthropicTraceControlPath = os.Getenv("SUB2API_ANTHROPIC_SSE_TRACE_FILE")

type anthropicSSETraceControl struct {
	KeyID     int64     `json:"api_key_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Only the scanner goroutine accesses mutable event and counter state.
type anthropicSSETrace struct {
	log       *zap.Logger
	started   time.Time
	lastEvent time.Time
	pending   anthropicSSETraceEvent
	events    int
	thinking  int
	text      int
	pings     int
	terminal  bool
}

type anthropicSSETraceEvent struct {
	kind      string
	block     string
	delta     string
	size      int
	stop      string
	hasOutput bool
	output    int64
}

func startAnthropicSSETrace(c *gin.Context, account *Account, started time.Time, status int) *anthropicSSETrace {
	if anthropicTraceControlPath == "" || c == nil || c.Request == nil || c.Request.URL.Path != "/v1/messages" ||
		account == nil || account.Platform != PlatformAnthropic {
		return nil
	}
	keyID := getAPIKeyIDFromContext(c)
	if keyID <= 0 {
		return nil
	}
	data, err := os.ReadFile(anthropicTraceControlPath)
	if err != nil {
		return nil
	}
	var control anthropicSSETraceControl
	if json.Unmarshal(data, &control) != nil || control.KeyID != keyID || !control.ExpiresAt.After(time.Now()) {
		return nil
	}
	ctx := c.Request.Context()
	requestID, _ := ctx.Value(ctxkey.RequestID).(string)
	clientID, _ := ctx.Value(ctxkey.ClientRequestID).(string)
	trace := &anthropicSSETrace{
		log: logger.L().With(zap.Int64("api_key_id", keyID), zap.Int64("account_id", account.ID),
			zap.String("request_id", requestID), zap.String("client_request_id", clientID)),
		started:   started,
		lastEvent: time.Now(),
	}
	trace.log.Info("anthropic_sse_trace.started", zap.Int("upstream_status", status), zap.Int64("elapsed_ms", time.Since(started).Milliseconds()))
	return trace
}

func (t *anthropicSSETrace) observeLine(line string) {
	line = strings.TrimSpace(line)
	switch {
	case line == "":
		t.emit()
	case strings.HasPrefix(line, "event:"):
		t.pending.kind = traceAnthropicEventKind(strings.TrimSpace(strings.TrimPrefix(line, "event:")))
	case strings.HasPrefix(line, "data:"):
		t.pending.observeData(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	case strings.HasPrefix(line, ":") && t.pending.kind == "":
		t.pending.kind = "comment"
	}
}

func traceAnthropicEventKind(value string) string {
	switch value {
	case "message_start", "message_delta", "message_stop", "content_block_start",
		"content_block_delta", "content_block_stop", "ping", "error":
		return value
	default:
		return "other"
	}
}

func (e *anthropicSSETraceEvent) observeData(data string) {
	if data == "[DONE]" {
		e.kind = "done"
		return
	}
	event := gjson.Parse(data)
	if kind := event.Get("type").String(); kind != "" {
		e.kind = traceAnthropicEventKind(kind)
	} else if e.kind == "" {
		e.kind = "other"
	}
	switch block := event.Get("content_block.type").String(); block {
	case "text", "thinking", "redacted_thinking", "tool_use", "server_tool_use", "web_search_tool_result":
		e.block = block
	}
	switch delta := event.Get("delta.type").String(); delta {
	case "text_delta":
		e.delta, e.size = delta, len(event.Get("delta.text").String())
	case "thinking_delta":
		e.delta, e.size = delta, len(event.Get("delta.thinking").String())
	case "input_json_delta":
		e.delta, e.size = delta, len(event.Get("delta.partial_json").String())
	case "signature_delta", "citations_delta":
		e.delta = delta
	}
	switch stop := event.Get("delta.stop_reason").String(); stop {
	case "end_turn", "max_tokens", "tool_use", "stop_sequence", "pause_turn", "refusal":
		e.stop = stop
	}
	if out := event.Get("usage.output_tokens"); out.Type == gjson.Number {
		e.hasOutput, e.output = true, out.Int()
	}
}

func (t *anthropicSSETrace) emit() {
	event := t.pending
	t.pending = anthropicSSETraceEvent{}
	if event.kind == "" {
		return
	}
	now := time.Now()
	gap := now.Sub(t.lastEvent)
	t.lastEvent = now
	t.events++
	if event.delta == "thinking_delta" || event.block == "thinking" || event.block == "redacted_thinking" {
		t.thinking++
	}
	if event.delta == "text_delta" && event.size > 0 {
		t.text++
	}
	if event.kind == "ping" || event.kind == "comment" {
		t.pings++
	}
	terminal := event.kind == "message_stop" || event.kind == "done"
	t.terminal = t.terminal || terminal
	if t.events > maxAnthropicTraceEvents && !terminal && event.kind != "error" {
		return
	}
	fields := []zap.Field{
		zap.Int("seq", t.events), zap.Int64("elapsed_ms", now.Sub(t.started).Milliseconds()),
		zap.Int64("gap_ms", gap.Milliseconds()), zap.String("event", event.kind),
		zap.String("block_type", event.block), zap.String("delta_type", event.delta),
		zap.Int("delta_bytes", event.size), zap.String("stop_reason", event.stop),
	}
	if event.hasOutput {
		fields = append(fields, zap.Int64("output_tokens", event.output))
	}
	t.log.Info("anthropic_sse_trace.event", fields...)
}

func (t *anthropicSSETrace) finishScan(err error) {
	t.log.Info("anthropic_sse_trace.upstream_end",
		zap.String("outcome", anthropicSSETraceOutcome(err)), zap.Bool("terminal_seen", t.terminal),
		zap.Int("events", t.events), zap.Int("thinking_events", t.thinking), zap.Int("text_events", t.text),
		zap.Int("keepalive_events", t.pings), zap.Bool("event_limit_reached", t.events > maxAnthropicTraceEvents),
		zap.Int64("elapsed_ms", time.Since(t.started).Milliseconds()), zap.Int64("since_last_event_ms", time.Since(t.lastEvent).Milliseconds()))
}

func (t *anthropicSSETrace) finishGateway(err error) {
	t.log.Info("anthropic_sse_trace.gateway_end", zap.String("outcome", anthropicSSETraceOutcome(err)),
		zap.Int64("elapsed_ms", time.Since(t.started).Milliseconds()))
}

func anthropicSSETraceOutcome(err error) string {
	switch {
	case err == nil:
		return "completed"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case strings.Contains(err.Error(), "missing terminal event"):
		return "missing_terminal"
	case strings.Contains(err.Error(), "stream data interval timeout"):
		return "stream_idle_timeout"
	default:
		return "stream_error"
	}
}
