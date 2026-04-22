package refresh

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
	"github.com/ddmww/grok2api-go/internal/platform/config"
)

type Result struct {
	Checked   int `json:"checked"`
	Refreshed int `json:"refreshed"`
	Recovered int `json:"recovered"`
	Failed    int `json:"failed"`
}

type refreshOutcome struct {
	token  string
	patch  *account.Patch
	result Result
}

type Service struct {
	repo    account.Repository
	runtime *account.Runtime
	cfg     *config.Service
	xai     *xai.Client
	wg      sync.WaitGroup
	stopCh  chan struct{}
	odMu    sync.Mutex
	odLast  time.Time
}

func New(repo account.Repository, runtime *account.Runtime, cfg *config.Service, client *xai.Client) *Service {
	return &Service{repo: repo, runtime: runtime, cfg: cfg, xai: client, stopCh: make(chan struct{})}
}

func (s *Service) Start() {
	type loop struct {
		pool     string
		interval int
	}
	loops := []loop{
		{pool: "basic", interval: s.cfg.GetInt("account.refresh.basic_interval_sec", 36000)},
		{pool: "super", interval: s.cfg.GetInt("account.refresh.super_interval_sec", 7200)},
		{pool: "heavy", interval: s.cfg.GetInt("account.refresh.heavy_interval_sec", 7200)},
	}
	for _, current := range loops {
		if current.interval <= 0 {
			continue
		}
		s.wg.Add(1)
		go func(item loop) {
			defer s.wg.Done()
			ticker := time.NewTicker(time.Duration(item.interval) * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					_, _ = s.RefreshPool(context.Background(), item.pool)
				case <-s.stopCh:
					return
				}
			}
		}(current)
	}
}

func (s *Service) Stop() {
	close(s.stopCh)
	s.wg.Wait()
	if s.xai != nil {
		s.xai.CloseSharedUsageSession()
	}
}

func (s *Service) RefreshOnImport(ctx context.Context, tokens []string) (Result, error) {
	return s.RefreshTokens(ctx, tokens)
}

func (s *Service) RefreshOnDemand(ctx context.Context) (Result, error) {
	minInterval := s.cfg.GetInt("account.refresh.on_demand_min_interval_sec", 300)
	if minInterval < 0 {
		minInterval = 0
	}
	now := time.Now()

	s.odMu.Lock()
	if minInterval > 0 && !s.odLast.IsZero() && now.Sub(s.odLast) < time.Duration(minInterval)*time.Second {
		s.odMu.Unlock()
		return Result{}, nil
	}
	s.odLast = now
	s.odMu.Unlock()

	result, err := s.refreshManageable(ctx)
	if err != nil {
		s.odMu.Lock()
		s.odLast = time.Time{}
		s.odMu.Unlock()
		return Result{}, err
	}
	return result, nil
}

func (s *Service) RefreshPool(ctx context.Context, pool string) (Result, error) {
	tokens := s.runtime.TokensByPool(pool)
	if len(tokens) == 0 {
		return Result{}, nil
	}
	records, err := s.repo.GetAccounts(ctx, tokens)
	if err != nil {
		return Result{}, err
	}
	interval := s.poolInterval(pool)
	filtered := make([]account.Record, 0, len(records))
	for _, record := range records {
		if shouldScheduledRefresh(record, interval) {
			filtered = append(filtered, record)
		}
	}
	return s.refreshRecords(ctx, filtered)
}

func (s *Service) RefreshTokens(ctx context.Context, tokens []string) (Result, error) {
	if len(tokens) == 0 {
		return Result{}, nil
	}
	records, err := s.repo.GetAccounts(ctx, tokens)
	if err != nil {
		return Result{}, err
	}
	return s.refreshRecords(ctx, records)
}

func (s *Service) refreshManageable(ctx context.Context) (Result, error) {
	page := 1
	pageSize := 1000
	records := make([]account.Record, 0, pageSize)
	for {
		current, err := s.repo.ListAccounts(ctx, account.ListQuery{
			Page:           page,
			PageSize:       pageSize,
			IncludeDeleted: false,
			SortBy:         "created_at",
			SortDesc:       true,
		})
		if err != nil {
			return Result{}, err
		}
		for _, record := range current.Items {
			if record.IsDeleted() {
				continue
			}
			status := record.EffectiveStatus(account.NowMS())
			if status == account.StatusActive || status == account.StatusCooling {
				records = append(records, record)
			}
		}
		if int64(page*pageSize) >= current.Total || len(current.Items) == 0 {
			break
		}
		page++
	}
	return s.refreshRecords(ctx, records)
}

