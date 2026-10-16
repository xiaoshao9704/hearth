// 会话列表与下线：登录设备的自助管理（"我的登录状态"）。
// 对外暴露的 ID 是 token 的 sha256 前缀而不是 token 本身——列表要能指名某一条会话下线，
// 但把 token 发回浏览器等于把别的设备的凭证也发了一遍，只发不可逆的指纹。
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// Session 一条登录会话（"我的登录设备"列表项）。Token 不外发，仅内部比对。
type Session struct {
	ID        string     `json:"id"`         // token 指纹（sha256 前 16 字节 hex）
	UserAgent string     `json:"user_agent"` // 登录时的 UA 原文，展示层自己截断
	CreatedAt *time.Time `json:"created_at"` // 存量库补列前的老会话为空
	LastSeen  *time.Time `json:"last_seen"`  // 从未刷新过（本版本之前登录的）为空
	ExpiresAt time.Time  `json:"expires_at"`
	Current   bool       `json:"current"` // 是否发起本次请求的这条会话（接口层填充）
}

const sessionTTL = 7 * 24 * time.Hour // 会话有效期 7 天

// SessionID token 的对外指纹。取 sha256 前 16 字节：碰撞概率可忽略，且不可逆推 token。
func SessionID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}

func (s *Store) CreateSession(ctx context.Context, userID int64) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	_, err := s.bun.NewRaw(
		"INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)",
		token, userID, time.Now().Add(sessionTTL)).Exec(ctx)
	return token, err
}

// UserByToken 校验会话 token，过期、不存在或账号已停用返回 ErrNotFound。
// 第二个返回值是该会话绑定的设备 ID（空 = 不绑定），设备一致性由 api.auth 判定。
func (s *Store) UserByToken(ctx context.Context, token string) (*User, string, error) {
	var u User
	var role, deviceID string
	var expiresAt *time.Time
	err := s.bun.NewRaw(`
SELECT u.id, u.username, u.role, u.expires_at, COALESCE(s.device_id, '')
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.token = ? AND s.expires_at > ? AND u.disabled = 0`, token, time.Now()).
		Scan(ctx, &u.ID, &u.Username, &role, &expiresAt, &deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	scanUserParts(&u, role, expiresAt)
	return &u, deviceID, err
}

// DeleteSession 下线一条会话。推送订阅绑在会话上（session_id 是它的指纹）：
// 会话没了就不该再往那台设备推，所以两者一起删——退出登录与远程下线走的都是这里。
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if err := s.DeletePushSubscriptionsBySession(ctx, SessionID(token)); err != nil {
		return err
	}
	_, err := s.bun.NewRaw("DELETE FROM sessions WHERE token = ?", token).Exec(ctx)
	return err
}

// CreateSessionWithUA 签发会话并记下登录时的 UA 与时间（会话列表要展示这三项）。
func (s *Store) CreateSessionWithUA(ctx context.Context, userID int64, userAgent string) (string, error) {
	return s.CreateSessionWithDevice(ctx, userID, userAgent, "")
}

// CreateSessionWithDevice 签发会话并绑定设备：deviceID 非空的会话此后只认带同一个
// X-Device-Id 的请求（访客的「跟浏览器走」语义，见 api.auth）；普通登录传空串不绑定。
func (s *Store) CreateSessionWithDevice(ctx context.Context, userID int64, userAgent, deviceID string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	if len(userAgent) > 255 {
		userAgent = userAgent[:255]
	}
	now := time.Now()
	_, err := s.bun.NewRaw(
		"INSERT INTO sessions (token, user_id, expires_at, user_agent, created_at, last_seen, device_id) VALUES (?, ?, ?, ?, ?, ?, ?)",
		token, userID, now.Add(sessionTTL), userAgent, now, now, deviceID).Exec(ctx)
	return token, err
}

// ListSessions 该用户当前有效（未过期）的全部会话，最近活跃的排前面。
func (s *Store) ListSessions(ctx context.Context, userID int64) ([]Session, error) {
	out := []Session{}
	rows, err := s.bun.QueryContext(ctx, `
SELECT token, COALESCE(user_agent, ''), created_at, last_seen, expires_at FROM sessions
WHERE user_id = ? AND expires_at > ?
ORDER BY COALESCE(last_seen, created_at, expires_at) DESC`, userID, time.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var token string
		var se Session
		if err := rows.Scan(&token, &se.UserAgent, &se.CreatedAt, &se.LastSeen, &se.ExpiresAt); err != nil {
			return nil, err
		}
		se.ID = SessionID(token)
		out = append(out, se)
	}
	return out, rows.Err()
}

// DeleteSessionByID 按对外指纹下线该用户的一条会话；没有匹配行返回 false。
// 指纹不可逆，无法直接写进 WHERE：取回该用户的 token 逐个比指纹（一个人的会话数是个位数）。
func (s *Store) DeleteSessionByID(ctx context.Context, userID int64, id string) (bool, error) {
	var tokens []string
	if err := s.bun.NewRaw("SELECT token FROM sessions WHERE user_id = ?", userID).Scan(ctx, &tokens); err != nil {
		return false, err
	}
	for _, token := range tokens {
		if SessionID(token) != id {
			continue
		}
		if err := s.DeleteSession(ctx, token); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// TouchSession 刷新会话的最近活跃时间。调用频率由接口层节流（见 api/account.go）。
func (s *Store) TouchSession(ctx context.Context, token string) error {
	_, err := s.bun.NewRaw("UPDATE sessions SET last_seen = ? WHERE token = ?", time.Now(), token).Exec(ctx)
	return err
}
