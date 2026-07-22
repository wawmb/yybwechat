// Package store 提供基于 SQLite 的数据持久化层。
// 管理微信账号、MMTLS 会话和功能特性等数据的存储与查询。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

// schema 定义了数据库的表结构 DDL 语句。
// 包含 wechat_accounts、sessions 和 settings 三张表。
const schema = `
CREATE TABLE IF NOT EXISTS wechat_accounts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    openid          TEXT    NOT NULL UNIQUE,
    uin             INTEGER,
    alias           TEXT,
    nickname        TEXT,
    avatar          TEXT,
    user_info       TEXT,
    login_buffer    TEXT    NOT NULL,
    credentials     TEXT,
    status          TEXT,
    device_id       TEXT,
    last_checked_at INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_wxacc_uin ON wechat_accounts(uin);

CREATE TABLE IF NOT EXISTS sessions (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    wechat_account_id INTEGER NOT NULL REFERENCES wechat_accounts(id) ON DELETE CASCADE,
    uin               INTEGER,
    tcp_proxy         TEXT    NOT NULL DEFAULT '',
    session_blob      TEXT    NOT NULL,
    expires_at        INTEGER NOT NULL,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    UNIQUE(wechat_account_id, tcp_proxy)
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL DEFAULT 0
);
`

// Open 打开或创建 SQLite 数据库，执行建表迁移并返回 DB 实例。
type DB struct {
	sql *sql.DB
}

// WechatAccount 表示一条微信账号记录，包含登录凭证和用户信息。
type WechatAccount struct {
	ID            int64          `json:"id"`
	OpenID        string         `json:"openid"`
	UIN           *int64         `json:"uin,omitempty"`
	Alias         *string        `json:"alias,omitempty"`
	Nickname      *string        `json:"nickname,omitempty"`
	Avatar        *string        `json:"avatar,omitempty"`
	UserInfo      map[string]any `json:"user_info,omitempty"`
	LoginBuffer   string         `json:"login_buffer,omitempty"`
	Credentials   map[string]any `json:"credentials,omitempty"`
	Status        *string        `json:"status,omitempty"`
	DeviceID      *string        `json:"device_id,omitempty"`
	LastCheckedAt *int64         `json:"last_checked_at,omitempty"`
	CreatedAt     int64          `json:"created_at"`
	UpdatedAt     int64          `json:"updated_at"`
}

// AccountPublic 是 WechatAccount 的安全公开视图，隐藏了 login_buffer 和 credentials 等敏感字段。
type AccountPublic struct {
	ID            int64   `json:"id"`
	OpenID        string  `json:"openid"`
	UIN           *int64  `json:"uin"`
	Alias         *string `json:"alias"`
	Nickname      *string `json:"nickname"`
	Avatar        *string `json:"avatar"`
	Status        *string `json:"status"`
	DeviceID      *string `json:"device_id"`
	LastCheckedAt *int64  `json:"last_checked_at"`
	CreatedAt     int64   `json:"created_at"`
	UpdatedAt     int64   `json:"updated_at"`
}

// SessionRow 表示 sessions 表中的一行，存储已建立的 MMTLS 会话。
type SessionRow struct {
	ID              int64
	WechatAccountID int64
	UIN             *int64
	TCPProxy        string
	SessionBlob     map[string]any
	ExpiresAt       int64
	CreatedAt       int64
	UpdatedAt       int64
}

