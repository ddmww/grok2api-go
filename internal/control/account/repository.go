package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ddmww/grok2api-go/internal/platform/paths"
	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository interface {
	Initialize(context.Context) error
	GetRevision(context.Context) (int64, error)
	RuntimeSnapshot(context.Context) ([]Record, int64, error)
	GetAccounts(context.Context, []string) ([]Record, error)
	ListAccounts(context.Context, ListQuery) (Page, error)
	SummarizeAccounts(context.Context, ListQuery) (Summary, error)
	UpsertAccounts(context.Context, []Upsert) (MutationResult, error)
	PatchAccounts(context.Context, []Patch) (MutationResult, error)
	DeleteAccounts(context.Context, []string) (MutationResult, error)
	ReplacePool(context.Context, string, []Upsert) (MutationResult, error)
	Close() error
	StorageType() string
	DB() *gorm.DB
}

type metaEntity struct {
	MetaKey string `gorm:"column:meta_key;primaryKey;size:64"`
	Value   int64  `gorm:"column:value"`
}

type legacyMetaEntity struct {
	Key   string `gorm:"column:key;primaryKey;size:64"`
	Value int64  `gorm:"column:value"`
}

type accountEntity struct {
	Token           string `gorm:"primaryKey;size:512"`
	Pool            string `gorm:"index:idx_accounts_pool"`
	Status          string `gorm:"index:idx_accounts_status"`
	CreatedAt       int64  `gorm:"column:created_at;index:idx_accounts_created_at"`
	UpdatedAt       int64  `gorm:"column:updated_at;index:idx_accounts_updated_at"`
	TagsJSON        string `gorm:"column:tags"`
	QuotaAutoJSON   string `gorm:"column:quota_auto"`
	QuotaFastJSON   string `gorm:"column:quota_fast"`
	QuotaExpertJSON string `gorm:"column:quota_expert"`
	QuotaHeavyJSON  string `gorm:"column:quota_heavy"`
	QuotaGrok43JSON string `gorm:"column:quota_grok_4_3"`
	UsageUseCount   int    `gorm:"column:usage_use_count"`
	UsageFailCount  int    `gorm:"column:usage_fail_count"`
	UsageSyncCount  int    `gorm:"column:usage_sync_count"`
	LastUseAt       int64  `gorm:"column:last_use_at"`
	LastFailAt      int64  `gorm:"column:last_fail_at"`
	LastFailReason  string `gorm:"column:last_fail_reason"`
	LastSyncAt      int64  `gorm:"column:last_sync_at"`
	LastClearAt     int64  `gorm:"column:last_clear_at"`
	StateReason     string `gorm:"column:state_reason"`
	DeletedAt       int64  `gorm:"column:deleted_at;index:idx_accounts_deleted_at"`
	ExtJSON         string `gorm:"column:ext"`
	Revision        int64
}

type gormRepository struct {
	db          *gorm.DB
	sqlDB       *gorm.DB
	storageType string
}

func (accountEntity) TableName() string { return "accounts" }
func (metaEntity) TableName() string    { return "account_meta" }
func (legacyMetaEntity) TableName() string {
	return "account_meta"
}

func NewRepositoryFromEnv() (Repository, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ACCOUNT_STORAGE"))) {
	case "", "local":
		dbPath := paths.LocalAccountPath()
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return nil, err
		}
		dsn := dbPath + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
		if err != nil {
			return nil, err
		}
		sqlDB, err := db.DB()
		if err != nil {
			return nil, err
		}
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
		sqlDB.SetConnMaxLifetime(0)
		return &gormRepository{db: db, storageType: "local"}, nil
	case "mysql":
		dsn := os.Getenv("ACCOUNT_MYSQL_URL")
		if strings.TrimSpace(dsn) == "" {
			return nil, errors.New("ACCOUNT_MYSQL_URL is required when ACCOUNT_STORAGE=mysql")
		}
		db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
		if err != nil {
			return nil, err
		}
		return &gormRepository{db: db, storageType: "mysql"}, nil
	default:
		return nil, fmt.Errorf("unsupported ACCOUNT_STORAGE %q", os.Getenv("ACCOUNT_STORAGE"))
	}
}

