package account

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"

	"github.com/ddmww/grok2api-go/internal/control/model"
)

type Lease struct {
	Token string
	Pool  string
	Mode  string
}

type runtimeItem struct {
	record   Record
	inflight int
}

type Runtime struct {
	mu       sync.RWMutex
	repo     Repository
	items    map[string]*runtimeItem
	revision int64
}

func NewRuntime(repo Repository) *Runtime {
	return &Runtime{repo: repo, items: map[string]*runtimeItem{}}
}

func (r *Runtime) Sync(ctx context.Context) error {
	records, revision, err := r.repo.RuntimeSnapshot(ctx)
	if err != nil {
		return err
	}
	next := make(map[string]*runtimeItem, len(records))
	for _, record := range records {
		inflight := 0
		if existing, ok := r.items[record.Token]; ok {
			inflight = existing.inflight
		}
		next[record.Token] = &runtimeItem{record: CloneRecord(record), inflight: inflight}
	}
	r.mu.Lock()
	r.items = next
	r.revision = revision
	r.mu.Unlock()
	return nil
}

func (r *Runtime) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.items)
}

func (r *Runtime) Revision() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.revision
}

func (r *Runtime) Pools() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := map[string]struct{}{}
	now := NowMS()
	for _, item := range r.items {
		if item.record.EffectiveStatus(now) == StatusActive {
			set[item.record.Pool] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for pool := range set {
		out = append(out, pool)
	}
	sort.Strings(out)
	return out
}

func (r *Runtime) Reserve(spec model.Spec) (*Lease, error) {
	return r.ReserveWithExclude(spec, nil)
}

func (r *Runtime) ReserveWithExclude(spec model.Spec, excluded map[string]struct{}) (*Lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := NowMS()
	for _, pool := range spec.PoolCandidates() {
		var (
			chosen    *runtimeItem
			bestScore       = math.Inf(-1)
			bestUseAt int64 = math.MaxInt64
			bestToken string
		)
		for _, item := range r.items {
			record := &item.record
			if _, skip := excluded[record.Token]; skip {
				continue
			}
			if record.Pool != pool || record.IsDeleted() || record.EffectiveStatus(now) != StatusActive {
				continue
			}
			window := maybeResetQuotaWindow(record, spec.Mode, now)
			if window == nil || window.Remaining <= 0 {
				continue
			}
			score := candidateScore(*record, item.inflight, spec.Mode, now)
			lastUseAt := record.LastUseAt
			if score > bestScore || (score == bestScore && (lastUseAt < bestUseAt || (lastUseAt == bestUseAt && record.Token < bestToken))) {
				chosen = item
				bestScore = score
				bestUseAt = lastUseAt
				bestToken = record.Token
			}
		}
		if chosen == nil {
			continue
		}
		chosen.inflight++
		return &Lease{Token: chosen.record.Token, Pool: chosen.record.Pool, Mode: spec.Mode}, nil
	}
	return nil, errors.New("no available accounts for this model tier")
}

func maybeResetQuotaWindow(record *Record, mode string, now int64) *QuotaWindow {
	if record == nil || NormalizePool(record.Pool) != "basic" {
		return quotaWindowPtr(&record.Quota, mode)
	}
	window := quotaWindowPtr(&record.Quota, mode)
	if window == nil || window.ResetAt == 0 || now < window.ResetAt {
		return window
	}
	if window.Total <= 0 || window.WindowSeconds <= 0 {
		return window
	}
	window.Remaining = window.Total
	window.ResetAt = now + int64(window.WindowSeconds)*1000
	if window.Source == QuotaSourceLocal || window.Source == QuotaSourceDefault {
		window.Source = QuotaSourceLocal
	}
	return window
}

func quotaWindowPtr(quota *QuotaSet, mode string) *QuotaWindow {
	if quota == nil {
		return nil
	}
	switch mode {
	case "fast":
		return &quota.Fast
	case "expert":
		return &quota.Expert
	case "heavy":
		return quota.Heavy
	case "grok-420-computer-use-sa", "grok_4_3":
		return quota.Grok4_3
	default:
		return &quota.Auto
	}
}

func candidateScore(record Record, inflight int, mode string, now int64) float64 {
	window := record.Quota.Window(mode)
	if window == nil || window.Remaining <= 0 {
		return math.Inf(-1)
	}
	score := float64(window.Remaining * 25)
	score -= float64(inflight * 20)
	failCount := record.UsageFailCount
	if failCount > 10 {
		failCount = 10
	}
	score -= float64(failCount * 4)
	if record.LastUseAt > 0 {
		const recentWindowMS = int64(15_000)
		age := now - record.LastUseAt
		if age < 0 {
			age = 0
		}
		if age < recentWindowMS {
			score -= (1 - float64(age)/float64(recentWindowMS)) * 15
		}
	}
	return score
}

func (r *Runtime) Release(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if item, ok := r.items[token]; ok && item.inflight > 0 {
		item.inflight--
	}
}

type Feedback struct {
	Kind   FeedbackKind
	Reason string
}

func (r *Runtime) ApplyFeedback(ctx context.Context, lease *Lease, feedback Feedback) error {
	if lease == nil {
		return nil
	}
	r.mu.Lock()
	item, ok := r.items[lease.Token]
	if !ok {
		r.mu.Unlock()
		return nil
	}
	if item.inflight > 0 {
		item.inflight--
	}
	record := CloneRecord(item.record)
	now := NowMS()
	quotaPatch := map[string]QuotaWindow{}
	patch := Patch{Token: lease.Token, Quota: quotaPatch, ExtMerge: map[string]any{}}
	switch feedback.Kind {
	case FeedbackSuccess:
		win := record.Quota.Window(lease.Mode)
		if win != nil && win.Remaining > 0 {
			win.Remaining--
			win.Source = QuotaSourceLocal
			quotaPatch[lease.Mode] = *win
		}
		record.UsageUseCount++
		record.LastUseAt = now
		patch.UsageUseDelta = ptrInt(1)
		patch.LastUseAt = ptrInt64(now)
		record.Status = StatusActive
	case FeedbackRateLimited:
		win := record.Quota.Window(lease.Mode)
		if win != nil {
			win.Remaining = 0
			if win.ResetAt == 0 {
				win.ResetAt = now + int64(win.WindowSeconds)*1000
			}
			quotaPatch[lease.Mode] = *win
			patch.ExtMerge["cooldown_until"] = win.ResetAt
		}
		record.Status = StatusCooling
		record.StateReason = "rate_limited"
		record.LastFailAt = now
		record.LastFailReason = feedback.Reason
		patch.Status = ptrStatus(StatusCooling)
		patch.StateReason = ptrString(record.StateReason)
		patch.LastFailAt = ptrInt64(now)
		patch.LastFailReason = ptrString(feedback.Reason)
		patch.UsageFailDelta = ptrInt(1)
	case FeedbackUnauthorized:
		record.Status = StatusExpired
		record.StateReason = "token_expired"
		record.LastFailAt = now
		record.LastFailReason = feedback.Reason
		patch.Status = ptrStatus(StatusExpired)
		patch.StateReason = ptrString(record.StateReason)
		patch.LastFailAt = ptrInt64(now)
		patch.LastFailReason = ptrString(feedback.Reason)
		patch.ExtMerge["expired_at"] = now
		patch.ExtMerge["expired_reason"] = feedback.Reason
		patch.UsageFailDelta = ptrInt(1)
	case FeedbackForbidden:
		record.Status = StatusDisabled
		record.StateReason = "forbidden"
		record.LastFailAt = now
		record.LastFailReason = feedback.Reason
		patch.Status = ptrStatus(StatusDisabled)
		patch.StateReason = ptrString(record.StateReason)
		patch.LastFailAt = ptrInt64(now)
		patch.LastFailReason = ptrString(feedback.Reason)
		patch.ExtMerge["disabled_at"] = now
		patch.ExtMerge["disabled_reason"] = feedback.Reason
		patch.UsageFailDelta = ptrInt(1)
	default:
		record.UsageFailCount++
		record.LastFailAt = now
		record.LastFailReason = feedback.Reason
		patch.LastFailAt = ptrInt64(now)
		patch.LastFailReason = ptrString(feedback.Reason)
		patch.UsageFailDelta = ptrInt(1)
	}
	item.record = record
	r.mu.Unlock()
	_, err := r.repo.PatchAccounts(ctx, []Patch{patch})
	return err
}

func (r *Runtime) ReplaceRecord(record Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.items[record.Token]; ok {
		existing.record = CloneRecord(record)
		return
	}
	r.items[record.Token] = &runtimeItem{record: CloneRecord(record)}
}

func (r *Runtime) TokensByPool(pool string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []string{}
	now := NowMS()
	for _, item := range r.items {
		if item.record.Pool == NormalizePool(pool) && !item.record.IsDeleted() && item.record.EffectiveStatus(now) != StatusDisabled {
			out = append(out, item.record.Token)
		}
	}
	sort.Strings(out)
	return out
}

func ptrInt(value int) *int          { return &value }
func ptrInt64(value int64) *int64    { return &value }
func ptrString(value string) *string { return &value }
func ptrStatus(value Status) *Status { return &value }
