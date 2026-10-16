package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"
)

type User struct {
	bun.BaseModel `bun:"table:users,alias:u"`

	ID        int64      `json:"id"`
	Username  string     `json:"username"`
	Role      Role       `json:"role"`       // 系统角色（权威）
	IsAdmin   bool       `json:"is_admin"`   // 派生只读（role ≥ admin），供前端过渡一个版本
	Disabled  bool       `json:"-"`          // 停用账号（管理后台用，登录/会话校验时拦截）
	ExpiresAt *time.Time `json:"expires_at"` // 仅访客有值；普通用户为 null
	InviteID  *int64     `json:"-"`          // 访客来源邀请（审计用，不外发）
}

// ---- 用户 ----

// CreateUser 创建用户（普通角色 user）；服务器上的第一个账号自动成为超级管理员。
// 注册产出其他档走 CreateUserWithRole。
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (*User, error) {
	return s.CreateUserWithRole(ctx, username, passwordHash, RoleUser)
}

// CreateUserWithRole 按指定系统角色创建用户；首个账号无条件成为 super（全站恰好一个的保底）。
func (s *Store) CreateUserWithRole(ctx context.Context, username, passwordHash string, role Role) (*User, error) {
	var n int
	if err := s.bun.NewRaw("SELECT COUNT(1) FROM users").Scan(ctx, &n); err != nil {
		return nil, err
	}
	if n == 0 {
		role = RoleSuper
	}
	row := &userRow{Username: username, PasswordHash: passwordHash, Role: string(role)}
	if _, err := s.bun.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, err
	}
	u := &User{ID: row.ID, Username: username}
	scanUserParts(u, string(role), nil)
	return u, nil
}

func (s *Store) UserByName(ctx context.Context, username string) (*User, string, error) {
	var u User
	var hash, role string
	var disabled int64
	var expiresAt *time.Time
	err := s.bun.NewRaw(
		"SELECT id, username, password_hash, role, disabled, expires_at FROM users WHERE username = ?", username).
		Scan(ctx, &u.ID, &u.Username, &hash, &role, &disabled, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	scanUserParts(&u, role, expiresAt)
	u.Disabled = disabled != 0
	return &u, hash, err
}