func (r *gormRepository) StorageType() string { return r.storageType }

func (r *gormRepository) DB() *gorm.DB { return r.db }

func (r *gormRepository) Initialize(ctx context.Context) error {
	if err := r.migrateMetaSchema(ctx); err != nil {
		return err
	}
	if err := r.db.WithContext(ctx).AutoMigrate(&accountEntity{}, &metaEntity{}); err != nil {
		return err
	}
	var count int64
	if err := r.db.WithContext(ctx).Model(&metaEntity{}).Where("meta_key = ?", "revision").Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		if err := r.db.WithContext(ctx).Create(&metaEntity{MetaKey: "revision", Value: 0}).Error; err != nil {
			return err
		}
	}
	if r.storageType == "mysql" {
		if err := r.migrateLocalSQLite(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *gormRepository) Close() error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func (r *gormRepository) GetRevision(ctx context.Context) (int64, error) {
	var meta metaEntity
	if err := r.db.WithContext(ctx).First(&meta, "meta_key = ?", "revision").Error; err != nil {
		return 0, err
	}
	return meta.Value, nil
}

func (r *gormRepository) bumpRevision(tx *gorm.DB) (int64, error) {
	var meta metaEntity
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&meta, "meta_key = ?", "revision").Error; err != nil {
		return 0, err
	}
	meta.Value++
	if err := tx.Save(&meta).Error; err != nil {
		return 0, err
	}
	return meta.Value, nil
}

func entityToRecord(entity accountEntity) Record {
	record := Record{
		Token:          entity.Token,
		Pool:           NormalizePool(entity.Pool),
		Status:         Status(entity.Status),
		CreatedAt:      entity.CreatedAt,
		UpdatedAt:      entity.UpdatedAt,
		UsageUseCount:  entity.UsageUseCount,
		UsageFailCount: entity.UsageFailCount,
		UsageSyncCount: entity.UsageSyncCount,
		LastUseAt:      entity.LastUseAt,
		LastFailAt:     entity.LastFailAt,
		LastFailReason: entity.LastFailReason,
		LastSyncAt:     entity.LastSyncAt,
		LastClearAt:    entity.LastClearAt,
		StateReason:    entity.StateReason,
		DeletedAt:      entity.DeletedAt,
		Ext:            map[string]any{},
		Revision:       entity.Revision,
	}
	_ = json.Unmarshal([]byte(entity.TagsJSON), &record.Tags)
	_ = json.Unmarshal([]byte(entity.ExtJSON), &record.Ext)
	record.Tags = NormalizeTags(record.Tags)
	record.Quota = DefaultQuotaSet(record.Pool)
	_ = json.Unmarshal([]byte(entity.QuotaAutoJSON), &record.Quota.Auto)
	_ = json.Unmarshal([]byte(entity.QuotaFastJSON), &record.Quota.Fast)
	_ = json.Unmarshal([]byte(entity.QuotaExpertJSON), &record.Quota.Expert)
	if strings.TrimSpace(entity.QuotaHeavyJSON) != "" && strings.TrimSpace(entity.QuotaHeavyJSON) != "{}" {
		var heavy QuotaWindow
		if err := json.Unmarshal([]byte(entity.QuotaHeavyJSON), &heavy); err == nil {
			record.Quota.Heavy = &heavy
		}
	}
	if strings.TrimSpace(entity.QuotaGrok43JSON) != "" && strings.TrimSpace(entity.QuotaGrok43JSON) != "{}" {
		var grok43 QuotaWindow
		if err := json.Unmarshal([]byte(entity.QuotaGrok43JSON), &grok43); err == nil {
			record.Quota.Grok4_3 = &grok43
		}
	}
	return record
}