// Open 打开或创建 SQLite 数据库，执行建表迁移并返回 DB 实例。
// 支持 path=":memory:" 用于测试环境的内存数据库。
// 自动配置 WAL 模式、正常同步级别和外键约束。
func Open(path string) (*DB, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
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
	_, _ = db.ExecContext(ctx, "PRAGMA synchronous=NORMAL")
	_, _ = db.ExecContext(ctx, "PRAGMA foreign_keys=ON")
	if err = migrateSessionsTable(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err = db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = migrateAddDeviceID(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = migrateSettingsTable(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	out := &DB{sql: db}
	return out, nil
}

// Close 关闭数据库连接。
func (db *DB) Close() error {
	if db == nil || db.sql == nil {
		return nil
	}
	return db.sql.Close()
}

// migrateSessionsTable 将旧版 wmpf_sessions 表迁移到 sessions 表。
func migrateSessionsTable(ctx context.Context, db *sql.DB) error {
	oldExists, err := sqliteTableExists(ctx, db, "wmpf_sessions")
	if err != nil {
		return err
	}
	if !oldExists {
		return nil
	}
	newExists, err := sqliteTableExists(ctx, db, "sessions")
	if err != nil {
		return err
	}
	if !newExists {
		if _, err = db.ExecContext(ctx, "DROP INDEX IF EXISTS idx_sess_expires"); err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, "ALTER TABLE wmpf_sessions RENAME TO sessions")
		return err
	}
	if _, err = db.ExecContext(ctx, `
INSERT OR IGNORE INTO sessions
(id, wechat_account_id, uin, tcp_proxy, session_blob, expires_at, created_at, updated_at)
SELECT id, wechat_account_id, uin, tcp_proxy, session_blob, expires_at, created_at, updated_at
FROM wmpf_sessions`); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "DROP TABLE wmpf_sessions")
	return err
}

// migrateAddDeviceID 为 wechat_accounts 表补充 device_id 列（稳定设备 MAC 标识）。
// 兼容旧数据库：列不存在时执行 ALTER TABLE ADD COLUMN，已存在则跳过。
// 在 schema 建表之后执行，表一定已存在（全新库由 schema 直接带 device_id 建表）。
func migrateAddDeviceID(ctx context.Context, db *sql.DB) error {
	exists, err := sqliteTableExists(ctx, db, "wechat_accounts")
	if err != nil || !exists {
		return err
	}
	var n int
	err = db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('wechat_accounts') WHERE name='device_id'").Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		_, err = db.ExecContext(ctx, "ALTER TABLE wechat_accounts ADD COLUMN device_id TEXT")
	}
	return err
}

