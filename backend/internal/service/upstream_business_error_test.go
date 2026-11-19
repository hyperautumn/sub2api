package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClassifyUpstreamBusinessError(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantCode   string
	}{
		{"overdue", 403, `{"error":{"code":"AccountOverdueError"}}`, 403, "insufficient_balance"},
		{"credits", 402, `{"error":{"code":"insufficient_credits"}}`, 403, "insufficient_balance"},
		{"account wallet in nested cause", 400, `{"error":{"cause":{"errorKey":"insufficient_balance"}}}`, 403, "insufficient_balance"},
		{"quota in forbidden response", 403, `{"error":{"code":"QuotaExceeded.AgentPlan"}}`, 429, "quota_exceeded"},
		{"quota in Google error details", 403, `{"error":{"details":[{"reason":"SPEND_LIMIT_EXCEEDED"}]}}`, 429, "quota_exceeded"},
		{"only HTTP rate limit signal", 429, "", 429, "rate_limit_exceeded"},
		{"model denied", 403, `{"error":{"code":"model_access_denied"}}`, 404, "model_not_found"},
		{"model unavailable in invalid request", 400, `{"error":{"code":"model_not_found"}}`, 404, "model_not_found"},
		{"explicit operation forbidden", 403, `{"error":{"code":"operation_not_allowed"}}`, 403, "permission_denied"},
		{"ambiguous upstream permission", 403, `{"error":{"type":"permission_error","status":"PERMISSION_DENIED"}}`, 0, ""},
		{"invalid upstream key", 401, `{"error":{"code":"invalid_api_key"}}`, 0, ""},
		{"upstream endpoint missing", 404, `{"error":{"code":"not_found"}}`, 0, ""},
		{"upstream outage", 503, `{"error":{"code":"service_unavailable"}}`, 0, ""},
		{"echoed code", 400, `{"error":{"message":"QuotaExceeded"},"request":{"code":"model_not_found"}}`, 0, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := ClassifyUpstreamBusinessError(test.status, []byte(test.body))
			require.Equal(t, test.wantStatus != 0, ok)
			require.Equal(t, test.wantStatus, got.Status)
			require.Equal(t, test.wantCode, got.Code)
		})
	}
}

func TestUpstreamCredentialFailureHasPriorityOverBusinessPayload(t *testing.T) {
	err := &UpstreamFailoverError{
		Stage:        GatewayFailureStageAccountAuth,
		StatusCode:   http.StatusForbidden,
		ResponseBody: []byte(`{"error":{"code":"QuotaExceeded"}}`),
	}
	_, ok := err.BusinessError()
	require.False(t, ok)
}