func recordToEntity(record Record) accountEntity {
	return accountEntity{
		Token:           record.Token,
		Pool:            record.Pool,
		Status:          string(record.Status),
		CreatedAt:       record.CreatedAt,
		UpdatedAt:       record.UpdatedAt,
		TagsJSON:        tagsJSON(record.Tags),
		QuotaAutoJSON:   mustJSON(record.Quota.Auto),
		QuotaFastJSON:   mustJSON(record.Quota.Fast),
		QuotaExpertJSON: mustJSON(record.Quota.Expert),
		QuotaHeavyJSON:  mustJSON(record.Quota.Heavy),
		QuotaGrok43JSON: mustJSON(record.Quota.Grok4_3),
		UsageUseCount:   record.UsageUseCount,
		UsageFailCount:  record.UsageFailCount,
		UsageSyncCount:  record.UsageSyncCount,
		LastUseAt:       record.LastUseAt,
		LastFailAt:      record.LastFailAt,
		LastFailReason:  record.LastFailReason,
		LastSyncAt:      record.LastSyncAt,
		LastClearAt:     record.LastClearAt,
		StateReason:     record.StateReason,
		DeletedAt:       record.DeletedAt,
		ExtJSON:         mustJSON(record.Ext),
		Revision:        record.Revision,
	}
}

func mustJSON(value any) string {
	if value == nil {
		return "{}"
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func tagsJSON(tags []string) string {
	data, err := json.Marshal(NormalizeTags(tags))
	if err != nil {
		return "[]"
	}
	return string(data)
}

func (r *gormRepository) RuntimeSnapshot(ctx context.Context) ([]Record, int64, error) {
	var entities []accountEntity
	if err := r.db.WithContext(ctx).Where("deleted_at = 0").Find(&entities).Error; err != nil {
		return nil, 0, err
	}
	revision, err := r.GetRevision(ctx)
	if err != nil {
		return nil, 0, err
	}
	records := make([]Record, 0, len(entities))
	for _, entity := range entities {
		records = append(records, entityToRecord(entity))
	}
	return records, revision, nil
}

func (r *gormRepository) GetAccounts(ctx context.Context, tokens []string) ([]Record, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	clean := make([]string, 0, len(tokens))
	for _, token := range tokens {
		text := normalizeToken(token)
		if text != "" {
			clean = append(clean, text)
		}
	}
	var entities []accountEntity
	if err := r.db.WithContext(ctx).Where("token IN ?", clean).Find(&entities).Error; err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(entities))
	for _, entity := range entities {
		out = append(out, entityToRecord(entity))
	}
	return out, nil
}

func (r *gormRepository) ListAccounts(ctx context.Context, query ListQuery) (Page, error) {
	if query.Page <= 0 {
		query.Page = 1
	}
	if query.PageSize <= 0 {
		query.PageSize = 50
	}
	if query.PageSize > 2000 {
		query.PageSize = 2000
	}
	db := r.db.WithContext(ctx).Model(&accountEntity{})
	db = applyListQuery(db, query)
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return Page{}, err
	}
	sortBy := "created_at"
	if strings.TrimSpace(query.SortBy) != "" {
		sortBy = strings.TrimSpace(query.SortBy)
	}
	var entities []accountEntity
	db = applySortOrder(db, sortBy, query.SortDesc)
	if err := db.Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Find(&entities).Error; err != nil {
		return Page{}, err
	}
	revision, err := r.GetRevision(ctx)
	if err != nil {
		return Page{}, err
	}
	items := make([]Record, 0, len(entities))
	for _, entity := range entities {
		items = append(items, entityToRecord(entity))
	}
	totalPages := int((total + int64(query.PageSize) - 1) / int64(query.PageSize))
	if totalPages == 0 {
		totalPages = 1
	}
	return Page{Items: items, Total: total, Page: query.Page, PageSize: query.PageSize, TotalPages: totalPages, Revision: revision}, nil
}

