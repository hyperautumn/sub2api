package egresscapture

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

func TestHTTPOnlyCapturesVerifiedTestKeyAndPreservesTransport(t *testing.T) {
	r := &recorder{keyID: 7, slots: make(chan struct{}, 4), done: make(chan *Attempt, 4)}
	previous := active.Swap(r)
	t.Cleanup(func() { active.Store(previous) })

	const requestBody = `{"input":"keep this","nested":{"clientSecret":"example-unit-secret"},"max_output_tokens":25}`
	const responseBody = `{"reply":"echo: example-unit-secret","authorization":"provider-real-secret","usage":{"output_tokens":2}}`
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "gateway-request")
	req, err := http.NewRequestWithContext(ctx, "POST", "https://upstream.invalid/v1/responses?api_key=query-secret", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer provider-real-secret")
	req.Header.Set("Content-Type", "application/json")
	if tagged := WithVerifiedKey(req, 9); tagged != req {
		t.Fatal("regular request was tagged")
	}
	if forwarded, attempt := StartHTTP(req, 3); forwarded != req || attempt != nil {
		t.Fatal("unverified request was captured")
	}
	forwarded, attempt := StartHTTP(WithVerifiedKey(req, 7), 3)
	if attempt == nil {
		t.Fatal("verified test request was not captured")
	}
	raw, _ := io.ReadAll(forwarded.Body)
	if string(raw) != requestBody || forwarded.Header.Get("Authorization") != "Bearer provider-real-secret" {
		t.Fatal("forwarding payload changed")
	}
	response := httptest.NewRecorder()
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Set-Cookie", "sid=private-cookie-secret")
	_, _ = response.WriteString(responseBody)
	httpResp := response.Result()
	attempt.AttachHTTP(httpResp, nil)
	body, _ := io.ReadAll(httpResp.Body)
	_ = httpResp.Body.Close()
	if string(body) != responseBody {
		t.Fatal("stream read changed")
	}

	saved := <-r.done
	reqBytes, reqSize, reqComplete := saved.request.snapshot(saved.requestSize)
	resBytes, resSize, resComplete := saved.response.snapshot(saved.responseSize)
	details, err := json.Marshal([]any{saved.endpoint,
		describeBody(reqBytes, reqSize, reqComplete, saved.requestHeader),
		describeBody(resBytes, resSize, resComplete, saved.responseHeader)})
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{"example-unit-secret", "Bearer provider-real-secret", "private-cookie-secret", "query-secret",
		"keep this", "output_tokens", "Authorization", `"complete":true`} {
		if !strings.Contains(string(details), wanted) {
			t.Fatalf("capture missing expected value %q", wanted)
		}
	}
}

func TestStreamRetainsCredentialShapedContentAcrossEvents(t *testing.T) {
	sse := "event: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"sk-unit-abc\"}\r\n\r\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"def0123456789\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output\":\"ordinary response\"}}\n\n"
	events := describeSSE([]byte(sse))
	if len(events) != 3 {
		t.Fatalf("got %d events", len(events))
	}
	raw, _ := json.Marshal(events)
	for _, value := range []string{"sk-unit-abc", "def0123456789", "ordinary response"} {
		if !strings.Contains(string(raw), value) {
			t.Fatal("stream event value was lost")
		}
	}
}

func TestPartialJSONIsPreservedAsText(t *testing.T) {
	raw := []byte(`{"password":"not-complete`)
	value := describeBody(raw, int64(len(raw)), false, http.Header{"Content-Type": {"application/json"}})
	if value["body"] != string(raw) || value["complete"] != false {
		t.Fatal("partial JSON was lost or incorrectly marked complete")
	}
}

func TestSSEWithNoCanonicalContentTypeDisplaysEventsAndTerminalStatus(t *testing.T) {
	raw := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n" +
		"data: {\"type\":\"response.completed\"}\n\n")
	value := describeBody(raw, int64(len(raw)), false, nil)
	events, valid := value["body"].([]map[string]any)
	if value["format"] != "sse" || !valid || len(events) != 2 || value["wire_eof"] != false || value["complete"] != true {
		t.Fatalf("SSE was not decoded as a complete logical response: %#v", value)
	}
}
