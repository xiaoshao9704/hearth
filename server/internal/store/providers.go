package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/uptrace/bun"
)

// ProviderRecord 是注册制下的服务实例记录：alias 为实例唯一标识，type 为实现类型，
// params 为该实例的配置（整体 JSON 存入 TEXT 列）。
type ProviderRecord struct {
	bun.BaseModel `bun:"table:providers"`

	Alias     string            `bun:",pk" json:"alias"`
	Type      string            `json:"type"`
	Params    map[string]string `bun:"-" json:"params"` // 整体 JSON 存 params 列，读写走 NewRaw
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// CreateProvider 注册服务实例；alias 冲突时返回可用 IsUniqueViolation 判定的错误。
func (s *Store) CreateProvider(ctx context.Context, rec *ProviderRecord) error {
	params, err := json.Marshal(rec.Params)
	if err != nil {
		return err
	}
	_, err = s.bun.NewRaw(
		"INSERT INTO providers (alias, type, params) VALUES (?, ?, ?)",
		rec.Alias, rec.Type, string(params)).Exec(ctx)
	return err
}

// ProviderByAlias 按 alias 取实例记录，不存在返回 ErrNotFound。
func (s *Store) ProviderByAlias(ctx context.Context, alias string) (*ProviderRecord, error) {
	var rec ProviderRecord
	var params string
	err := s.bun.NewRaw(
		"SELECT alias, type, params, created_at, updated_at FROM providers WHERE alias = ?", alias).
		Scan(ctx, &rec.Alias, &rec.Type, &params, &rec.CreatedAt, &rec.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(params), &rec.Params); err != nil {
		return nil, err
	}
	return &rec, nil
}

// ListProviders 列出全部实例，按 created_at 升序（同秒按 alias 次级排序，保证顺序稳定）。
func (s *Store) ListProviders(ctx context.Context) ([]*ProviderRecord, error) {
	// params 列是整体 JSON，先扫进完整行结构体再解到 Params（ProviderRecord.Params 不对应列）
	var rows []providerRow
	if err := s.bun.NewRaw(
		"SELECT alias, type, params, created_at, updated_at FROM providers ORDER BY created_at, alias").
		Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := []*ProviderRecord{}
	for _, row := range rows {
		rec := &ProviderRecord{
			Alias:     row.Alias,
			Type:      row.Type,
			CreatedAt: row.CreatedAt,
			UpdatedAt: row.UpdatedAt,
		}
		if err := json.Unmarshal([]byte(row.Params), &rec.Params); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// UpdateProviderParams 整体替换实例配置并刷新 updated_at；alias 不存在返回 ErrNotFound。
// 存在性用先查后写判定，不从 RowsAffected 推断（方言间「匹配行数/改变行数」语义不同）。
func (s *Store) UpdateProviderParams(ctx context.Context, alias string, params map[string]string) error {
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if _, err := s.ProviderByAlias(ctx, alias); err != nil {
		return err
	}
	_, err = s.bun.NewRaw(
		"UPDATE providers SET params = ?, updated_at = CURRENT_TIMESTAMP WHERE alias = ?",
		string(data), alias).Exec(ctx)
	return err
}

func (s *Store) DeleteProvider(ctx context.Context, alias string) error {
	_, err := s.bun.NewRaw("DELETE FROM providers WHERE alias = ?", alias).Exec(ctx)
	return err
}