func (r *gormRepository) SummarizeAccounts(ctx context.Context, query ListQuery) (Summary, error) {
	newScope := func(q ListQuery) *gorm.DB {
		return applyListQuery(r.db.WithContext(ctx).Model(&accountEntity{}), q)
	}

	revision, err := r.GetRevision(ctx)
	if err != nil {
		return Summary{}, err
	}

	summary := Summary{
		Status:   map[string]int64{"all": 0, "active": 0, "cooling": 0, "invalid": 0, "disabled": 0},
		Pool:     map[string]int64{"all": 0, "basic": 0, "super": 0, "heavy": 0},
		NSFW:     map[string]int64{"all": 0, "enabled": 0, "disabled": 0},
		Quota:    map[string]int64{"auto": 0, "fast": 0, "expert": 0, "heavy": 0},
		Revision: revision,
		FilteredBy: map[string]any{
			"pool":   query.Pool,
			"status": string(query.Status),
			"nsfw":   query.NSFW,
		},
	}

	type totalsRow struct {
		Total  int64
		Calls  int64
		Auto   int64
		Fast   int64
		Expert int64
		Heavy  int64
	}
	var totals totalsRow
	filtered := newScope(query)
	if err := filtered.Select(strings.Join([]string{
		"COUNT(*) AS total",
		"COALESCE(SUM(usage_use_count + usage_fail_count), 0) AS calls",
		"COALESCE(SUM(" + quotaRemainingExpr(r.storageType, "quota_auto") + "), 0) AS auto",
		"COALESCE(SUM(" + quotaRemainingExpr(r.storageType, "quota_fast") + "), 0) AS fast",
		"COALESCE(SUM(" + quotaRemainingExpr(r.storageType, "quota_expert") + "), 0) AS expert",
		"COALESCE(SUM(" + quotaRemainingExpr(r.storageType, "quota_heavy") + "), 0) AS heavy",
	}, ", ")).Scan(&totals).Error; err != nil {
		return Summary{}, err
	}
	summary.Total = totals.Total
	summary.Calls = totals.Calls
	summary.Quota["auto"] = totals.Auto
	summary.Quota["fast"] = totals.Fast
	summary.Quota["expert"] = totals.Expert
	summary.Quota["heavy"] = totals.Heavy

	type groupedCount struct {
		Key   string
		Count int64
	}
	var statusRows []groupedCount
	if err := newScope(queryWithoutStatus(query)).Select("status AS key, COUNT(*) AS count").Group("status").Scan(&statusRows).Error; err != nil {
		return Summary{}, err
	}
	for _, row := range statusRows {
		summary.Status["all"] += row.Count
		switch row.Key {
		case string(StatusActive):
			summary.Status["active"] = row.Count
		case string(StatusCooling):
			summary.Status["cooling"] = row.Count
		case string(StatusDisabled):
			summary.Status["disabled"] = row.Count
		default:
			summary.Status["invalid"] += row.Count
		}
	}

	var poolRows []groupedCount
	if err := newScope(queryWithoutPool(query)).Select("pool AS key, COUNT(*) AS count").Group("pool").Scan(&poolRows).Error; err != nil {
		return Summary{}, err
	}
	for _, row := range poolRows {
		summary.Pool["all"] += row.Count
		if _, ok := summary.Pool[row.Key]; ok {
			summary.Pool[row.Key] = row.Count
		}
	}

	type nsfwRow struct {
		Total   int64
		Enabled int64
	}
	var nsfw nsfwRow
	if err := newScope(queryWithoutNSFW(query)).
		Select("COUNT(*) AS total, COALESCE(SUM(CASE WHEN "+tagsContainsClause(r.storageType)+" THEN 1 ELSE 0 END), 0) AS enabled", tagsLikePattern("nsfw")).
		Scan(&nsfw).Error; err != nil {
		return Summary{}, err
	}
	summary.NSFW["all"] = nsfw.Total
	summary.NSFW["enabled"] = nsfw.Enabled
	summary.NSFW["disabled"] = nsfw.Total - nsfw.Enabled
	if summary.NSFW["disabled"] < 0 {
		summary.NSFW["disabled"] = 0
	}

	return summary, nil
}

