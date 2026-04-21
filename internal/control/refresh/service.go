package refresh

import (
	"context"
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

type Service struct {
	repo    account.Repository
	runtime *account.Runtime
	cfg     *config.Service
	xai     *xai.Client
	wg      sync.WaitGroup
	stopCh  chan struct{}
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
}

func (s *Service) RefreshOnImport(ctx context.Context, tokens []string) (Result, error) {
	return s.RefreshTokens(ctx, tokens)
}

func (s *Service) RefreshPool(ctx context.Context, pool string) (Result, error) {
	return s.RefreshTokens(ctx, s.runtime.TokensByPool(pool))
}

func (s *Service) RefreshTokens(ctx context.Context, tokens []string) (Result, error) {
	result := Result{}
	for _, token := range tokens {
		result.Checked++
		records, err := s.repo.GetAccounts(ctx, []string{token})
		if err != nil || len(records) == 0 {
			result.Failed++
			if err != nil {
				return result, err
			}
			continue
		}
		record := records[0]
		quotas, err := s.xai.FetchAllQuotas(ctx, record.Token, record.Pool)
		if err != nil {
			result.Failed++
			continue
		}
		pool := account.InferPool(quotas)
		patch := account.Patch{
			Token:          record.Token,
			Quota:          quotas,
			Pool:           stringPtr(pool),
			Status:         statusPtr(account.StatusActive),
			StateReason:    stringPtr(""),
			LastSyncAt:     int64Ptr(account.NowMS()),
			UsageSyncDelta: intPtr(1),
		}
		if _, err := s.repo.PatchAccounts(ctx, []account.Patch{patch}); err != nil {
			result.Failed++
			continue
		}
		updated, err := s.repo.GetAccounts(ctx, []string{record.Token})
		if err == nil && len(updated) > 0 {
			s.runtime.ReplaceRecord(updated[0])
		}
		result.Refreshed++
	}
	return result, nil
}

func intPtr(value int) *int          { return &value }
func int64Ptr(value int64) *int64    { return &value }
func stringPtr(value string) *string { return &value }
func statusPtr(value account.Status) *account.Status { return &value }
