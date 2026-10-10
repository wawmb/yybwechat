// Package auth 提供 yybwechat 的单管理员认证与会话管理。
//
// 设计上刻意保持精简：系统只存在一个管理员账号，不提供注册入口，
// 也不做多用户与账号归属隔离。首次启动时通过初始化页面（或环境变量预设）
// 创建管理员，之后所有管理类接口都需要登录会话才能访问。
//
// 安全约定：
//   - 口令使用 bcrypt 哈希存储，永不明文落库。
//   - 会话令牌为 32 字节随机数，数据库中只保存其 SHA-256 摘要，
//     因此数据库泄露也无法直接冒用会话。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

// schema 定义认证库的表结构。独立成库，避免与业务库的 settings 表产生耦合。
const schema = `
CREATE TABLE IF NOT EXISTS admin_users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    last_login_at INTEGER,
    login_count   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS admin_sessions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash   BLOB    NOT NULL UNIQUE,
    user_id      INTEGER NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    user_agent   TEXT,
    ip_address   TEXT,
    expires_at   INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_admin_sessions_expires ON admin_sessions(expires_at);
CREATE INDEX IF NOT EXISTS idx_admin_sessions_user ON admin_sessions(user_id);
`

// ErrNoAdmin 表示系统中尚未创建管理员账号，需要先执行初始化。
var ErrNoAdmin = errors.New("no administrator configured")

// ErrAdminExists 表示管理员已存在，初始化请求被拒绝。
var ErrAdminExists = errors.New("administrator already exists")

// ErrInvalidCredentials 表示用户名或口令不正确。
var ErrInvalidCredentials = errors.New("invalid username or password")

// User 表示一条管理员记录。
type User struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	CreatedAt   int64  `json:"created_at"`
	LastLoginAt *int64 `json:"last_login_at,omitempty"`
	LoginCount  int64  `json:"login_count"`
}

// Store 是认证库的访问入口。
type Store struct {
	sql *sql.DB
}

// Open 打开或创建认证数据库并执行建表迁移。
// path 传 ":memory:" 可用于测试环境。
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite 单连接，避免并发写锁竞争
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err = db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if path != ":memory:" {
		_, _ = db.ExecContext(ctx, "PRAGMA journal_mode=WAL")
	}
	_, _ = db.ExecContext(ctx, "PRAGMA foreign_keys=ON")
	if _, err = db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{sql: db}, nil
}

// Close 关闭认证数据库连接。
func (s *Store) Close() error {
	if s == nil || s.sql == nil {
		return nil
	}
	return s.sql.Close()
}

// HasAdmin 判断系统是否已创建管理员。
func (s *Store) HasAdmin(ctx context.Context) (bool, error) {
	var n int
	err := s.sql.QueryRowContext(ctx, "SELECT count(*) FROM admin_users").Scan(&n)
	return n > 0, err
}

// CreateAdmin 创建唯一的管理员账号。
// 系统中已存在管理员时返回 ErrAdminExists，避免被重复调用覆盖口令。
func (s *Store) CreateAdmin(ctx context.Context, username, password string) (*User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return nil, errors.New("username is required")
	}
	if len(password) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	// 用 count(*) 做前置判断 + 唯一约束兜底，防止并发调用创建出第二个管理员
	var n int
	if err = s.sql.QueryRowContext(ctx, "SELECT count(*) FROM admin_users").Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, ErrAdminExists
	}
	res, err := s.sql.ExecContext(ctx,
		"INSERT INTO admin_users(username, password_hash, created_at, login_count) VALUES(?,?,?,0)",
		username, string(hash), now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Username: username, CreatedAt: now}, nil
}

// Authenticate 校验用户名与口令，成功时返回管理员信息并更新登录统计。
func (s *Store) Authenticate(ctx context.Context, username, password string) (*User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	var u User
	var hash string
	var lastLogin sql.NullInt64
	err := s.sql.QueryRowContext(ctx,
		"SELECT id, username, password_hash, created_at, last_login_at, login_count FROM admin_users WHERE username=?",
		username).Scan(&u.ID, &u.Username, &hash, &u.CreatedAt, &lastLogin, &u.LoginCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, ErrInvalidCredentials
	}
	if lastLogin.Valid {
		u.LastLoginAt = &lastLogin.Int64
	}
	now := time.Now().Unix()
	_, _ = s.sql.ExecContext(ctx,
		"UPDATE admin_users SET last_login_at=?, login_count=login_count+1 WHERE id=?", now, u.ID)
	u.LastLoginAt = &now
	u.LoginCount++
	return &u, nil
}

