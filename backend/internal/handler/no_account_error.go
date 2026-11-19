package handler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// noAccountErrorClassification is the protocol error to return when account
// selection fails.
type noAccountErrorClassification struct {
	Status        int
	ErrType       string
	Code          string
	Message       string
	ModelNotFound bool
}

func classifySelectionFailureError(err error, fallback noAccountErrorClassification) noAccountErrorClassification {
	if fallback.ModelNotFound || !errors.Is(err, service.ErrAccountSelectionRateLimited) {
		return fallback
	}
	classification := noAccountErrorClassification{
		Status:  http.StatusTooManyRequests,
		ErrType: "rate_limit_error",
		Code:    "rate_limit_exceeded",
		Message: "Available accounts are temporarily rate-limited or have reached their usage quota. Please retry later.",
	}
	switch service.GatewaySelectionLimitReason(err) {
	case "window_cost":
		classification.Code = "quota_exceeded"
		classification.Message = "The available accounts have reached their 5-hour spend limit."
	case "quota":
		classification.Code = "quota_exceeded"
		classification.Message = "The available accounts have exhausted their usage quota."
	}
	return classification
}

// classifyNoAccountError combines scheduler reasons with the persistent model
// availability lookup. A confirmed model configuration error takes precedence.

// routingModel is the model name that account selection actually compared
// against (i.e. after group-level dispatch mapping). displayModel is the
// raw model the caller asked for; it is used only in the user-facing error
// message so that internal mapping details don't leak. Most callers pass
// the same value for both.
//
// platform is the platform the request was routed to (use
// service.PlatformOpenAI / PlatformAnthropic / PlatformGemini). It is
// required because Anthropic/Gemini routes additionally surface
// mixed-scheduled Antigravity accounts; passing the wrong platform would
// flip a legitimate 503 to a misleading 404 (or vice versa).
func classifyNoAccountError(
	ctx context.Context,
	diag service.ModelAvailabilityDiagnoser,
	apiKey *service.APIKey,
	routingModel string,
	displayModel string,
	platform string,
	selectionErr error,
) noAccountErrorClassification {
	fallback := noAccountErrorClassification{
		Status:  http.StatusServiceUnavailable,
		ErrType: "api_error",
		Message: "Service temporarily unavailable",
	}
	if errors.Is(selectionErr, service.ErrClaudeCodeOnly) {
		return noAccountErrorClassification{
			Status:  http.StatusForbidden,
			ErrType: "permission_error",
			Code:    "client_not_allowed",
			Message: "This group only allows Claude Code clients.",
		}
	}
	if errors.Is(selectionErr, service.ErrAccountSelectionUpstreamBalance) {
		return noAccountErrorClassification{
			Status:  http.StatusForbidden,
			ErrType: "permission_error",
			Code:    "insufficient_balance",
			Message: "The available upstream accounts have insufficient balance.",
		}
	}
	if selectionErr != nil && !errors.Is(selectionErr, service.ErrNoAvailableAccounts) &&
		!errors.Is(selectionErr, service.ErrAccountSelectionRateLimited) {
		return fallback
	}
	// A scoped scheduler rejection has already checked the resolved group,
	// routing restrictions, and model. The original group can differ after fallback.
	if service.GatewaySelectionLimitReason(selectionErr) != "" {
		return classifySelectionFailureError(selectionErr, fallback)
	}
	if errors.Is(selectionErr, service.ErrChannelModelRestricted) {
		return noAccountErrorClassification{
			Status:        http.StatusNotFound,
			ErrType:       "model_not_found",
			Code:          "model_not_found",
			Message:       "Requested model is unavailable",
			ModelNotFound: true,
		}
	}

	routingModel = strings.TrimSpace(routingModel)
	displayModel = strings.TrimSpace(displayModel)
	if displayModel == "" {
		displayModel = routingModel
	}
	if diag == nil || apiKey == nil || apiKey.GroupID == nil || routingModel == "" {
		return classifySelectionFailureError(selectionErr, fallback)
	}

	result := diag.DiagnoseModelAvailabilityForPlatform(ctx, apiKey.GroupID, routingModel, platform)
	if result.HasAccountsInPool && !result.HasModelSupport {
		message := fmt.Sprintf("Model %q is not supported by any configured account in this group", displayModel)
		if result.HasRetiredSubscriptionModel {
			message = fmt.Sprintf("Model %q targets GPT-5.5, which retired from Codex with ChatGPT sign-in on October 14, 2026. Select an available replacement such as GPT-6.1 Sol or GPT-6 Luna, or configure an API-key upstream that supports GPT-5.5.", displayModel)
		}
		return noAccountErrorClassification{
			Status:        http.StatusNotFound,
			ErrType:       "model_not_found",
			Code:          "model_not_found",
			Message:       message,
			ModelNotFound: true,
		}
	}
	if result.AllModelAccountsRateLimited && errors.Is(selectionErr, service.ErrNoAvailableAccounts) {
		selectionErr = service.ErrAccountSelectionRateLimited
	}
	return classifySelectionFailureError(selectionErr, fallback)
}

// classifyNoAccountErrorFromGin is a thin wrapper that forwards the gin
// context's underlying request context. Most call sites already have a
// *gin.Context handy, so this keeps the call sites uncluttered.
func classifyNoAccountErrorFromGin(
	c *gin.Context,
	diag service.ModelAvailabilityDiagnoser,
	apiKey *service.APIKey,
	routingModel string,
	displayModel string,
	platform string,
	selectionErr error,
) noAccountErrorClassification {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	classification := classifyNoAccountError(ctx, diag, apiKey, routingModel, displayModel, platform, selectionErr)
	switch classification.Status {
	case http.StatusNotFound:
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalModelConfiguration)
	case http.StatusForbidden:
		if errors.Is(selectionErr, service.ErrAccountSelectionUpstreamBalance) {
			markOpsRoutingCapacityLimited(c)
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonUpstreamRejection)
		} else {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
		}
	case http.StatusTooManyRequests:
		markOpsRoutingCapacityLimited(c)
		if c != nil && c.Writer != nil && !c.Writer.Written() {
			if seconds := time.Until(service.GatewaySelectionRetryAt(selectionErr)).Seconds(); seconds > 0 {
				c.Header("Retry-After", strconv.Itoa(int(math.Ceil(seconds))))
			}
		}
	}
	return classification
}

func classifyOpenAICompatibleNoAccountErrorFromGin(
	c *gin.Context,
	diag service.ModelAvailabilityDiagnoser,
	apiKey *service.APIKey,
	routingModel string,
	displayModel string,
	selectionErr error,
) noAccountErrorClassification {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	return classifyNoAccountErrorFromGin(
		c,
		diag,
		apiKey,
		routingModel,
		displayModel,
		openAICompatibleRequestPlatform(ctx, apiKey),
		selectionErr,
	)
}

func openAICompatibleSelectionErrorForLog(err error, platform string) error {
	if err == nil || platform != service.PlatformGrok {
		return err
	}
	message := strings.ReplaceAll(err.Error(), "OpenAI accounts", "Grok accounts")
	if message == err.Error() {
		return err
	}
	return fmt.Errorf("%s", message)
}
