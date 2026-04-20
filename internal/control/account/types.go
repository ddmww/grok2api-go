package account

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
)

type Status string

const (
	StatusActive   Status = "active"
	StatusCooling  Status = "cooling"
	StatusExpired  Status = "expired"
	StatusDisabled Status = "disabled"
)

type FeedbackKind string

const (
	FeedbackSuccess      FeedbackKind = "success"
	FeedbackUnauthorized FeedbackKind = "unauthorized"
	FeedbackForbidden    FeedbackKind = "forbidden"
	FeedbackRateLimited  FeedbackKind = "rate_limited"
	FeedbackServerError  FeedbackKind = "server_error"
)

type QuotaSource int

const (
	QuotaSourceDefault QuotaSource = 0
	QuotaSourceReal    QuotaSource = 1
	QuotaSourceLocal   QuotaSource = 2
)

type QuotaWindow struct {
	Remaining     int         `json:"remaining"`
	Total         int         `json:"total"`
	WindowSeconds int         `json:"window_seconds"`
	ResetAt       int64       `json:"reset_at,omitempty"`
	SyncedAt      int64       `json:"synced_at,omitempty"`
	Source        QuotaSource `json:"source"`
}

func (q QuotaWindow) Clone() QuotaWindow { return q }

type QuotaSet struct {
	Auto    QuotaWindow  `json:"auto"`
	Fast    QuotaWindow  `json:"fast"`
	Expert  QuotaWindow  `json:"expert"`
	Heavy   *QuotaWindow `json:"heavy,omitempty"`
	Grok4_3 *QuotaWindow `json:"grok_4_3,omitempty"`
}

func (q QuotaSet) ToMap() map[string]QuotaWindow {
	out := map[string]QuotaWindow{
		"auto":   q.Auto,
		"fast":   q.Fast,
		"expert": q.Expert,
	}
	if q.Heavy != nil {
		out["heavy"] = *q.Heavy
	}
	if q.Grok4_3 != nil {
		out["grok_4_3"] = *q.Grok4_3
	}
	return out
}

func (q QuotaSet) Window(mode string) *QuotaWindow {
	switch mode {
	case "fast":
		return &q.Fast
	case "expert":
		return &q.Expert
	case "heavy":
		return q.Heavy
	case "grok-420-computer-use-sa", "grok_4_3":
		return q.Grok4_3
	default:
		return &q.Auto
	}
}

func normalizeToken(raw string) string {
	replacer := strings.NewReplacer(
		"\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-", "\u2014", "-", "\u2212", "-",
		"\u00a0", " ", "\u2007", " ", "\u202f", " ", "\u200b", "", "\u200c", "", "\u200d", "", "\ufeff", "",
	)
	value := strings.TrimSpace(replacer.Replace(raw))
	value = strings.Join(strings.Fields(value), "")
	if strings.HasPrefix(value, "sso=") {
		value = strings.TrimPrefix(value, "sso=")
	}
	value = strings.TrimSpace(value)
	return value
}

func NormalizeToken(raw string) string {
	return normalizeToken(raw)
}

func NormalizePool(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "super":
		return "super"
	case "heavy":
		return "heavy"
	default:
		return "basic"
	}
}

func NormalizeTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		text := strings.TrimSpace(tag)
		if text != "" && !slices.Contains(out, text) {
			out = append(out, text)
		}
	}
	return out
}

type Record struct {
	Token          string         `json:"token"`
	Pool           string         `json:"pool"`
	Status         Status         `json:"status"`
	CreatedAt      int64          `json:"created_at"`
	UpdatedAt      int64          `json:"updated_at"`
	Tags           []string       `json:"tags"`
	Quota          QuotaSet       `json:"quota"`
	UsageUseCount  int            `json:"usage_use_count"`
	UsageFailCount int            `json:"usage_fail_count"`
	UsageSyncCount int            `json:"usage_sync_count"`
	LastUseAt      int64          `json:"last_use_at,omitempty"`
	LastFailAt     int64          `json:"last_fail_at,omitempty"`
	LastFailReason string         `json:"last_fail_reason,omitempty"`
	LastSyncAt     int64          `json:"last_sync_at,omitempty"`
	LastClearAt    int64          `json:"last_clear_at,omitempty"`
	StateReason    string         `json:"state_reason,omitempty"`
	DeletedAt      int64          `json:"deleted_at,omitempty"`
	Ext            map[string]any `json:"ext"`
	Revision       int64          `json:"revision"`
}

func (r Record) IsDeleted() bool { return r.DeletedAt > 0 }

func (r Record) EffectiveStatus(now int64) Status {
	if r.Status != StatusCooling {
		return r.Status
	}
	cooldown, ok := r.Ext["cooldown_until"]
	if !ok {
		return r.Status
	}
	switch v := cooldown.(type) {
	case int64:
		if now >= v {
			return StatusActive
		}
	case float64:
		if now >= int64(v) {
			return StatusActive
		}
	}
	return r.Status
}

