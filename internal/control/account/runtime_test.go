package account

import (
	"context"
	"testing"

	"github.com/ddmww/grok2api-go/internal/control/model"
	"gorm.io/gorm"
)

type stubRepository struct {
	snapshot []Record
	patches  []Patch
}

func (s *stubRepository) Initialize(context.Context) error           { return nil }
func (s *stubRepository) GetRevision(context.Context) (int64, error) { return 1, nil }
func (s *stubRepository) RuntimeSnapshot(context.Context) ([]Record, int64, error) {
	return s.snapshot, 1, nil
}
func (s *stubRepository) GetAccounts(context.Context, []string) ([]Record, error) { return nil, nil }
func (s *stubRepository) ListAccounts(context.Context, ListQuery) (Page, error)   { return Page{}, nil }
func (s *stubRepository) UpsertAccounts(context.Context, []Upsert) (MutationResult, error) {
	return MutationResult{}, nil
}
func (s *stubRepository) DeleteAccounts(context.Context, []string) (MutationResult, error) {
	return MutationResult{}, nil
}
func (s *stubRepository) ReplacePool(context.Context, string, []Upsert) (MutationResult, error) {
	return MutationResult{}, nil
}
func (s *stubRepository) PatchAccounts(_ context.Context, patches []Patch) (MutationResult, error) {
	s.patches = append(s.patches, patches...)
	return MutationResult{Patched: len(patches)}, nil
}
func (s *stubRepository) Close() error        { return nil }
func (s *stubRepository) StorageType() string { return "local" }
func (s *stubRepository) DB() *gorm.DB        { return nil }

func TestRuntimeReserveAndFeedback(t *testing.T) {
	repo := &stubRepository{
		snapshot: []Record{
			{
				Token:  "token-a",
				Pool:   "basic",
				Status: StatusActive,
				Quota:  DefaultQuotaSet("basic"),
			},
			{
				Token:     "token-b",
				Pool:      "basic",
				Status:    StatusActive,
				LastUseAt: 1,
				Quota:     DefaultQuotaSet("basic"),
			},
		},
	}
	runtime := NewRuntime(repo)
	if err := runtime.Sync(context.Background()); err != nil {
		t.Fatalf("sync runtime: %v", err)
	}

	spec, ok := model.Get("grok-4.20-fast")
	if !ok {
		t.Fatal("model not found")
	}
	lease, err := runtime.Reserve(spec)
	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	if lease.Token != "token-a" {
		t.Fatalf("expected token-a, got %q", lease.Token)
	}

	if err := runtime.ApplyFeedback(context.Background(), lease, Feedback{Kind: FeedbackRateLimited, Reason: "too many requests"}); err != nil {
		t.Fatalf("apply feedback: %v", err)
	}
	if len(repo.patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(repo.patches))
	}
	if repo.patches[0].Status == nil || *repo.patches[0].Status != StatusCooling {
		t.Fatalf("expected cooling patch, got %#v", repo.patches[0].Status)
	}

	nextLease, err := runtime.Reserve(spec)
	if err != nil {
		t.Fatalf("second reserve failed: %v", err)
	}
	if nextLease.Token != "token-b" {
		t.Fatalf("expected token-b after cooldown, got %q", nextLease.Token)
	}
}
