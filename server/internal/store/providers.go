package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ProviderRecord 是注册制下的服务实例记录：alias 为实例唯一标识，type 为实现类型，
// params 为该实例的配置（整体 JSON 存入 TEXT 列）。
type ProviderRecord struct {
	Alias     string            `json:"alias"`
	Type      string            `json:"type"`
	Params    map[string]string `json:"params"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// CreateProvider 注册服务实例；alias 冲突时返回可用 IsUniqueViolation 判定的错误。
func (s *Store) CreateProvider(ctx context.Context, rec *ProviderRecord) error {
	params, err := json.Marshal(rec.Params)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, s.q(
		"INSERT INTO providers (alias, type, params) VALUES (?, ?, ?)"),
		rec.Alias, rec.Type, string(params))
	return err
}

// ProviderByAlias 按 alias 取实例记录，不存在返回 ErrNotFound。
func (s *Store) ProviderByAlias(ctx context.Context, alias string) (*ProviderRecord, error) {
	var rec ProviderRecord
	var params string
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT alias, type, params, created_at, updated_at FROM providers WHERE alias = ?"), alias).
		Scan(&rec.Alias, &rec.Type, &params, &rec.CreatedAt, &rec.UpdatedAt)
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
	rows, err := s.db.QueryContext(ctx, s.q(
		"SELECT alias, type, params, created_at, updated_at FROM providers ORDER BY created_at, alias"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ProviderRecord{}
	for rows.Next() {
		var rec ProviderRecord
		var params string
		if err := rows.Scan(&rec.Alias, &rec.Type, &params, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(params), &rec.Params); err != nil {
			return nil, err
		}
		out = append(out, &rec)
	}
	return out, rows.Err()
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
	_, err = s.db.ExecContext(ctx, s.q(
		"UPDATE providers SET params = ?, updated_at = CURRENT_TIMESTAMP WHERE alias = ?"),
		string(data), alias)
	return err
}

func (s *Store) DeleteProvider(ctx context.Context, alias string) error {
	_, err := s.db.ExecContext(ctx, s.q("DELETE FROM providers WHERE alias = ?"), alias)
	return err
}