func (r Record) JSONQuota(mode string) map[string]any {
	win := r.Quota.Window(mode)
	if win == nil {
		return nil
	}
	return map[string]any{
		"remaining":      win.Remaining,
		"total":          win.Total,
		"window_seconds": win.WindowSeconds,
		"reset_at":       win.ResetAt,
		"synced_at":      win.SyncedAt,
		"source":         win.Source,
	}
}

func DefaultQuotaSet(pool string) QuotaSet {
	makeWindow := func(remaining, total, seconds int) QuotaWindow {
		return QuotaWindow{Remaining: remaining, Total: total, WindowSeconds: seconds, Source: QuotaSourceDefault}
	}
	switch NormalizePool(pool) {
	case "super":
		grok43 := makeWindow(50, 50, 7200)
		return QuotaSet{
			Auto:    makeWindow(50, 50, 7200),
			Fast:    makeWindow(140, 140, 7200),
			Expert:  makeWindow(50, 50, 7200),
			Grok4_3: &grok43,
		}
	case "heavy":
		heavy := makeWindow(20, 20, 7200)
		grok43 := makeWindow(150, 150, 7200)
		return QuotaSet{
			Auto:    makeWindow(150, 150, 7200),
			Fast:    makeWindow(400, 400, 7200),
			Expert:  makeWindow(150, 150, 7200),
			Heavy:   &heavy,
			Grok4_3: &grok43,
		}
	default:
		return QuotaSet{
			Auto:   makeWindow(20, 20, 72000),
			Fast:   makeWindow(60, 60, 72000),
			Expert: makeWindow(8, 8, 36000),
		}
	}
}

func SupportedModes(pool string) []string {
	switch NormalizePool(pool) {
	case "heavy":
		return []string{"auto", "fast", "expert", "heavy", "grok-420-computer-use-sa"}
	case "super":
		return []string{"auto", "fast", "expert", "grok-420-computer-use-sa"}
	default:
		return []string{"auto", "fast", "expert"}
	}
}

func InferPool(quota map[string]QuotaWindow) string {
	if win, ok := quota["auto"]; ok {
		switch win.Total {
		case 50:
			return "super"
		case 150:
			return "heavy"
		}
	}
	return "basic"
}

func CloneRecord(record Record) Record {
	copyRecord := record
	copyRecord.Tags = append([]string(nil), record.Tags...)
	copyRecord.Ext = map[string]any{}
	for key, value := range record.Ext {
		copyRecord.Ext[key] = value
	}
	if record.Quota.Heavy != nil {
		heavy := record.Quota.Heavy.Clone()
		copyRecord.Quota.Heavy = &heavy
	}
	if record.Quota.Grok4_3 != nil {
		grok43 := record.Quota.Grok4_3.Clone()
		copyRecord.Quota.Grok4_3 = &grok43
	}
	return copyRecord
}

func MarshalJSONMap(values map[string]any) string {
	if values == nil {
		return "{}"
	}
	data, err := json.Marshal(values)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func NowMS() int64 {
	return time.Now().UnixMilli()
}

type Upsert struct {
	Token string
	Pool  string
	Tags  []string
	Ext   map[string]any
}

type Patch struct {
	Token          string
	Pool           *string
	Status         *Status
	Tags           []string
	AddTags        []string
	RemoveTags     []string
	Quota          map[string]QuotaWindow
	UsageUseDelta  *int
	UsageFailDelta *int
	UsageSyncDelta *int
	LastUseAt      *int64
	LastFailAt     *int64
	LastFailReason *string
	LastSyncAt     *int64
	LastClearAt    *int64
	StateReason    *string
	ExtMerge       map[string]any
	ClearFailures  bool
}

type ListQuery struct {
	Page           int
	PageSize       int
	Pool           string
	Status         Status
	Tags           []string
	NSFW           string
	IncludeDeleted bool
	SortBy         string
	SortDesc       bool
}

type Page struct {
	Items      []Record `json:"items"`
	Total      int64    `json:"total"`
	Page       int      `json:"page"`
	PageSize   int      `json:"page_size"`
	TotalPages int      `json:"total_pages"`
	Revision   int64    `json:"revision"`
}

type Summary struct {
	Total      int64            `json:"total"`
	Status     map[string]int64 `json:"status"`
	Pool       map[string]int64 `json:"pool"`
	NSFW       map[string]int64 `json:"nsfw"`
	Calls      int64            `json:"calls"`
	Quota      map[string]int64 `json:"quota"`
	Revision   int64            `json:"revision"`
	FilteredBy map[string]any   `json:"filtered_by,omitempty"`
}

type MutationResult struct {
	Upserted int   `json:"upserted"`
	Patched  int   `json:"patched"`
	Deleted  int   `json:"deleted"`
	Revision int64 `json:"revision"`
}
