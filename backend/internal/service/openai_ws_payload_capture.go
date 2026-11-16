package service

import (
	"context"
	"net/http"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/egresscapture"
	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

// openAIWSPayloadCaptureConn observes the transformed frames on the upstream
// connection. It is only installed for the deployment's authenticated test key.
type openAIWSPayloadCaptureConn struct {
	openaiwsv2.FrameConn
	requestCtx context.Context
	accountID  int64
	url        string
	headers    http.Header
	handshake  http.Header

	mu     sync.Mutex
	active *egresscapture.Attempt
}

func (c *openAIWSPayloadCaptureConn) WriteFrame(ctx context.Context, typ coderws.MessageType, frame []byte) error {
	if typ == coderws.MessageText && gjson.GetBytes(frame, "type").String() == "response.create" {
		c.mu.Lock()
		c.active.FinishWebSocket(nil)
		c.active = egresscapture.StartWebSocket(c.requestCtx, c.accountID, c.url, c.headers, c.handshake, frame)
		c.mu.Unlock()
	}
	err := c.FrameConn.WriteFrame(ctx, typ, frame)
	if err != nil {
		c.finish(err)
	}
	return err
}

func (c *openAIWSPayloadCaptureConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	typ, frame, err := c.FrameConn.ReadFrame(ctx)
	if err != nil {
		// Some reads are canceled to update stream deadlines and are retried
		// without ending the WebSocket turn.
		return typ, frame, err
	}
	c.mu.Lock()
	c.active.ReadFrame(frame)
	if egresscapture.IsTerminalFrame(frame) {
		c.active.FinishWebSocket(nil)
		c.active = nil
	}
	c.mu.Unlock()
	return typ, frame, nil
}

func (c *openAIWSPayloadCaptureConn) finish(err error) {
	c.mu.Lock()
	c.active.FinishWebSocket(err)
	c.active = nil
	c.mu.Unlock()
}
