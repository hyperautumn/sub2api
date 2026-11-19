package handler

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBillingErrorDetails_MapsGroupRPMExceededToTooManyRequests(t *testing.T) {
	status, code, msg, retryAfter := billingErrorDetails(service.ErrGroupRPMExceeded)
	require.Equal(t, http.StatusTooManyRequests, status)
	require.Equal(t, "rate_limit_exceeded", code)
	require.NotEmpty(t, msg)
	require.Greater(t, retryAfter, 0, "RPM exceeded should return positive Retry-After")
	require.LessOrEqual(t, retryAfter, 60)
}

func TestBillingErrorDetails_MapsUserRPMExceededToTooManyRequests(t *testing.T) {
	status, code, msg, retryAfter := billingErrorDetails(service.ErrUserRPMExceeded)
	require.Equal(t, http.StatusTooManyRequests, status)
	require.Equal(t, "rate_limit_exceeded", code)
	require.NotEmpty(t, msg)
	require.Greater(t, retryAfter, 0, "RPM exceeded should return positive Retry-After")
	require.LessOrEqual(t, retryAfter, 60)
}

func TestBillingErrorDetails_APIKeyRateLimitStillMaps(t *testing.T) {
	// 回归保护：加 RPM 分支后不应影响已有 APIKey rate limit 的映射。
	for _, err := range []error{
		service.ErrAPIKeyQuotaExhausted,
		service.ErrAPIKeyRateLimit5hExceeded,
		service.ErrAPIKeyRateLimit1dExceeded,
		service.ErrAPIKeyRateLimit7dExceeded,
		service.ErrDailyLimitExceeded,
		service.ErrWeeklyLimitExceeded,
		service.ErrMonthlyLimitExceeded,
	} {
		status, code, _, retryAfter := billingErrorDetails(err)
		require.Equal(t, http.StatusTooManyRequests, status, "status for %v", err)
		require.Equal(t, "rate_limit_exceeded", code)
		require.Zero(t, retryAfter)
	}
}

func TestBillingErrorDetails_BillingServiceUnavailableMapsTo503(t *testing.T) {
	status, code, _, retryAfter := billingErrorDetails(service.ErrBillingServiceUnavailable)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, "billing_service_error", code)
	require.Equal(t, 0, retryAfter, "non-RPM errors should not set Retry-After")
}

func TestBillingErrorDetails_BusinessDenialMapsTo403(t *testing.T) {
	for _, err := range []error{
		service.ErrInsufficientBalance,
		service.ErrSubscriptionInvalid,
		service.ErrSubscriptionNotFound,
		service.ErrSubscriptionExpired,
		service.ErrSubscriptionSuspended,
	} {
		status, code, msg, retryAfter := billingErrorDetails(err)
		require.Equal(t, http.StatusForbidden, status)
		require.Equal(t, "billing_error", code)
		require.NotEmpty(t, msg)
		require.Zero(t, retryAfter)
	}
}

func TestBillingErrorDetails_InternalErrorsUsePublicMessages(t *testing.T) {
	status, code, msg, _ := billingErrorDetails(service.ErrBillingPriceUnavailable)
	require.Equal(t, http.StatusInternalServerError, status)
	require.Equal(t, "billing_price_unavailable", code)
	require.Equal(t, service.ErrBillingPriceUnavailable.Message, msg)
	status, code, msg, _ = billingErrorDetails(errors.New("private backend error"))
	require.Equal(t, http.StatusInternalServerError, status)
	require.Equal(t, "billing_service_error", code)
	require.NotContains(t, msg, "private backend error")
}

func TestExtractQuotaResetSeconds_T19_HappyPath(t *testing.T) {
	err := service.ErrUserPlatformDailyQuotaExhausted.WithMetadata(map[string]string{
		"window_resets_at": time.Now().Add(10 * time.Second).UTC().Format(time.RFC3339),
	})
	got := extractQuotaResetSeconds(err)
	if got < 10 || got > 11 {
		t.Errorf("T19: got %d, want 10 or 11 (math.Ceil boundary)", got)
	}
}

func TestExtractQuotaResetSeconds_T20_NoMetadata(t *testing.T) {
	if got := extractQuotaResetSeconds(errors.New("naked error")); got != 0 {
		t.Errorf("T20: got %d, want 0 without a reset time", got)
	}
}

func TestExtractQuotaResetSeconds_T21_BadFormat(t *testing.T) {
	err := service.ErrUserPlatformDailyQuotaExhausted.WithMetadata(map[string]string{
		"window_resets_at": "not-a-time",
	})
	if got := extractQuotaResetSeconds(err); got != 0 {
		t.Errorf("T21: got %d, want 0 with an invalid reset time", got)
	}
}

func TestExtractQuotaResetSeconds_T22_PastReset(t *testing.T) {
	err := service.ErrUserPlatformDailyQuotaExhausted.WithMetadata(map[string]string{
		"window_resets_at": time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339),
	})
	if got := extractQuotaResetSeconds(err); got != 0 {
		t.Errorf("T22: got %d, want 0 after the reset time", got)
	}
}

func TestBillingErrorDetails_T10_QuotaExhaustedReturns429WithRetryAfter(t *testing.T) {
	// quota 超限映射 429 + Retry-After（RFC 6585 / 与 RPM 一致），
	// 让 SDK（OpenAI 兼容客户端等）能按 Retry-After 自动退避。
	// 旧实现用 403 导致客户端不退避直接报错。
	// 三个窗口共用同一映射分支，循环覆盖避免漏测某个窗口的 status/code。
	cases := []struct {
		name string
		err  error
	}{
		{"daily", service.ErrUserPlatformDailyQuotaExhausted.WithMetadata(map[string]string{
			"window_resets_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
		})},
		{"weekly", service.ErrUserPlatformWeeklyQuotaExhausted.WithMetadata(map[string]string{
			"window_resets_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
		})},
		{"monthly", service.ErrUserPlatformMonthlyQuotaExhausted.WithMetadata(map[string]string{
			"window_resets_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code, _, retryAfter := billingErrorDetails(tc.err)
			if status != http.StatusTooManyRequests {
				t.Errorf("status = %d, want 429", status)
			}
			if code != "rate_limit_exceeded" {
				t.Errorf("code = %q, want rate_limit_exceeded", code)
			}
			if retryAfter < 3599 || retryAfter > 3601 {
				t.Errorf("retryAfter = %d, want ~3600", retryAfter)
			}
		})
	}
}