func applySortOrder(db *gorm.DB, sortBy string, sortDesc bool) *gorm.DB {
	dir := "asc"
	if sortDesc {
		dir = "desc"
	}
	switch sortBy {
	case "updated_at":
		return db.Order("updated_at " + dir).Order("token asc")
	case "token":
		return db.Order("token " + dir)
	default:
		return db.Order("created_at " + dir).Order("token asc")
	}
}

func applyListQuery(db *gorm.DB, query ListQuery) *gorm.DB {
	if !query.IncludeDeleted {
		db = db.Where("deleted_at = 0")
	}
	if pool := NormalizePool(query.Pool); query.Pool != "" {
		db = db.Where("pool = ?", pool)
	}
	switch query.Status {
	case "":
	case Status("invalid"):
		db = db.Where("status NOT IN ?", []string{string(StatusActive), string(StatusCooling), string(StatusDisabled)})
	default:
		db = db.Where("status = ?", string(query.Status))
	}
	for _, tag := range NormalizeTags(query.Tags) {
		db = db.Where(tagsContainsClause(storageTypeOf(db)), tagsLikePattern(tag))
	}
	switch strings.ToLower(strings.TrimSpace(query.NSFW)) {
	case "enabled":
		db = db.Where(tagsContainsClause(storageTypeOf(db)), tagsLikePattern("nsfw"))
	case "disabled":
		db = db.Where(tagsNotContainsClause(storageTypeOf(db)), tagsLikePattern("nsfw"))
	}
	return db
}

func queryWithoutStatus(query ListQuery) ListQuery {
	query.Status = ""
	return query
}

func queryWithoutPool(query ListQuery) ListQuery {
	query.Pool = ""
	return query
}

func queryWithoutNSFW(query ListQuery) ListQuery {
	query.NSFW = ""
	return query
}

func tagsContainsClause(storageType string) string {
	switch storageType {
	case "mysql":
		return "tags LIKE ? ESCAPE '\\\\'"
	default:
		return `tags LIKE ? ESCAPE '\'`
	}
}

func tagsNotContainsClause(storageType string) string {
	switch storageType {
	case "mysql":
		return "(tags NOT LIKE ? ESCAPE '\\\\' OR tags IS NULL OR tags = '')"
	default:
		return `(tags NOT LIKE ? ESCAPE '\' OR tags IS NULL OR tags = '')`
	}
}

func tagsLikePattern(tag string) string {
	return "%" + escapeLike(`"`+tag+`"`) + "%"
}

func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

func storageTypeOf(db *gorm.DB) string {
	if db == nil || db.Dialector == nil {
		return ""
	}
	return db.Dialector.Name()
}

func quotaRemainingExpr(storageType, column string) string {
	switch storageType {
	case "mysql":
		return fmt.Sprintf(
			`CAST(CASE WHEN %s IS NULL OR %s = '' OR JSON_VALID(%s) = 0 THEN '0' ELSE COALESCE(JSON_UNQUOTE(JSON_EXTRACT(%s, '$.remaining')), '0') END AS UNSIGNED)`,
			column,
			column,
			column,
			column,
		)
	default:
		return fmt.Sprintf(`CAST(COALESCE(json_extract(%s, '$.remaining'), 0) AS INTEGER)`, column)
	}
}

