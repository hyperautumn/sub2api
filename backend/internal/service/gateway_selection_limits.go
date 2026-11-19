package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// GatewaySelectionLimitReason identifies the scheduling constraint shared by
// the remaining candidates. A mixed pool has the generic account_limits reason.
func GatewaySelectionLimitReason(err error) string {
	var limit gatewaySelectionLimitError
	if errors.As(err, &limit) {
		return limit.reason
	}
	return ""
}

// GatewaySelectionRetryAt returns the earliest known time a candidate can
// serve this request again. A predicted or unknown reset is left unset.
func GatewaySelectionRetryAt(err error) time.Time {
	var limit gatewaySelectionLimitError
	if errors.As(err, &limit) {
		return limit.retryAt
	}
	return time.Time{}
}

type gatewaySelectionLimitError struct {
	accountSelectionRateLimitError
	reason  string
	retryAt time.Time
}

type gatewayCandidateLimit struct {
	reason string
	reset  time.Time
}

type gatewaySessionLimitIDsKey struct{}

// gatewayNoSelectionError confirms a limit across the compatible candidates
// in the resolved pool. The regular scheduling query excludes accounts under
// cooldown and static quotas, so inspect the configured pool on this path.
func (s *GatewayService) gatewayNoSelectionError(
	ctx context.Context,
	groupID *int64,
	model, platform string,
	useMixed bool,
	requirePrivacy bool,
	listed []Account,
	excluded map[int64]struct{},
	fallback error,
) error {
	if s.accountRepo == nil {
		return fallback
	}

	platforms := []string{platform}
	if useMixed {
		platforms = append(platforms, PlatformAntigravity)
	}
	queryGroupID := groupID
	includeGrouped := s.cfg != nil && s.cfg.RunMode == config.RunModeSimple && (!useMixed || groupID == nil)
	if includeGrouped {
		queryGroupID = nil
	}
	accounts, err := s.accountRepo.ListModelAvailabilityCandidates(ctx, queryGroupID, platforms, includeGrouped)
	if err != nil {
		return fmt.Errorf("inspect account pool: %w", err)
	}

	// Prefer the snapshots actually used in the failed selection, including
	// model limits installed by the scheduling threshold check.
	observed := make(map[int64]*Account, len(listed))
	for i := range listed {
		observed[listed[i].ID] = &listed[i]
	}
	sessionLimited, _ := ctx.Value(gatewaySessionLimitIDsKey{}).(map[int64]struct{})
	needsUpstreamCheck := s.needsUpstreamChannelRestrictionCheck(ctx, groupID)
	now := time.Now()
	var result gatewayCandidateLimit
	unknownReset := false
	for i := range accounts {
		account := &accounts[i]
		if snapshot := observed[account.ID]; snapshot != nil {
			account = snapshot
		}
		_, sessionDenied := sessionLimited[account.ID]
		if _, skip := excluded[account.ID]; skip && !sessionDenied {
			continue
		}
		if !account.IsActive() || !account.Schedulable ||
			!s.isAccountAllowedForPlatform(account, platform, useMixed) ||
			!s.isGatewayAccountProfitEligible(ctx, account) ||
			(model != "" && !s.isModelSupportedByAccountWithContext(ctx, account, model)) ||
			(needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, account, model)) {
			continue
		}
		if requirePrivacy && !account.IsPrivacySet() {
			return fallback
		}

		limit := s.gatewayAccountRequestLimit(ctx, account, model, now)
		if sessionDenied && limit.reason == "" && account.IsSchedulable() {
			limit.reason = "session_limit"
		}
		if limit.reason == "" {
			return fallback
		}
		if result.reason == "" {
			result.reason = limit.reason
		} else if result.reason != limit.reason {
			result.reason = "account_limits"
		}
		if limit.reset.IsZero() {
			unknownReset = true
		} else if result.reset.IsZero() || limit.reset.Before(result.reset) {
			result.reset = limit.reset
		}
	}

	if result.reason == "" {
		return fallback
	}
	if unknownReset {
		result.reset = time.Time{}
	}
	return gatewaySelectionLimitError{
		accountSelectionRateLimitError: accountSelectionRateLimitError{fmt.Errorf("%w (%s)", fallback, result.reason)},
		reason:                         result.reason,
		retryAt:                        result.reset,
	}
}

func (s *GatewayService) gatewayAccountRequestLimit(ctx context.Context, account *Account, model string, now time.Time) gatewayCandidateLimit {
	if account.AutoPauseOnExpired && account.ExpiresAt != nil && !account.ExpiresAt.After(now) ||
		account.OverloadUntil != nil && account.OverloadUntil.After(now) {
		return gatewayCandidateLimit{}
	}
	if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(now) {
		if IsAccountSchedulingThresholdReason(account.TempUnschedulableReason) {
			// The account might also have a different active limit.
			return gatewayCandidateLimit{reason: "quota"}
		}
		return gatewayCandidateLimit{}
	}
	if !s.isAccountSchedulableForQuota(account) {
		return gatewayCandidateLimit{reason: "quota"}
	}
	if account.RateLimitResetAt != nil && account.RateLimitResetAt.After(now) {
		return gatewayCandidateLimit{reason: "account_rate_limit"}
	}
	if !s.isAccountSchedulableForWindowCost(ctx, account, false) {
		// Stored SessionWindowEnd can be predicted when the upstream omits a reset.
		return gatewayCandidateLimit{reason: "window_cost"}
	}
	if !s.isAccountSchedulableForRPM(ctx, account, false) {
		return gatewayCandidateLimit{reason: "account_rpm"}
	}
	if !s.isAccountSchedulableForModelSelection(ctx, account, model) {
		reset := time.Time{}
		if remaining := account.GetRateLimitRemainingTimeWithContext(ctx, model); remaining > 0 {
			reset = now.Add(remaining)
		}
		return gatewayCandidateLimit{reason: "model_rate_limit", reset: reset}
	}
	return gatewayCandidateLimit{}
}
