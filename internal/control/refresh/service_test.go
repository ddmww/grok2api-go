package refresh

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/proxy"
	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
	"github.com/ddmww/grok2api-go/internal/platform/paths"
	"github.com/ddmww/grok2api-go/internal/testutil"
)

func TestRefreshTokensRecoversCoolingAndWritesDetailedQuota(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ACCOUNT_STORAGE", "local")
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("ACCOUNT_LOCAL_PATH", filepath.Join(dataDir, "accounts.db"))
	if err := paths.EnsureRuntimeDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}

	repo, err := account.NewRepositoryFromEnv()
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	defer repo.Close()
	if err := repo.Initialize(context.Background()); err != nil {
		t.Fatalf("init repo: %v", err)
	}
	if _, err := repo.UpsertAccounts(context.Background(), []account.Upsert{{Token: "token-1", Pool: "basic"}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := repo.PatchAccounts(context.Background(), []account.Patch{{
		Token:       "token-1",
		Status:      func() *account.Status { status := account.StatusCooling; return &status }(),
		StateReason: func() *string { reason := "rate_limited"; return &reason }(),
		ExtMerge:    map[string]any{"cooldown_until": account.NowMS() + 60000},
	}}); err != nil {
		t.Fatalf("patch cooling: %v", err)
	}

	fake := testutil.NewFakeGrokServer()
	defer fake.Close()
	cfg := testutil.NewConfig(map[string]any{
		"proxy": map[string]any{
			"egress":   map[string]any{"mode": "direct"},
			"upstream": map[string]any{"base_url": fake.BaseURL()},
		},
		"account": map[string]any{
			"refresh": map[string]any{"usage_concurrency": 4},
		},
	})
	runtime := account.NewRuntime(repo)
	if err := runtime.Sync(context.Background()); err != nil {
		t.Fatalf("sync runtime: %v", err)
	}
	service := New(repo, runtime, cfg, xai.NewClient(cfg, proxy.NewRuntime(cfg)))

	result, err := service.RefreshTokens(context.Background(), []string{"token-1"})
	if err != nil {
		t.Fatalf("refresh tokens: %v", err)
	}
	if result.Checked != 1 || result.Refreshed != 1 || result.Recovered != 1 || result.Failed != 0 {
		t.Fatalf("unexpected result: %#v", result)
	}

	records, err := repo.GetAccounts(context.Background(), []string{"token-1"})
	if err != nil || len(records) != 1 {
		t.Fatalf("get refreshed record: %v %#v", err, records)
	}
	record := records[0]
	if record.Status != account.StatusActive {
		t.Fatalf("expected active after refresh, got %#v", record)
	}
	if record.Quota.Auto.Source != account.QuotaSourceReal || record.Quota.Fast.Source != account.QuotaSourceReal || record.Quota.Expert.Source != account.QuotaSourceReal {
		t.Fatalf("expected detailed real quotas, got %#v", record.Quota)
	}
	if record.Quota.Auto.Total != 20 || record.Quota.Fast.Total != 60 || record.Quota.Expert.Total != 8 {
		t.Fatalf("unexpected refreshed quota totals: %#v", record.Quota)
	}
}

func TestRefreshOnDemandRespectsMinInterval(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ACCOUNT_STORAGE", "local")
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("ACCOUNT_LOCAL_PATH", filepath.Join(dataDir, "accounts.db"))
	if err := paths.EnsureRuntimeDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}

	repo, err := account.NewRepositoryFromEnv()
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	defer repo.Close()
	if err := repo.Initialize(context.Background()); err != nil {
		t.Fatalf("init repo: %v", err)
	}
	if _, err := repo.UpsertAccounts(context.Background(), []account.Upsert{{Token: "token-1", Pool: "basic"}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	fake := testutil.NewFakeGrokServer()
	defer fake.Close()
	cfg := testutil.NewConfig(map[string]any{
		"proxy": map[string]any{
			"egress":   map[string]any{"mode": "direct"},
			"upstream": map[string]any{"base_url": fake.BaseURL()},
		},
		"account": map[string]any{
			"refresh": map[string]any{
				"usage_concurrency":          2,
				"on_demand_min_interval_sec": 300,
			},
		},
	})
	runtime := account.NewRuntime(repo)
	if err := runtime.Sync(context.Background()); err != nil {
		t.Fatalf("sync runtime: %v", err)
	}
	service := New(repo, runtime, cfg, xai.NewClient(cfg, proxy.NewRuntime(cfg)))

	first, err := service.RefreshOnDemand(context.Background())
	if err != nil {
		t.Fatalf("first refresh on demand: %v", err)
	}
	if first.Refreshed != 1 {
		t.Fatalf("expected first refresh to run, got %#v", first)
	}

	second, err := service.RefreshOnDemand(context.Background())
	if err != nil {
		t.Fatalf("second refresh on demand: %v", err)
	}
	if second.Checked != 0 || second.Refreshed != 0 || second.Failed != 0 {
		t.Fatalf("expected throttled no-op refresh, got %#v", second)
	}
}

func TestRefreshCallSyncsOnlySelectedMode(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ACCOUNT_STORAGE", "local")
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("ACCOUNT_LOCAL_PATH", filepath.Join(dataDir, "accounts.db"))
	if err := paths.EnsureRuntimeDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}

	repo, err := account.NewRepositoryFromEnv()
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	defer repo.Close()
	if err := repo.Initialize(context.Background()); err != nil {
		t.Fatalf("init repo: %v", err)
	}
	if _, err := repo.UpsertAccounts(context.Background(), []account.Upsert{{Token: "token-1", Pool: "basic"}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	fake := testutil.NewFakeGrokServer()
	defer fake.Close()
	cfg := testutil.NewConfig(map[string]any{
		"proxy": map[string]any{
			"egress":   map[string]any{"mode": "direct"},
			"upstream": map[string]any{"base_url": fake.BaseURL()},
		},
	})
	runtime := account.NewRuntime(repo)
	if err := runtime.Sync(context.Background()); err != nil {
		t.Fatalf("sync runtime: %v", err)
	}
	service := New(repo, runtime, cfg, xai.NewClient(cfg, proxy.NewRuntime(cfg)))

	if err := service.RefreshCall(context.Background(), "token-1", "fast"); err != nil {
		t.Fatalf("refresh call: %v", err)
	}
	if got := fake.RateLimitCallCount("token-1", "fast"); got != 1 {
		t.Fatalf("expected exactly one fast usage probe, got %d", got)
	}
	if got := fake.RateLimitCallCount("token-1", "auto"); got != 0 {
		t.Fatalf("expected no auto usage probe, got %d", got)
	}

	records, err := repo.GetAccounts(context.Background(), []string{"token-1"})
	if err != nil || len(records) != 1 {
		t.Fatalf("get refreshed record: %v %#v", err, records)
	}
	record := records[0]
	if record.Quota.Fast.Source != account.QuotaSourceReal {
		t.Fatalf("expected fast quota to be synced, got %#v", record.Quota.Fast)
	}
	if record.Quota.Auto.Source == account.QuotaSourceReal {
		t.Fatalf("expected auto quota to remain untouched, got %#v", record.Quota.Auto)
	}
}