// CreateSession 创建登录会话，返回明文令牌。
// 数据库只保存令牌的 SHA-256 摘要，明文仅在返回给调用方这一次出现。
func (s *Store) CreateSession(ctx context.Context, userID int64, userAgent, ip string, ttl time.Duration) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	now := time.Now()
	if len(userAgent) > 512 {
		userAgent = userAgent[:512]
	}
	_, err := s.sql.ExecContext(ctx,
		"INSERT INTO admin_sessions(token_hash, user_id, user_agent, ip_address, expires_at, created_at, last_seen_at) VALUES(?,?,?,?,?,?,?)",
		sum[:], userID, userAgent, ip, now.Add(ttl).Unix(), now.Unix(), now.Unix())
	if err != nil {
		return "", err
	}
	return token, nil
}

// UserBySession 根据明文令牌查询管理员信息。令牌无效或已过期返回错误。
// 命中时会顺带刷新 last_seen_at，便于展示会话活跃度。
func (s *Store) UserBySession(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, sql.ErrNoRows
	}
	sum := sha256.Sum256([]byte(token))
	var u User
	var lastLogin sql.NullInt64
	err := s.sql.QueryRowContext(ctx, `
SELECT u.id, u.username, u.created_at, u.last_login_at, u.login_count
FROM admin_sessions s JOIN admin_users u ON u.id = s.user_id
WHERE s.token_hash = ? AND s.expires_at > ?`, sum[:], time.Now().Unix()).
		Scan(&u.ID, &u.Username, &u.CreatedAt, &lastLogin, &u.LoginCount)
	if err != nil {
		return nil, err
	}
	if lastLogin.Valid {
		u.LastLoginAt = &lastLogin.Int64
	}
	_, _ = s.sql.ExecContext(ctx,
		"UPDATE admin_sessions SET last_seen_at=? WHERE token_hash=?", time.Now().Unix(), sum[:])
	return &u, nil
}

// DeleteSession 删除指定令牌对应的会话（登出）。
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(token))
	_, err := s.sql.ExecContext(ctx, "DELETE FROM admin_sessions WHERE token_hash=?", sum[:])
	return err
}

// DeleteOtherSessions 删除指定用户除当前令牌外的所有会话（用于改密后踢下线）。
func (s *Store) DeleteOtherSessions(ctx context.Context, userID int64, keepToken string) (int64, error) {
	sum := sha256.Sum256([]byte(keepToken))
	res, err := s.sql.ExecContext(ctx,
		"DELETE FROM admin_sessions WHERE user_id=? AND token_hash<>?", userID, sum[:])
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeExpiredSessions 清理已过期会话，返回清理条数。
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.sql.ExecContext(ctx,
		"DELETE FROM admin_sessions WHERE expires_at<=?", time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ChangePassword 修改管理员口令，需要验证原口令。
func (s *Store) ChangePassword(ctx context.Context, userID int64, oldPassword, newPassword string) error {
	if len(newPassword) < 8 {
		return errors.New("new password must be at least 8 characters")
	}
	var hash string
	err := s.sql.QueryRowContext(ctx, "SELECT password_hash FROM admin_users WHERE id=?", userID).Scan(&hash)
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(oldPassword)) != nil {
		return ErrInvalidCredentials
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), 12)
	if err != nil {
		return err
	}
	_, err = s.sql.ExecContext(ctx, "UPDATE admin_users SET password_hash=? WHERE id=?", string(newHash), userID)
	return err
}

// DefaultAuthDBFilename 是认证数据库的默认文件名。
const DefaultAuthDBFilename = "auth.db"

// AuthDBPath 拼出认证数据库的完整路径。
// dbDir 是数据库所在目录（形如 <data-root>/db），filename 为空时使用默认文件名。
func AuthDBPath(dbDir, filename string) string {
	if filename == "" {
		filename = DefaultAuthDBFilename
	}
	return filepath.Join(dbDir, filename)
}

// FormatSetupHint 生成首次启动时的管理员初始化提示语，写入日志。
func FormatSetupHint(path string) string {
	return fmt.Sprintf("未检测到管理员账号，请访问 /setup 完成初始化（认证库: %s）", path)
}
