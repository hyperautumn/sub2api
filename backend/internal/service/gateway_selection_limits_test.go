//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type selectionWindowCostCache struct {
	sessionLimitCacheHotpathStub
}

func (c *selectionWindowCostCache) GetWindowCost(_ context.Context, accountID int64) (float64, bool, error) {
	cost, ok := c.batchData[accountID]
	return cost, ok, nil
}

func TestGatewaySelectionFiveHourWindowLimit(t *testing.T) {
	const model = "claude-opus-4-8"
	const sessionHash = "existing-session"
	groupID := int64(10)

	for _, loadBatch := range []bool{true, false} {
		mode := "legacy"
		if loadBatch {
			mode = "load-aware"
		}
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct {
				name           string
				stickyCost     float64
				backupCost     float64
				backupOverload bool
				wantAccountID  int64
			}{
				{name: "five accounts at hard cap", stickyCost: 35, backupCost: 36},
				{name: "fallback account under cap", stickyCost: 35, backupCost: 20, wantAccountID: 5},
				{name: "sticky reserve is available", stickyCost: 34, backupCost: 36, wantAccountID: 1},
				{name: "system-unavailable fallback", stickyCost: 35, backupCost: 20, backupOverload: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					future := time.Now().Add(time.Hour)
					usage := map[int64]float64{1: tc.stickyCost, 2: 36, 3: 36, 4: 36, 5: tc.backupCost}
					repo := &mockAccountRepoForPlatform{accounts: make([]Account, 5), accountsByID: make(map[int64]*Account)}
					for i := range repo.accounts {
						id := int64(i + 1)
						account := &repo.accounts[i]
						*account = Account{
							ID: id, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
							Status: StatusActive, Schedulable: true, Concurrency: 1,
							AccountGroups: []AccountGroup{{GroupID: groupID}},
							Extra:         map[string]any{"window_cost_limit": 30, "window_cost_sticky_reserve": 5},
						}
						if id == 5 && tc.backupOverload {
							account.OverloadUntil = &future
						}
						repo.accountsByID[id] = account
					}
					cfg := testConfig()
					cfg.Gateway.Scheduling.LoadBatchEnabled = loadBatch
					svc := &GatewayService{
						accountRepo: repo,
						cfg:         cfg,
						groupRepo: &mockGroupRepoForGateway{
							groups: map[int64]*Group{groupID: {
								ID: groupID, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true,
								ModelRoutingEnabled: true, ModelRouting: map[string][]int64{model: {1}},
							}},
						},
						cache:              &mockGatewayCacheForPlatform{sessionBindings: map[string]int64{sessionHash: 1}},
						sessionLimitCache:  &selectionWindowCostCache{sessionLimitCacheHotpathStub{batchData: usage}},
						usageLogRepo:       &usageLogWindowBatchRepoStub{},
						concurrencyService: NewConcurrencyService(&mockConcurrencyCache{}),
					}
					result, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, sessionHash, model, nil, "", 0)
					if tc.wantAccountID != 0 {
						require.NoError(t, err)
						require.Equal(t, tc.wantAccountID, result.Account.ID)
						return
					}
					require.Nil(t, result)
					require.ErrorIs(t, err, ErrNoAvailableAccounts)
					if tc.backupOverload {
						require.NotErrorIs(t, err, ErrAccountSelectionRateLimited)
					} else {
						require.ErrorIs(t, err, ErrAccountSelectionRateLimited)
						require.Equal(t, "window_cost", GatewaySelectionLimitReason(err))
						require.True(t, GatewaySelectionRetryAt(err).IsZero())
					}
				})
			}
		})
	}
}

func TestGatewaySelectionQuotaFilteredByRepository(t *testing.T) {
	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
				Status: StatusActive, Schedulable: true,
				Extra: map[string]any{"quota_limit": 10, "quota_used": 10}},
		},
	}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &GatewayService{accountRepo: repo, cfg: cfg, concurrencyService: NewConcurrencyService(&mockConcurrencyCache{})}
	_, err := svc.SelectAccountWithLoadAwareness(context.Background(), nil, "", "claude-opus-4-8", nil, "", 0)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.ErrorIs(t, err, ErrAccountSelectionRateLimited)
	require.Equal(t, "quota", GatewaySelectionLimitReason(err))
}