// migrateSettingsTable 为旧数据库补充 settings 表，不存在则创建。
func migrateSettingsTable(ctx context.Context, db *sql.DB) error {
	exists, err := sqliteTableExists(ctx, db, "settings")
	if err != nil || exists {
		return err
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS settings (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL,
		updated_at INTEGER NOT NULL DEFAULT 0
	)`)
	return err
}

// sqliteTableExists 检查数据库中是否存在指定名称的表。
func sqliteTableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n)
	return n > 0, err
}

// UpsertAccount 插入或更新微信账号。若 openid 已存在则更新登录缓冲区和凭证。
func (db *DB) UpsertAccount(ctx context.Context, openid, loginBuffer string, alias, nickname, avatar *string, userInfo map[string]any, credentials map[string]any, status *string) (*WechatAccount, error) {
	now := time.Now().Unix()
	userJSON, err := marshalNullable(userInfo)
	if err != nil {
		return nil, err
	}
	credJSON, err := marshalNullable(credentials)
	if err != nil {
		return nil, err
	}
	_, err = db.sql.ExecContext(ctx,
		`INSERT INTO wechat_accounts
		(openid, login_buffer, alias, nickname, avatar, user_info, credentials, status, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(openid) DO UPDATE SET
		login_buffer=excluded.login_buffer, alias=excluded.alias, nickname=excluded.nickname,
		avatar=excluded.avatar, user_info=excluded.user_info, credentials=excluded.credentials,
		status=excluded.status, updated_at=excluded.updated_at`,
		openid, loginBuffer, nullableString(alias), nullableString(nickname), nullableString(avatar),
		userJSON, credJSON, nullableString(status), now, now,
	)
	if err != nil {
		return nil, err
	}
	return db.GetAccountByOpenID(ctx, openid)
}

func (db *DB) GetAccount(ctx context.Context, id int64) (*WechatAccount, error) {
	return db.scanAccount(db.sql.QueryRowContext(ctx, selectAccountSQL+" WHERE id=?", id))
}

func (db *DB) GetAccountByOpenID(ctx context.Context, openid string) (*WechatAccount, error) {
	return db.scanAccount(db.sql.QueryRowContext(ctx, selectAccountSQL+" WHERE openid=?", openid))
}

func (db *DB) GetAccountByUIN(ctx context.Context, uin int64) (*WechatAccount, error) {
	return db.scanAccount(db.sql.QueryRowContext(ctx, selectAccountSQL+" WHERE uin=?", uin))
}

// ResolveAccount 根据引用字符串（ID 数字/UIN/openid）智能解析账号。
func (db *DB) ResolveAccount(ctx context.Context, ref string) (*WechatAccount, error) {
	if ref == "" {
		return nil, sql.ErrNoRows
	}
	if isDigits(ref) {
		n, _ := strconv.ParseInt(ref, 10, 64)
		if acc, err := db.GetAccountByUIN(ctx, n); err == nil {
			return acc, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return db.GetAccount(ctx, n)
	}
	return db.GetAccountByOpenID(ctx, ref)
}

func (db *DB) ListAccounts(ctx context.Context) ([]*WechatAccount, error) {
	rows, err := db.sql.QueryContext(ctx, selectAccountSQL+" ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WechatAccount
	for rows.Next() {
		acc, err := scanAccountRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, acc)
	}
	return out, rows.Err()
}

func (db *DB) SetAccountUIN(ctx context.Context, id, uin int64) error {
	_, err := db.sql.ExecContext(ctx, "UPDATE wechat_accounts SET uin=?, updated_at=? WHERE id=?", uin, time.Now().Unix(), id)
	return err
}

// generateDeviceMAC 生成一个随机设备 MAC 地址字符串（格式 02-XX-XX-XX-XX-XX）。
// 首位设置本地管理位、清除组播位，符合微信客户端设备标识格式。
func generateDeviceMAC() string {
	mac := make([]byte, 6)
	if _, err := rand.Read(mac); err != nil {
		panic("crypto/rand.Read failed: " + err.Error())
	}
	mac[0] = (mac[0] | 0x02) & 0xfe
	return fmt.Sprintf("%02X-%02X-%02X-%02X-%02X-%02X", mac[0], mac[1], mac[2], mac[3], mac[4], mac[5])
}

// EnsureAccountDeviceID 返回指定账号的稳定设备 MAC。
// 若数据库中尚无记录，则生成一个并持久化；后续调用始终复用同一个值。
// 这样同一账号的所有业务请求都携带一致的设备标识，避免因设备指纹频繁变化触发微信风控。
func (db *DB) EnsureAccountDeviceID(ctx context.Context, accountID int64) (string, error) {
	var deviceID sql.NullString
	err := db.sql.QueryRowContext(ctx, "SELECT device_id FROM wechat_accounts WHERE id=?", accountID).Scan(&deviceID)
	if err != nil {
		return "", err
	}
	if deviceID.Valid && deviceID.String != "" {
		return deviceID.String, nil
	}
	mac := generateDeviceMAC()
	_, err = db.sql.ExecContext(ctx,
		"UPDATE wechat_accounts SET device_id=?, updated_at=? WHERE id=? AND (device_id IS NULL OR device_id='')",
		mac, time.Now().Unix(), accountID)
	if err != nil {
		return "", err
	}
	return mac, nil
}

func (db *DB) SetAccountProfile(ctx context.Context, id int64, nickname, avatar *string, userInfo map[string]any) error {
	userJSON, err := marshalNullable(userInfo)
	if err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx,
		"UPDATE wechat_accounts SET nickname=?, avatar=?, user_info=?, updated_at=? WHERE id=?",
		nullableString(nickname), nullableString(avatar), userJSON, time.Now().Unix(), id,
	)
	return err
}

func (db *DB) SetAccountCredential(ctx context.Context, id int64, loginBuffer string, credentials map[string]any) error {
	credJSON, err := marshalNullable(credentials)
	if err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx,
		"UPDATE wechat_accounts SET login_buffer=?, credentials=?, updated_at=? WHERE id=?",
		loginBuffer, credJSON, time.Now().Unix(), id,
	)
	return err
}

func (db *DB) SetAccountStatus(ctx context.Context, id int64, status string) error {
	now := time.Now().Unix()
	_, err := db.sql.ExecContext(ctx,
		"UPDATE wechat_accounts SET status=?, last_checked_at=?, updated_at=? WHERE id=?",
		status, now, now, id,
	)
	return err
}

func (db *DB) DeleteAccount(ctx context.Context, id int64) error {
	_, err := db.sql.ExecContext(ctx, "DELETE FROM wechat_accounts WHERE id=?", id)
	return err
}

// GetSession 获取未过期的 MMTLS 会话记录。
func (db *DB) GetSession(ctx context.Context, accountID int64, tcpProxy string) (*SessionRow, error) {
	row := db.sql.QueryRowContext(ctx,
		"SELECT id, wechat_account_id, uin, tcp_proxy, session_blob, expires_at, created_at, updated_at FROM sessions WHERE wechat_account_id=? AND tcp_proxy=? AND expires_at>?",
		accountID, tcpProxy, time.Now().Unix(),
	)
	return scanSession(row)
}

// PutSession 插入或更新 MMTLS 会话记录。
func (db *DB) PutSession(ctx context.Context, accountID int64, uin *int64, sessionBlob map[string]any, expiresAt int64, tcpProxy string) error {
	now := time.Now().Unix()
	blob, err := json.Marshal(sessionBlob)
	if err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx,
		`INSERT INTO sessions
		(wechat_account_id, uin, tcp_proxy, session_blob, expires_at, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(wechat_account_id, tcp_proxy) DO UPDATE SET
		uin=excluded.uin, session_blob=excluded.session_blob,
		expires_at=excluded.expires_at, updated_at=excluded.updated_at`,
		accountID, nullableInt(uin), tcpProxy, string(blob), expiresAt, now, now,
	)
	return err
}

// InvalidateSession 删除指定账号和代理的会话（使其失效）。
func (db *DB) InvalidateSession(ctx context.Context, accountID int64, tcpProxy string) error {
	_, err := db.sql.ExecContext(ctx, "DELETE FROM sessions WHERE wechat_account_id=? AND tcp_proxy=?", accountID, tcpProxy)
	return err
}

// PurgeExpiredSessions 删除所有已过期的会话记录，返回清理的条数。
// 建议由定时任务周期性调用以避免 sessions 表无限增长。
func (db *DB) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := db.sql.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at<=?", time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (a *WechatAccount) Public() AccountPublic {
	return AccountPublic{
		ID:            a.ID,
		OpenID:        a.OpenID,
		UIN:           a.UIN,
		Alias:         a.Alias,
		Nickname:      a.Nickname,
		Avatar:        a.Avatar,
		Status:        a.Status,
		DeviceID:      a.DeviceID,
		LastCheckedAt: a.LastCheckedAt,
		CreatedAt:     a.CreatedAt,
		UpdatedAt:     a.UpdatedAt,
	}
}

const selectAccountSQL = `SELECT id, openid, uin, alias, nickname, avatar, user_info, login_buffer, credentials, status, device_id, last_checked_at, created_at, updated_at FROM wechat_accounts`

type accountScanner interface {
	Scan(dest ...any) error
}

func (db *DB) scanAccount(row accountScanner) (*WechatAccount, error) {
	return scanAccountRows(row)
}

func scanAccountRows(row accountScanner) (*WechatAccount, error) {
	var (
		a                       WechatAccount
		uin, lastChecked        sql.NullInt64
		alias, nickname, avatar sql.NullString
		userJSON, credJSON      sql.NullString
		status, deviceID        sql.NullString
	)
	err := row.Scan(
		&a.ID, &a.OpenID, &uin, &alias, &nickname, &avatar, &userJSON,
		&a.LoginBuffer, &credJSON, &status, &deviceID, &lastChecked, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if uin.Valid {
		a.UIN = &uin.Int64
	}
	a.Alias = stringPtrFromNull(alias)
	a.Nickname = stringPtrFromNull(nickname)
	a.Avatar = stringPtrFromNull(avatar)
	a.Status = stringPtrFromNull(status)
	a.DeviceID = stringPtrFromNull(deviceID)
	if lastChecked.Valid {
		a.LastCheckedAt = &lastChecked.Int64
	}
	if userJSON.Valid && userJSON.String != "" {
		_ = json.Unmarshal([]byte(userJSON.String), &a.UserInfo)
	}
	if credJSON.Valid && credJSON.String != "" {
		_ = json.Unmarshal([]byte(credJSON.String), &a.Credentials)
	}
	return &a, nil
}

func scanSession(row accountScanner) (*SessionRow, error) {
	var s SessionRow
	var uin sql.NullInt64
	var blob string
	if err := row.Scan(&s.ID, &s.WechatAccountID, &uin, &s.TCPProxy, &blob, &s.ExpiresAt, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	if uin.Valid {
		s.UIN = &uin.Int64
	}
	if err := json.Unmarshal([]byte(blob), &s.SessionBlob); err != nil {
		return nil, fmt.Errorf("decode session_blob: %w", err)
	}
	return &s, nil
}

func marshalNullable(v map[string]any) (sql.NullString, error) {
	if v == nil {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

func nullableString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

func nullableInt(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func stringPtrFromNull(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

// GetSetting 从 settings 表中读取指定 key 的值。若 key 不存在返 nil,""。
func (db *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := db.sql.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// SetSetting 插入或更新 settings 表中的键值对。
func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := db.sql.ExecContext(ctx,
		"INSERT INTO settings(key, value, updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at",
		key, value, time.Now().Unix(),
	)
	return err
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