func (r *gormRepository) UpsertAccounts(ctx context.Context, items []Upsert) (MutationResult, error) {
	if len(items) == 0 {
		return MutationResult{}, nil
	}
	result := MutationResult{}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		revision, err := r.bumpRevision(tx)
		if err != nil {
			return err
		}
		now := NowMS()
		for _, item := range items {
			token := normalizeToken(item.Token)
			if token == "" {
				continue
			}
			pool := NormalizePool(item.Pool)
			quota := DefaultQuotaSet(pool)
			entity := accountEntity{
				Token:           token,
				Pool:            pool,
				Status:          string(StatusActive),
				CreatedAt:       now,
				UpdatedAt:       now,
				TagsJSON:        tagsJSON(item.Tags),
				QuotaAutoJSON:   mustJSON(quota.Auto),
				QuotaFastJSON:   mustJSON(quota.Fast),
				QuotaExpertJSON: mustJSON(quota.Expert),
				QuotaHeavyJSON:  mustJSON(quota.Heavy),
				QuotaGrok43JSON: mustJSON(quota.Grok4_3),
				ExtJSON:         mustJSON(item.Ext),
				Revision:        revision,
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "token"}},
				DoUpdates: clause.Assignments(map[string]any{
					"pool":       entity.Pool,
					"status":     entity.Status,
					"updated_at": entity.UpdatedAt,
					"tags":       entity.TagsJSON,
					"ext":        entity.ExtJSON,
					"deleted_at": 0,
					"revision":   revision,
				}),
			}).Create(&entity).Error; err != nil {
				return err
			}
			result.Upserted++
		}
		result.Revision = revision
		return nil
	})
	return result, err
}

func (r *gormRepository) PatchAccounts(ctx context.Context, patches []Patch) (MutationResult, error) {
	if len(patches) == 0 {
		return MutationResult{}, nil
	}
	result := MutationResult{}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		revision, err := r.bumpRevision(tx)
		if err != nil {
			return err
		}
		for _, patch := range patches {
			token := normalizeToken(patch.Token)
			if token == "" {
				continue
			}
			var entity accountEntity
			if err := tx.First(&entity, "token = ?", token).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return err
			}
			record := entityToRecord(entity)
			updated := CloneRecord(record)
			updated.UpdatedAt = NowMS()
			if patch.Pool != nil {
				updated.Pool = NormalizePool(*patch.Pool)
			}
			if patch.Status != nil {
				updated.Status = *patch.Status
			}
			if patch.StateReason != nil {
				updated.StateReason = *patch.StateReason
			}
			if patch.LastUseAt != nil {
				updated.LastUseAt = *patch.LastUseAt
			}
			if patch.LastFailAt != nil {
				updated.LastFailAt = *patch.LastFailAt
			}
			if patch.LastFailReason != nil {
				updated.LastFailReason = *patch.LastFailReason
			}
			if patch.LastSyncAt != nil {
				updated.LastSyncAt = *patch.LastSyncAt
			}
			if patch.LastClearAt != nil {
				updated.LastClearAt = *patch.LastClearAt
			}
			if patch.UsageUseDelta != nil {
				updated.UsageUseCount += *patch.UsageUseDelta
			}
			if patch.UsageFailDelta != nil {
				updated.UsageFailCount += *patch.UsageFailDelta
			}
			if patch.UsageSyncDelta != nil {
				updated.UsageSyncCount += *patch.UsageSyncDelta
			}
			if patch.Tags != nil {
				updated.Tags = NormalizeTags(patch.Tags)
			}
			if len(patch.AddTags) > 0 {
				updated.Tags = NormalizeTags(append(updated.Tags, patch.AddTags...))
			}
			if len(patch.RemoveTags) > 0 {
				filtered := make([]string, 0, len(updated.Tags))
				for _, tag := range updated.Tags {
					if !contains(patch.RemoveTags, tag) {
						filtered = append(filtered, tag)
					}
				}
				updated.Tags = filtered
			}
			for mode, quota := range patch.Quota {
				switch mode {
				case "auto":
					updated.Quota.Auto = quota
				case "fast":
					updated.Quota.Fast = quota
				case "expert":
					updated.Quota.Expert = quota
				case "heavy":
					q := quota
					updated.Quota.Heavy = &q
				case "grok-420-computer-use-sa", "grok_4_3":
					q := quota
					updated.Quota.Grok4_3 = &q
				}
			}
			if updated.Ext == nil {
				updated.Ext = map[string]any{}
			}
			for key, value := range patch.ExtMerge {
				updated.Ext[key] = value
			}
			if patch.ClearFailures {
				delete(updated.Ext, "cooldown_until")
				delete(updated.Ext, "cooldown_reason")
				delete(updated.Ext, "disabled_at")
				delete(updated.Ext, "disabled_reason")
				delete(updated.Ext, "expired_at")
				delete(updated.Ext, "expired_reason")
				updated.Status = StatusActive
				updated.StateReason = ""
				updated.UsageFailCount = 0
				updated.LastFailAt = 0
				updated.LastFailReason = ""
			}
			updated.Revision = revision
			next := recordToEntity(updated)
			next.CreatedAt = entity.CreatedAt
			next.DeletedAt = entity.DeletedAt
			next.TagsJSON = tagsJSON(updated.Tags)
			if err := tx.Model(&accountEntity{}).Where("token = ?", token).Updates(map[string]any{
				"pool":             next.Pool,
				"status":           next.Status,
				"updated_at":       next.UpdatedAt,
				"tags":             next.TagsJSON,
				"quota_auto":       next.QuotaAutoJSON,
				"quota_fast":       next.QuotaFastJSON,
				"quota_expert":     next.QuotaExpertJSON,
				"quota_heavy":      next.QuotaHeavyJSON,
				"quota_grok_4_3":   next.QuotaGrok43JSON,
				"usage_use_count":  next.UsageUseCount,
				"usage_fail_count": next.UsageFailCount,
				"usage_sync_count": next.UsageSyncCount,
				"last_use_at":      next.LastUseAt,
				"last_fail_at":     next.LastFailAt,
				"last_fail_reason": next.LastFailReason,
				"last_sync_at":     next.LastSyncAt,
				"last_clear_at":    next.LastClearAt,
				"state_reason":     next.StateReason,
				"ext":              next.ExtJSON,
				"revision":         revision,
			}).Error; err != nil {
				return err
			}
			result.Patched++
		}
		result.Revision = revision
		return nil
	})
	return result, err
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func (r *gormRepository) DeleteAccounts(ctx context.Context, tokens []string) (MutationResult, error) {
	if len(tokens) == 0 {
		return MutationResult{}, nil
	}
	result := MutationResult{}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		revision, err := r.bumpRevision(tx)
		if err != nil {
			return err
		}
		clean := make([]string, 0, len(tokens))
		for _, token := range tokens {
			text := normalizeToken(token)
			if text != "" {
				clean = append(clean, text)
			}
		}
		if len(clean) == 0 {
			result.Revision = revision
			return nil
		}
		res := tx.Model(&accountEntity{}).Where("token IN ?", clean).Updates(map[string]any{
			"deleted_at": NowMS(),
			"revision":   revision,
			"updated_at": NowMS(),
		})
		if res.Error != nil {
			return res.Error
		}
		result.Deleted = int(res.RowsAffected)
		result.Revision = revision
		return nil
	})
	return result, err
}

