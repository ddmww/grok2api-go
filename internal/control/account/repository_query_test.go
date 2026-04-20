package account

import (
	"context"
	"path/filepath"
	"testing"
)

func testIntPtr(value int) *int          { return &value }
func testStatusPtr(value Status) *Status { return &value }

func TestRepositoryListAccountsAndSummary(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ACCOUNT_STORAGE", "local")
	t.Setenv("ACCOUNT_LOCAL_PATH", filepath.Join(dataDir, "accounts.db"))

	repo, err := NewRepositoryFromEnv()
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	defer repo.Close()
	if err := repo.Initialize(context.Background()); err != nil {
		t.Fatalf("init repo: %v", err)
	}

	if _, err := repo.UpsertAccounts(context.Background(), []Upsert{
		{Token: "basic-active", Pool: "basic", Tags: []string{"seed"}},
		{Token: "basic-nsfw", Pool: "basic", Tags: []string{"nsfw"}},
		{Token: "basic-cooling", Pool: "basic"},
		{Token: "super-disabled", Pool: "super"},
		{Token: "heavy-invalid", Pool: "heavy"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	_, err = repo.PatchAccounts(context.Background(), []Patch{
		{
			Token:         "basic-cooling",
			Status:        testStatusPtr(StatusCooling),
			UsageFailDelta: testIntPtr(1),
		},
		{
			Token:         "super-disabled",
			Status:        testStatusPtr(StatusDisabled),
			UsageUseDelta: testIntPtr(5),
		},
		{
			Token:          "heavy-invalid",
			Status:         testStatusPtr(StatusExpired),
			UsageFailDelta: testIntPtr(2),
		},
		{
			Token:         "basic-nsfw",
			UsageUseDelta: testIntPtr(3),
		},
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}

	page, err := repo.ListAccounts(context.Background(), ListQuery{
		Page:     1,
		PageSize: 10,
		NSFW:     "enabled",
	})
	if err != nil {
		t.Fatalf("list nsfw enabled: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Token != "basic-nsfw" {
		t.Fatalf("unexpected nsfw items: %#v", page.Items)
	}

	page, err = repo.ListAccounts(context.Background(), ListQuery{
		Page:     1,
		PageSize: 10,
		Status:   Status("invalid"),
	})
	if err != nil {
		t.Fatalf("list invalid: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Token != "heavy-invalid" {
		t.Fatalf("unexpected invalid items: %#v", page.Items)
	}

	summary, err := repo.SummarizeAccounts(context.Background(), ListQuery{
		Pool: "basic",
	})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Total != 3 {
		t.Fatalf("expected basic total 3, got %d", summary.Total)
	}
	if summary.Status["all"] != 3 || summary.Status["active"] != 2 || summary.Status["cooling"] != 1 {
		t.Fatalf("unexpected status summary: %#v", summary.Status)
	}
	if summary.NSFW["enabled"] != 1 || summary.NSFW["disabled"] != 2 {
		t.Fatalf("unexpected nsfw summary: %#v", summary.NSFW)
	}
	if summary.Calls != 4 {
		t.Fatalf("unexpected calls summary: %d", summary.Calls)
	}
	if summary.Quota["auto"] <= 0 || summary.Quota["fast"] <= 0 {
		t.Fatalf("unexpected quota summary: %#v", summary.Quota)
	}

	fullSummary, err := repo.SummarizeAccounts(context.Background(), ListQuery{})
	if err != nil {
		t.Fatalf("full summary: %v", err)
	}
	if fullSummary.Pool["basic"] != 3 || fullSummary.Pool["super"] != 1 || fullSummary.Pool["heavy"] != 1 {
		t.Fatalf("unexpected pool summary: %#v", fullSummary.Pool)
	}
	if fullSummary.Status["cooling"] != 1 || fullSummary.Status["disabled"] != 1 || fullSummary.Status["invalid"] != 1 {
		t.Fatalf("unexpected full status summary: %#v", fullSummary.Status)
	}
}
