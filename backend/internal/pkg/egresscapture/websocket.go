package egresscapture

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/tidwall/gjson"
)

const frameMediaType = "application/x-sub2api-websocket-frames"

// StartWebSocket records the logical upstream turn after WebSocket negotiation.
// The header status, when known, belongs to the connection handshake.
func StartWebSocket(ctx context.Context, accountID int64, target string, headers, handshake http.Header, request []byte) *Attempt {
	if _, ok := ctx.Value(contextKey{}).(*recorder); !ok {
		return nil
	}
	u, err := url.Parse(target)
	if err != nil {
		return nil
	}
	a := begin(ctx, accountID, "WEBSOCKET", u, headers)
	if a == nil {
		return nil
	}
	if a.requestHeader == nil {
		a.requestHeader = make(http.Header)
	}
	a.requestHeader.Set("Content-Type", "application/json")
	a.responseHeader = handshake.Clone()
	if a.responseHeader == nil {
		a.responseHeader = make(http.Header)
	}
	a.responseHeader.Set("Content-Type", frameMediaType)
	a.status = http.StatusSwitchingProtocols
	a.request.add(request)
	a.request.markComplete()
	return a
}

// CaptureEnabled is a cheap check before serializing a WebSocket request.
func CaptureEnabled(ctx context.Context) bool {
	_, enabled := ctx.Value(contextKey{}).(*recorder)
	return enabled
}

// ReadFrame preserves frame boundaries without reading ahead or re-encoding
// the data seen by either side of the gateway.
func (a *Attempt) ReadFrame(frame []byte) {
	if a == nil {
		return
	}
	if len(frame) > maxBodyBytes {
		a.response.add(frame)
		return
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(frame)))
	a.response.add(size[:])
	a.response.add(frame)
	if IsTerminalFrame(frame) {
		a.response.markComplete()
	}
}

// IsTerminalFrame reports the final event in a Responses turn.
func IsTerminalFrame(frame []byte) bool {
	switch gjson.GetBytes(frame, "type").String() {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "error":
		return true
	default:
		return false
	}
}

// FinishWebSocket releases the payload at the end of a turn, including partial
// turns when the connection fails.
func (a *Attempt) FinishWebSocket(err error) {
	if a == nil {
		return
	}
	a.once.Do(func() {
		if err != nil {
			a.failure = fmt.Sprintf("%T", err)
		}
		a.owner.done <- a
	})
}

func framePayloads(raw []byte) [][]byte {
	var result [][]byte
	for len(raw) >= 4 {
		size := int(binary.BigEndian.Uint32(raw))
		raw = raw[4:]
		if size > len(raw) {
			break
		}
		result = append(result, raw[:size])
		raw = raw[size:]
	}
	return result
}

func describeFrames(raw []byte) []any {
	var result []any
	for _, frame := range framePayloads(raw) {
		if value, ok := decodeJSON(frame); ok {
			result = append(result, value)
		} else {
			format, body := plainBody(frame)
			result = append(result, map[string]any{"format": format, "body": body})
		}
	}
	return result
}

// MarshalWebSocketRequest follows the same JSON marshaling used by the lease.
func MarshalWebSocketRequest(payload any) []byte {
	raw, _ := json.Marshal(payload)
	return raw
}
