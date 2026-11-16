// Package egresscapture records upstream exchanges for one explicitly
// enabled API key. It never sends body captures through application logging.
package egresscapture

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const (
	maxBodyBytes = 16 << 20
	maxInFlight  = 4
)

type contextKey struct{}
type attemptKey struct{}

type recorder struct {
	keyID int64
	db    *sql.DB
	slots chan struct{}
	done  chan *Attempt
}

var active atomic.Pointer[recorder]

// Configure leaves capture disabled unless the deployment names a positive key ID.
func Configure(db *sql.DB) {
	keyID, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("SUB2API_EGRESS_CAPTURE_API_KEY_ID")), 10, 64)
	if err != nil || keyID <= 0 || db == nil {
		return
	}
	r := &recorder{keyID: keyID, db: db, slots: make(chan struct{}, maxInFlight), done: make(chan *Attempt, maxInFlight)}
	if active.CompareAndSwap(nil, r) {
		go r.run()
		logger.L().Info("egress_payload_capture_enabled", zap.Int64("api_key_id", keyID))
	}
}

// WithVerifiedKey must only run after normal API-key authentication succeeds.
func WithVerifiedKey(req *http.Request, keyID int64) *http.Request {
	r := active.Load()
	if r == nil || r.keyID != keyID {
		return req
	}
	return req.WithContext(context.WithValue(req.Context(), contextKey{}, r))
}

type collected struct {
	mu       sync.Mutex
	body     []byte
	total    int64
	complete bool
}

func (b *collected) add(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += int64(len(p))
	if b.total <= maxBodyBytes {
		b.body = append(b.body, p...)
	} else {
		b.body = nil
	}
}

func (b *collected) markComplete() {
	b.mu.Lock()
	b.complete = true
	b.mu.Unlock()
}

func (b *collected) snapshot(expected int64) ([]byte, int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.body, b.total, b.complete || (expected >= 0 && b.total == expected)
}

type captureBody struct {
	io.ReadCloser
	tap  *collected
	done func()
}

func (b *captureBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.tap.add(p[:n])
	}
	if errors.Is(err, io.EOF) {
		b.tap.markComplete()
	}
	if err != nil && b.done != nil {
		b.done()
	}
	return n, err
}

func (b *captureBody) Close() error {
	err := b.ReadCloser.Close()
	if b.done != nil {
		b.done()
	}
	return err
}

type Attempt struct {
	owner          *recorder
	once           sync.Once
	started        time.Time
	accountID      int64
	gatewayID      string
	clientID       string
	endpoint       string
	method         string
	request        collected
	requestHeader  http.Header
	requestSize    int64
	response       collected
	responseHeader http.Header
	responseSize   int64
	status         int
	failure        string
}

func begin(ctx context.Context, accountID int64, method string, target *url.URL, headers http.Header) *Attempt {
	if target == nil || accountID <= 0 {
		return nil
	}
	r, ok := ctx.Value(contextKey{}).(*recorder)
	if !ok {
		return nil
	}
	gatewayID, _ := ctx.Value(ctxkey.RequestID).(string)
	clientID, _ := ctx.Value(ctxkey.ClientRequestID).(string)
	if gatewayID == "" && clientID == "" {
		return nil
	}
	select {
	case r.slots <- struct{}{}:
	default:
		return nil
	}
	a := &Attempt{
		owner: r, started: time.Now(), accountID: accountID, gatewayID: gatewayID, clientID: clientID,
		method: method, requestSize: -1, responseSize: -1,
		requestHeader: headers.Clone(),
	}
	a.endpoint = target.String()
	return a
}

// StartHTTP takes a snapshot of the request handed to the upstream transport.
// Bodies without GetBody are observed as the transport reads them.
func StartHTTP(req *http.Request, accountID int64) (*http.Request, *Attempt) {
	if req == nil {
		return req, nil
	}
	if traced, _ := req.Context().Value(attemptKey{}).(bool); traced {
		return req, nil
	}
	a := begin(req.Context(), accountID, req.Method, req.URL, req.Header)
	if a == nil {
		return req, nil
	}
	req = req.WithContext(context.WithValue(req.Context(), attemptKey{}, true))
	a.requestSize = req.ContentLength
	if req.Body == nil || req.Body == http.NoBody {
		a.request.markComplete()
		return req, a
	}
	if req.GetBody != nil {
		if body, err := req.GetBody(); err == nil {
			data, readErr := io.ReadAll(io.LimitReader(body, maxBodyBytes+1))
			_ = body.Close()
			a.request.add(data)
			if readErr == nil && len(data) <= maxBodyBytes {
				a.request.markComplete()
			}
			return req, a
		}
	}
	cloned := req.Clone(req.Context())
	cloned.Body = &captureBody{ReadCloser: req.Body, tap: &a.request}
	return cloned, a
}

// AttachHTTP observes the decoded body the gateway receives without changing
// bytes, timing, cancellation, or whether the caller drains the stream.
func (a *Attempt) AttachHTTP(resp *http.Response, err error) {
	if a == nil {
		return
	}
	if err != nil {
		a.failure = fmt.Sprintf("%T", err)
	}
	if resp == nil {
		a.finish()
		return
	}
	a.status = resp.StatusCode
	a.responseHeader = resp.Header.Clone()
	a.responseSize = resp.ContentLength
	if resp.Body == nil || resp.Body == http.NoBody {
		a.response.markComplete()
		a.finish()
		return
	}
	resp.Body = &captureBody{ReadCloser: resp.Body, tap: &a.response, done: a.finish}
}

func (a *Attempt) finish() {
	a.once.Do(func() {
		a.owner.done <- a
	})
}

func (r *recorder) run() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case a := <-r.done:
			r.save(a)
			<-r.slots
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = r.db.ExecContext(ctx, "DELETE FROM grafana_sub2api.egress_payloads WHERE expires_at <= now()")
			cancel()
		}
	}
}

func (r *recorder) save(a *Attempt) {
	req, requSize, reqComplete := a.request.snapshot(a.requestSize)
	res, respSize, respComplete := a.response.snapshot(a.responseSize)
	request := describeBody(req, requSize, reqComplete, a.requestHeader)
	response := describeBody(res, respSize, respComplete, a.responseHeader)
	requestJSON, reqErr := json.Marshal(request)
	responseJSON, respErr := json.Marshal(response)
	if reqErr != nil || respErr != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.db.ExecContext(ctx, `INSERT INTO grafana_sub2api.egress_payloads
		(created_at, gateway_id, client_id, api_key_id, account_id, method, url, status_code, failure,
		 request_json, response_json)
		VALUES ($1, NULLIF($2,''), NULLIF($3,''), $4, $5, $6, $7, NULLIF($8,0), NULLIF($9,''),
		        $10::jsonb, $11::jsonb)`,
		a.started, a.gatewayID, a.clientID, r.keyID, a.accountID, a.method, a.endpoint, a.status, a.failure,
		string(requestJSON), string(responseJSON))
	if err != nil {
		// The error text or statement parameters may contain payload fragments.
		logger.L().Warn("egress_payload_capture_persist_failed")
	}
}