func (s *Service) refreshRecords(ctx context.Context, records []account.Record) (Result, error) {
	candidates := make([]account.Record, 0, len(records))
	for _, record := range records {
		if !record.IsDeleted() {
			candidates = append(candidates, record)
		}
	}
	if len(candidates) == 0 {
		return Result{}, nil
	}
	session, err := s.xai.SharedUsageSession()
	if err != nil {
		return Result{}, err
	}

	concurrency := s.cfg.GetInt("account.refresh.usage_concurrency", 20)
	if concurrency <= 0 {
		concurrency = 20
	}
	sem := make(chan struct{}, concurrency)
	outcomes := make(chan refreshOutcome, len(candidates))
	var wg sync.WaitGroup
	for _, record := range candidates {
		record := record
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			outcomes <- s.refreshOne(ctx, session, record)
		}()
	}
	wg.Wait()
	close(outcomes)

	patches := make([]account.Patch, 0, len(candidates))
	refreshedTokens := make([]string, 0, len(candidates))
	result := Result{}
	for outcome := range outcomes {
		result.Checked += outcome.result.Checked
		result.Refreshed += outcome.result.Refreshed
		result.Recovered += outcome.result.Recovered
		result.Failed += outcome.result.Failed
		if outcome.patch != nil {
			patches = append(patches, *outcome.patch)
			refreshedTokens = append(refreshedTokens, outcome.token)
		}
	}

	if len(patches) > 0 {
		if _, err := s.repo.PatchAccounts(ctx, patches); err != nil {
			return result, err
		}
		updated, err := s.repo.GetAccounts(ctx, refreshedTokens)
		if err != nil {
			return result, err
		}
		for _, record := range updated {
			s.runtime.ReplaceRecord(record)
		}
	}
	return result, nil
}

func (s *Service) refreshOne(ctx context.Context, session *xai.RequestSession, record account.Record) refreshOutcome {
	now := account.NowMS()
	outcome := refreshOutcome{
		token:  record.Token,
		result: Result{Checked: 1},
	}
	probe, err := session.FetchQuotaProbe(ctx, record.Token)
	if err != nil {
		switch {
		case isStatus(err, 401):
			outcome.patch = &account.Patch{
				Token:          record.Token,
				Status:         statusPtr(account.StatusExpired),
				StateReason:    stringPtr("rate_limits_auth_failed"),
				LastSyncAt:     int64Ptr(now),
				LastFailAt:     int64Ptr(now),
				LastFailReason: stringPtr(err.Error()),
				UsageSyncDelta: intPtr(1),
				ExtMerge: map[string]any{
					"expired_at":     now,
					"expired_reason": "rate_limits_auth_failed",
				},
			}
			return outcome
		case isStatus(err, 429):
			outcome.patch = &account.Patch{
				Token:          record.Token,
				Status:         statusPtr(account.StatusCooling),
				StateReason:    stringPtr("rate_limited"),
				LastSyncAt:     int64Ptr(now),
				LastFailAt:     int64Ptr(now),
				LastFailReason: stringPtr(err.Error()),
				UsageSyncDelta: intPtr(1),
			}
			return outcome
		default:
			outcome.result.Failed = 1
			return outcome
		}
	}

	quotas := map[string]account.QuotaWindow{"auto": probe}
	pool := account.InferPool(quotas)
	if pool == "" {
		pool = record.Pool
	}
	detailed, detailErr := session.FetchDetailedQuotas(ctx, record.Token, pool, quotas)
	if detailErr != nil && len(detailed) == 0 {
		detailed = quotas
	}
	if len(detailed) == 0 {
		outcome.result.Failed = 1
		return outcome
	}

	patch := account.Patch{
		Token:          record.Token,
		Quota:          detailed,
		Pool:           stringPtr(pool),
		Status:         statusPtr(account.StatusActive),
		StateReason:    stringPtr(""),
		LastSyncAt:     int64Ptr(now),
		UsageSyncDelta: intPtr(1),
		ExtMerge: map[string]any{
			"cooldown_until": int64(0),
		},
	}
	outcome.patch = &patch
	outcome.result.Refreshed = 1
	if record.Status == account.StatusCooling {
		outcome.result.Recovered = 1
	}
	return outcome
}

func (s *Service) poolInterval(pool string) int64 {
	switch account.NormalizePool(pool) {
	case "super":
		return int64(s.cfg.GetInt("account.refresh.super_interval_sec", 7200)) * 1000
	case "heavy":
		return int64(s.cfg.GetInt("account.refresh.heavy_interval_sec", 7200)) * 1000
	default:
		return int64(s.cfg.GetInt("account.refresh.basic_interval_sec", 36000)) * 1000
	}
}

func shouldScheduledRefresh(record account.Record, intervalMS int64) bool {
	if record.IsDeleted() {
		return false
	}
	if record.Status == account.StatusDisabled || record.Status == account.StatusExpired {
		return false
	}
	if record.Status == account.StatusCooling {
		return true
	}
	if intervalMS <= 0 {
		return true
	}
	if record.LastSyncAt <= 0 {
		return true
	}
	return account.NowMS()-record.LastSyncAt >= intervalMS
}

func isStatus(err error, status int) bool {
	var upstream *xai.UpstreamError
	if errors.As(err, &upstream) {
		return upstream.Status == status
	}
	return false
}

func intPtr(value int) *int                          { return &value }
func int64Ptr(value int64) *int64                    { return &value }
func stringPtr(value string) *string                 { return &value }
func statusPtr(value account.Status) *account.Status { return &value }
