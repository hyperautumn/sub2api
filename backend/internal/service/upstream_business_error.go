package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const OpsClientBusinessLimitedReasonUpstreamRejection = "upstream_business_rejection"

// ErrAccountSelectionUpstreamBalance means that every eligible upstream
// account was skipped because the provider reported insufficient balance.
var ErrAccountSelectionUpstreamBalance = errors.New("upstream accounts have insufficient balance")

// UpstreamBusinessError is the caller-facing classification of a provider rejection.
type UpstreamBusinessError struct {
	Status  int
	Type    string
	Code    string
	Message string
}

// ClassifyUpstreamBusinessError reads explicit provider error fields. An
// ordinary upstream 401, 403 or 404 can also be a gateway credential or
// endpoint failure, so those statuses alone do not establish a business error.
func ClassifyUpstreamBusinessError(status int, body []byte) (UpstreamBusinessError, bool) {
	if status < 400 || status >= 500 || status == http.StatusUnauthorized {
		return UpstreamBusinessError{}, false
	}
	if gjson.ValidBytes(body) {
		for _, path := range []string{
			"error.code", "response.error.code", "error.errorKey", "error.error_key",
			"error.cause.errorKey", "error.details.code", "error.details.#.reason",
			"error.type", "response.error.type", "error.status", "code", "errorKey",
		} {
			value := gjson.GetBytes(body, path)
			if value.IsArray() {
				for _, entry := range value.Array() {
					if result, ok := classifyUpstreamBusinessCode(entry); ok {
						return result, true
					}
				}
			} else if result, ok := classifyUpstreamBusinessCode(value); ok {
				return result, true
			}
		}
	}
	if status == http.StatusTooManyRequests {
		return upstreamRateLimitError, true
	}
	return UpstreamBusinessError{}, false
}

var (
	upstreamInsufficientBalanceError = UpstreamBusinessError{
		Status: http.StatusForbidden, Type: "permission_error", Code: "insufficient_balance",
		Message: "Upstream account has insufficient balance",
	}
	upstreamQuotaExceededError = UpstreamBusinessError{
		Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "quota_exceeded",
		Message: "Upstream usage quota exceeded",
	}
	upstreamRateLimitError = UpstreamBusinessError{
		Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "rate_limit_exceeded",
		Message: "Upstream rate limit exceeded, please retry later",
	}
	upstreamModelNotFoundError = UpstreamBusinessError{
		Status: http.StatusNotFound, Type: "not_found_error", Code: "model_not_found",
		Message: "Requested model is unavailable",
	}
	upstreamPermissionError = UpstreamBusinessError{
		Status: http.StatusForbidden, Type: "permission_error", Code: "permission_denied",
		Message: "Request is not permitted by the upstream provider",
	}
)

func classifyUpstreamBusinessCode(value gjson.Result) (UpstreamBusinessError, bool) {
	if value.Type != gjson.String {
		return UpstreamBusinessError{}, false
	}
	code := strings.ToLower(strings.TrimSpace(value.String()))
	switch code {
	case "accountoverdueerror", "insufficient_balance", "insufficient_credit", "insufficient_credits", "insufficient_funds":
		return upstreamInsufficientBalanceError, true
	case "quotaexceeded", "quota_exceeded", "insufficient_quota", "usage_limit_exceeded", "spend_limit_exceeded", "inference_cap_error", "resource_exhausted",
		"subscription:free-usage-exhausted", "free-usage-exhausted", "free_usage_exhausted", "usage-limit-exceeded", "personal-team-blocked:spending-limit":
		return upstreamQuotaExceededError, true
	case "rate_limit_error", "rate_limit_exceeded", "too_many_requests", "requestlimitexceeded", "concurrency_limit_exceeded":
		return upstreamRateLimitError, true
	case "model_not_found", "model_not_available", "model_access_denied", "model_permission_denied", "modelnotfound", "modelnotopen":
		return upstreamModelNotFoundError, true
	case "user_permission_denied", "request_not_authorized", "operation_not_allowed":
		return upstreamPermissionError, true
	}
	if strings.HasPrefix(code, "quotaexceeded.") {
		return upstreamQuotaExceededError, true
	}
	return UpstreamBusinessError{}, false
}

// BusinessError returns the semantic client rejection for an exhausted attempt.
func (e *UpstreamFailoverError) BusinessError() (UpstreamBusinessError, bool) {
	if e == nil || e.IsCredentialFailure() || e.IsOpenAICapacityShed() {
		return UpstreamBusinessError{}, false
	}
	if e.Reason == OpenAIImagesInsufficientBalanceReason {
		result := upstreamInsufficientBalanceError
		result.Message = OpenAIImagesInsufficientBalanceMessage
		return result, true
	}
	return ClassifyUpstreamBusinessError(e.StatusCode, e.ResponseBody)
}

// MarkUpstreamBusinessError records the final result without removing the
// original provider status and request ID from the upstream diagnostics.
func MarkUpstreamBusinessError(c *gin.Context, business UpstreamBusinessError, headers http.Header) {
	MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonUpstreamRejection)
	if c == nil || c.Writer == nil || c.Writer.Written() || business.Status != http.StatusTooManyRequests {
		return
	}
	if retryAfter := strings.TrimSpace(headers.Get("Retry-After")); IsSafeUpstreamRetryAfter(retryAfter) {
		c.Header("Retry-After", retryAfter)
	}
}

// IsSafeUpstreamRetryAfter validates the header before forwarding it.
func IsSafeUpstreamRetryAfter(value string) bool {
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "\r\n") {
		return false
	}
	digitsOnly := true
	for _, char := range value {
		if char < '0' || char > '9' {
			digitsOnly = false
			break
		}
	}
	const maxDelay = 7 * 24 * time.Hour
	if digitsOnly {
		seconds, err := strconv.ParseUint(value, 10, 32)
		return err == nil && seconds <= uint64(maxDelay/time.Second)
	}
	retryAt, err := http.ParseTime(value)
	now := time.Now()
	return err == nil && retryAt.After(now) && !retryAt.After(now.Add(maxDelay))
}

func writeAnthropicBusinessError(c *gin.Context, business UpstreamBusinessError, headers http.Header) {
	MarkUpstreamBusinessError(c, business, headers)
	SetOpsLocalErrorType(c, business.Type, business.Code)
	body := gin.H{"type": "error", "error": gin.H{"type": business.Type, "message": business.Message}}
	if c.Writer.Written() {
		MarkOpsStreamFailure(c, business.Type, business.Code, business.Message, business.Status)
		if payload, err := json.Marshal(body); err == nil {
			_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", payload)
			c.Writer.Flush()
		}
		return
	}
	c.JSON(business.Status, body)
}

func writeOpenAIBusinessError(c *gin.Context, business UpstreamBusinessError, headers http.Header) {
	keepaliveCommitted := StopOpenAICompactSSEKeepaliveCommitted(c)
	MarkUpstreamBusinessError(c, business, headers)
	body := gin.H{"error": gin.H{"type": business.Type, "code": business.Code, "message": business.Message}}
	if keepaliveCommitted || c.Writer.Written() {
		MarkOpsStreamFailure(c, business.Type, business.Code, business.Message, business.Status)
		if keepaliveCommitted {
			writeOpenAICompactSSEFailureMessage(c, business.Status, business.Code, business.Message)
		}
		return
	}
	c.JSON(business.Status, body)
}
