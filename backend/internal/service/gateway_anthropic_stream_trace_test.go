package service

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestAnthropicSSETraceLogsEventMetadataOnly(t *testing.T) {
	core, entries := observer.New(zap.InfoLevel)
	now := time.Now()
	trace := anthropicSSETrace{log: zap.New(core), started: now, lastEvent: now}
	for _, line := range []string{
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"private thinking"}}`,
		"",
		"event: ping",
		`data: {"type":"ping"}`,
		"",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"private answer"}}`,
		"",
		`data: {"type":"message_stop"}`,
		"",
	} {
		trace.observeLine(line)
	}
	trace.finishScan(context.Canceled)

	logs := entries.All()
	require.Len(t, logs, 5)
	require.Equal(t, "thinking_delta", logs[0].ContextMap()["delta_type"])
	require.EqualValues(t, len("private thinking"), logs[0].ContextMap()["delta_bytes"])
	require.Equal(t, "ping", logs[1].ContextMap()["event"])
	require.Equal(t, "text_delta", logs[2].ContextMap()["delta_type"])
	require.Equal(t, "message_stop", logs[3].ContextMap()["event"])
	for _, entry := range logs {
		for _, value := range entry.ContextMap() {
			if str, ok := value.(string); ok {
				require.NotContains(t, str, "private")
			}
		}
	}
	require.Equal(t, "context_canceled", logs[4].ContextMap()["outcome"])
	require.Equal(t, true, logs[4].ContextMap()["terminal_seen"])
}

func TestAnthropicSSETraceRequiresMatchingAuthenticatedKeyAndActiveWindow(t *testing.T) {
	old := anthropicTraceControlPath
	anthropicTraceControlPath = filepath.Join(t.TempDir(), "control.json")
	t.Cleanup(func() { anthropicTraceControlPath = old })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	account := &Account{ID: 2, Platform: PlatformAnthropic}
	active := []byte(`{"api_key_id":3,"expires_at":"2099-01-01T00:00:00Z"}`)
	require.NoError(t, os.WriteFile(anthropicTraceControlPath, active, 0o600))
	require.Nil(t, startAnthropicSSETrace(c, account, time.Now(), 200))
	c.Set("api_key", &APIKey{ID: 1})
	require.Nil(t, startAnthropicSSETrace(c, account, time.Now(), 200))
	c.Set("api_key", &APIKey{ID: 3})
	require.NotNil(t, startAnthropicSSETrace(c, account, time.Now(), 200))

	expired := []byte(`{"api_key_id":3,"expires_at":"2020-01-01T00:00:00Z"}`)
	require.NoError(t, os.WriteFile(anthropicTraceControlPath, expired, 0o600))
	require.Nil(t, startAnthropicSSETrace(c, account, time.Now(), 200))
}