func (r *gormRepository) ReplacePool(ctx context.Context, pool string, items []Upsert) (MutationResult, error) {
	pool = NormalizePool(pool)
	result := MutationResult{}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		revision, err := r.bumpRevision(tx)
		if err != nil {
			return err
		}
		newTokens := map[string]Upsert{}
		for _, item := range items {
			token := normalizeToken(item.Token)
			if token == "" {
				continue
			}
			item.Token = token
			item.Pool = pool
			newTokens[token] = item
		}
		var existing []accountEntity
		if err := tx.Where("pool = ? AND deleted_at = 0", pool).Find(&existing).Error; err != nil {
			return err
		}
		for _, entity := range existing {
			if _, ok := newTokens[entity.Token]; !ok {
				if err := tx.Model(&accountEntity{}).Where("token = ?", entity.Token).Updates(map[string]any{
					"deleted_at": NowMS(),
					"updated_at": NowMS(),
					"revision":   revision,
				}).Error; err != nil {
					return err
				}
				result.Deleted++
			}
		}
		now := NowMS()
		for _, item := range newTokens {
			quota := DefaultQuotaSet(pool)
			entity := accountEntity{
				Token:           item.Token,
				Pool:            pool,
				Status:          string(StatusActive),
				CreatedAt:       now,
				UpdatedAt:       now,
				TagsJSON:        tagsJSON(item.Tags),
				QuotaAutoJSON:   mustJSON(quota.Auto),
				QuotaFastJSON:   mustJSON(quota.Fast),
				QuotaExpertJSON: mustJSON(quota.Expert),
				QuotaHeavyJSON:  mustJSON(quota.Heavy),
				QuotaGrok43JSON: mustJSON(quota.Grok4_3),
				ExtJSON:         mustJSON(item.Ext),
				Revision:        revision,
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "token"}},
				DoUpdates: clause.Assignments(map[string]any{
					"pool":       entity.Pool,
					"status":     entity.Status,
					"updated_at": entity.UpdatedAt,
					"tags":       entity.TagsJSON,
					"ext":        entity.ExtJSON,
					"deleted_at": 0,
					"revision":   revision,
				}),
			}).Create(&entity).Error; err != nil {
				return err
			}
			result.Upserted++
		}
		result.Revision = revision
		return nil
	})
	return result, err
}

func (r *gormRepository) migrateLocalSQLite(ctx context.Context) error {
	localPath := paths.LocalAccountPath()
	if strings.TrimSpace(localPath) == "" {
		return nil
	}
	info, err := os.Stat(localPath)
	if errors.Is(err, os.ErrNotExist) || info == nil || info.IsDir() {
		return nil
	}
	var targetCount int64
	if err := r.db.WithContext(ctx).Model(&accountEntity{}).Count(&targetCount).Error; err != nil {
		return err
	}
	if targetCount > 0 {
		return nil
	}

	sourceDB, err := gorm.Open(sqlite.Open(localPath), &gorm.Config{})
	if err != nil {
		return err
	}
	sqlSource, err := sourceDB.DB()
	if err == nil {
		defer sqlSource.Close()
	}
	if !sourceDB.WithContext(ctx).Migrator().HasTable(&accountEntity{}) {
		return nil
	}

	var sourceAccounts []accountEntity
	if err := sourceDB.WithContext(ctx).Find(&sourceAccounts).Error; err != nil {
		return err
	}
	if len(sourceAccounts) == 0 {
		return nil
	}

	revision := int64(len(sourceAccounts))
	if sourceDB.WithContext(ctx).Migrator().HasTable(&metaEntity{}) {
		if value, err := readMetaRevision(sourceDB.WithContext(ctx)); err == nil && value > 0 {
			revision = value
		}
	}

	if err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, item := range sourceAccounts {
			item.Revision = revision
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "token"}},
				DoNothing: true,
			}).Create(&item).Error; err != nil {
				return err
			}
		}
		return tx.Model(&metaEntity{}).Where("meta_key = ?", "revision").Update("value", revision).Error
	}); err != nil {
		return err
	}

	migratedPath := localPath + ".migrated"
	_ = os.Remove(migratedPath)
	if err := os.Rename(localPath, migratedPath); err != nil {
		return err
	}
	return nil
}

func (r *gormRepository) migrateMetaSchema(ctx context.Context) error {
	migrator := r.db.WithContext(ctx).Migrator()
	if !migrator.HasTable(&metaEntity{}) {
		return nil
	}
	if migrator.HasColumn(&legacyMetaEntity{}, "key") && !migrator.HasColumn(&metaEntity{}, "meta_key") {
		if err := migrator.RenameColumn(&legacyMetaEntity{}, "key", "meta_key"); err != nil {
			return err
		}
	}
	return nil
}

func readMetaRevision(db *gorm.DB) (int64, error) {
	var meta metaEntity
	if err := db.First(&meta, "meta_key = ?", "revision").Error; err == nil {
		return meta.Value, nil
	}

	var legacy legacyMetaEntity
	if err := db.First(&legacy, "key = ?", "revision").Error; err != nil {
		return 0, err
	}
	return legacy.Value, nil
}
