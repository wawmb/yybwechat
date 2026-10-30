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
	"strings"
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
    remark          TEXT,
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

CREATE TABLE IF NOT EXISTS proxy_provider_profiles (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    provider   TEXT    NOT NULL DEFAULT 'ipzan',
    proxy_type TEXT    NOT NULL DEFAULT 'http',
    api_url    TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS account_proxy_settings (
    account_id            INTEGER PRIMARY KEY REFERENCES wechat_accounts(id) ON DELETE CASCADE,
    mode                  TEXT    NOT NULL DEFAULT 'direct',
    proxy_type            TEXT    NOT NULL DEFAULT 'http',
    static_proxy          TEXT    NOT NULL DEFAULT '',
    api_url               TEXT    NOT NULL DEFAULT '',
    provider_profile_id   INTEGER,
    region_code           TEXT    NOT NULL DEFAULT '',
    region_province       TEXT    NOT NULL DEFAULT '',
    region_city           TEXT    NOT NULL DEFAULT '',
    refresh_ahead_seconds INTEGER NOT NULL DEFAULT 300,
    protocol_proxy_mode   TEXT    NOT NULL DEFAULT 'account',
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL
);

-- account_links 保存一次性授权链接。
--
-- 链接里的 token 本身才是凭证，这张表只存它的 SHA256（token_hash），
-- 另外用 AES-GCM 加密存一份明文副本，用于管理页「复制链接」——
-- 明文副本只在这台机器上可解，泄露数据库不等于泄露可用链接。
--
-- expected_openid 是跨渠道替换的安全边界：更新类链接必须由原账号扫码，
-- 换了微信号就不能改。target_product 记录这条链接打算用哪个渠道扫码，
-- switch_confirmed 表示运营者已经明确同意「换渠道 = 换身份」。
CREATE TABLE IF NOT EXISTS account_links (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash       TEXT    NOT NULL UNIQUE,
    token_ciphertext TEXT    NOT NULL DEFAULT '',
    kind             TEXT    NOT NULL,
    account_id       INTEGER NOT NULL REFERENCES wechat_accounts(id) ON DELETE CASCADE,
    owner_user_id    INTEGER,
    expected_openid  TEXT    NOT NULL DEFAULT '',
    expires_at       INTEGER NOT NULL,
    used_at          INTEGER,
    revoked_at       INTEGER,
    created_at       INTEGER NOT NULL,
    target_product   TEXT    NOT NULL DEFAULT '',
    switch_confirmed INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_account_links_account ON account_links(account_id);
CREATE INDEX IF NOT EXISTS idx_account_links_expires ON account_links(expires_at);
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
	Remark        *string        `json:"remark,omitempty"`
	LastCheckedAt *int64         `json:"last_checked_at,omitempty"`
	CreatedAt     int64          `json:"created_at"`
	UpdatedAt     int64          `json:"updated_at"`

	// credentialJSON 保存从库里读出来的原始凭据 JSON 快照。它不导出、不进
	// JSON，只在做条件更新（CAS）时用来确认「这条记录还是我读到的那个版本」。
	//
	// 用途是防覆盖：一次扫码/刷新从读到写之间可能隔了几秒，期间保活可能已经
	// 轮换了凭据。如果此时拿旧快照直接 UPDATE，就会把刚拿到的新 token 盖回去，
	// 下一次续期必然被拒。用快照比对可以把这种竞态变成一次明确的失败。
	credentialJSON sql.NullString
}

// StatusSessionOnly 是「授权已被拒绝、但缓存会话可能仍可用」的中间状态。
//
// 上游早期把 OAuth 被拒和 ILink 会话失效当成一件事：只要 refresh 被拒就清账号。
// 实测发现两者并不等价——refresh 被拒之后，同一份会话仍然能完成 MMTLS 认证并
// 取到 32 字符 code。所以这里单独开一个状态：既不宣称账号健康，也不把它判死。
//
// 进入该状态后：
//   - 不再发起新握手、不再取新票据、不再走自动恢复、不重放业务请求；
//   - 缓存会话仍可继续使用，成功也不算「保活恢复」；
//   - 缓存缺失或到期后返回需重扫；手动标失效仍完整清理；
//   - 禁止手动改回 alive（见 httpapi/account_maintenance.go）。
const StatusSessionOnly = "session_only"

// SessionOnly 判断账号是否处于「仅原会话可用」状态。
func (a *WechatAccount) SessionOnly() bool {
	return a != nil && a.Status != nil && strings.ToLower(strings.TrimSpace(*a.Status)) == StatusSessionOnly
}

// AccountPublic 是 WechatAccount 的安全公开视图，隐藏了 login_buffer 和 credentials 等敏感字段。
//
// 凭据相关字段只暴露时间戳与时长，不暴露 token 本身，用于在控制台展示两条生命周期：
//   - credential_expires_at / credential_expires_in：当前凭据的剩余有效期，反映后台保活的效果；
//   - refresh_token_observed_at：refresh token 首次被观察到的时间，用于推算 30 天扫码授权窗口。
type AccountPublic struct {
	ID                     int64   `json:"id"`
	OpenID                 string  `json:"openid"`
	UIN                    *int64  `json:"uin"`
	Alias                  *string `json:"alias"`
	Nickname               *string `json:"nickname"`
	Avatar                 *string `json:"avatar"`
	Status                 *string `json:"status"`
	DeviceID               *string `json:"device_id"`
	Remark                 *string `json:"remark"`
	LastCheckedAt          *int64  `json:"last_checked_at"`
	RefreshTokenObservedAt *int64  `json:"refresh_token_observed_at,omitempty"`
	CredentialExpiresAt    *int64  `json:"credential_expires_at,omitempty"`
	CredentialExpiresIn    int64   `json:"credential_expires_in,omitempty"`
	RescanRecommended      bool    `json:"rescan_recommended"`
	// ScanProduct 是账号当初的扫码渠道：appstore = 应用宝，pcmgr = 腾讯电脑管家。
	// 前端据此显示渠道标识，脚本也靠它区分两套续期语义：
	// 应用宝有「扫码 30 天」窗口，电脑管家只有「下次续期」计划，两者不能互相套用。
	ScanProduct string `json:"scan_product"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// accountScanProduct 判定账号的扫码渠道。
//
// 优先看 OpenID 前缀：电脑管家账号的 OpenID 由本地生成（pcmgr:<账号标识>），
// 一定带前缀，这是最可靠的来源。历史凭据里没有 provider 字段时也能正确判定。
// 其次看凭据里的 provider，最后回落到应用宝。
func accountScanProduct(openID string, credentials map[string]any) string {
	if strings.HasPrefix(openID, "pcmgr:") {
		return "pcmgr"
	}
	if provider := credentialString(credentials, "provider"); provider == "pcmgr" {
		return "pcmgr"
	}
	return "appstore"
}

// credentialExpiresAt 从凭据映射中读取到期时间戳，无法判断时返回 0。
//
// 这里刻意不引用 protocol 包：store 只需要一个数字，避免为取一个字段而引入协议层依赖。
func credentialExpiresAt(credentials map[string]any) int64 {
	if credentials == nil {
		return 0
	}
	switch v := credentials["expires_at"].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}

// credentialInt64 按 key 读取凭据里的整数字段，无法解析时返回 0。
func credentialInt64(credentials map[string]any, key string) int64 {
	if credentials == nil {
		return 0
	}
	switch v := credentials[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}

// credentialString 按 key 读取凭据里的字符串字段。
func credentialString(credentials map[string]any, key string) string {
	if credentials == nil {
		return ""
	}
	value, _ := credentials[key].(string)
	return value
}

// rescanWindowSeconds 是微信侧的扫码授权窗口长度。
// 超过这个时长即便保活一直成功，也应提示运营者重新扫码。
const rescanWindowSeconds = int64(30 * 24 * 60 * 60)

// rescanRecommendSeconds 是开始建议重扫的阈值，比窗口略早，给出提前准备时间。
const rescanRecommendSeconds = int64(25 * 24 * 60 * 60)

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

	// blobJSON 是 session_blob 列的原始字符串快照。与 credentialJSON 同理，
	// 它只在条件更新里当 CAS 令牌用：原生会话替换必须证明「我改的还是我读到的
	// 那一行」，否则并发的扫码、密钥轮换或手动失效会被旧请求悄悄盖掉。
	blobJSON string
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
	if err = migrateAccountProxySettings(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = migrateSettingsTable(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = migrateAddRemark(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = migrateAddStatusNotified(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = migrateAddExpiryAlerted(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = migrateRepairExpiredNotifications(ctx, db); err != nil {
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
	err := db.sql.Close()
	if db.sql.Stats().OpenConnections == 0 {
		return err
	}
	// sql.DB.Close 只是停止接受新查询，**不会等待**正在建立中的连接。
	// SQLite 在连接收尾时还会创建/删除 WAL 文件，此时如果调用方紧接着删掉
	// 数据目录（测试里的 TempDir、容器重建时的清理），就会拿到
	// "directory not empty" 这类看似无关的错误。这里最多等 5 秒。
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for db.sql.Stats().OpenConnections != 0 {
		select {
		case <-ticker.C:
		case <-timeout.C:
			return errors.Join(err, errors.New("database close: pending connections did not stop within 5s"))
		}
	}
	return err
}

// migrateAccountProxySettings 为旧库补齐代理设置表的新列。
//
// 少了这一步，从早期版本升级上来的库会在第一条代理相关查询上直接报
// "no such column: provider_profile_id"——整个代理功能不可用，
// 而新装的库一切正常，问题只在升级路径上出现。
func migrateAccountProxySettings(ctx context.Context, db *sql.DB) error {
	exists, err := sqliteTableExists(ctx, db, "account_proxy_settings")
	if err != nil || !exists {
		return err
	}
	columns := []struct {
		name       string
		definition string
	}{
		{"provider_profile_id", "INTEGER"},
		{"region_code", "TEXT NOT NULL DEFAULT ''"},
		{"region_province", "TEXT NOT NULL DEFAULT ''"},
		{"region_city", "TEXT NOT NULL DEFAULT ''"},
		{"refresh_ahead_seconds", "INTEGER NOT NULL DEFAULT 300"},
		{"protocol_proxy_mode", "TEXT NOT NULL DEFAULT 'account'"},
	}
	for _, column := range columns {
		ok, err := sqliteColumnExists(ctx, db, "account_proxy_settings", column.name)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		if _, err = db.ExecContext(ctx, "ALTER TABLE account_proxy_settings ADD COLUMN "+column.name+" "+column.definition); err != nil {
			return err
		}
	}
	return nil
}

// sqliteColumnExists 判断某张表是否已有指定列。
func sqliteColumnExists(ctx context.Context, db *sql.DB, table, wanted string) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == wanted {
			return true, nil
		}
	}
	return false, rows.Err()
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
	openid = strings.TrimSpace(openid)
	if openid == "" {
		return nil, errors.New("openid is required")
	}
	// 「查已有账号 → 算最小空闲 ID → 写入」必须在一个事务里完成，
	// 否则两次并发成功的扫码会算到同一个空位，第二个必然撞主键。
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// 已有账号：原位更新。这里刻意把 expiry_alerted_at 归零——
	// 重新扫码意味着重新拿到了一份授权，上一轮的失效告警记账要跟着失效，
	// 否则新授权过期时会被「已经推送过」挡住。
	var existingID int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM wechat_accounts WHERE openid=?", openid).Scan(&existingID)
	if err == nil {
		if _, err = tx.ExecContext(ctx,
			`UPDATE wechat_accounts SET login_buffer=?, alias=?, nickname=?, avatar=?, user_info=?,
credentials=?, status=?, status_notified=?, expiry_alerted_at=0, updated_at=? WHERE id=?`,
			loginBuffer, nullableString(alias), nullableString(nickname), nullableString(avatar),
			userJSON, credJSON, nullableString(status), nullableString(status), now, existingID,
		); err != nil {
			return nil, err
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		// 新账号：占用「最小的空闲 ID」而不是继续自增。
		//
		// 这样「删掉唯一账号再重新扫码」会回到 ID 1，脚本里写的 @1 不用改；
		// 否则 ID 一路涨到 37、38，运营者只能靠「整理 ID」去收，而整理又会
		// 重排其他账号、连带把别人的引用打乱。已有账号的 ID 不受影响。
		nextID, allocErr := lowestFreeAccountID(ctx, tx)
		if allocErr != nil {
			return nil, allocErr
		}
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO wechat_accounts
			(id, openid, login_buffer, alias, nickname, avatar, user_info, credentials, status, status_notified, created_at, updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			nextID, openid, loginBuffer, nullableString(alias), nullableString(nickname), nullableString(avatar),
			userJSON, credJSON, nullableString(status), nullableString(status), now, now,
		); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return db.GetAccountByOpenID(ctx, openid)
}

// lowestFreeAccountID 返回从 1 开始的最小空闲账号 ID。
//
// 调用方必须已开启事务：这里只做读取，真正的写入仍由调用方在同一事务里完成，
// 否则并发时两个事务会挑到同一个空位。
func lowestFreeAccountID(ctx context.Context, tx *sql.Tx) (int64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM wechat_accounts ORDER BY id")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	nextID := int64(1)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		if id < nextID {
			continue
		}
		if id == nextID {
			nextID++
			continue
		}
		// id > nextID：这里就是第一个空洞
		break
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return nextID, nil
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

// ApplyCredentialRefresh 以「条件更新」写回续期结果（凭据 + 状态）。
//
// 与按 ID 直写的区别在于 WHERE 里那一串比对：账号身份、原凭据快照、原 login_buffer
// 与状态都必须还是调用方读到的那一份。一次刷新可能持续数秒，期间账号可能被
// 「更新链接 / 合并 / 跨渠道重扫」整体替换；此时旧结果写进去会把新身份覆盖掉，
// 外部表现就是「刚换完身份，过一会儿又变回旧号」。
//
// 返回 false 表示库里已被别人改过，本次结果应当丢弃——这不是错误，也不该告警。
// status 为 expired 时会顺带清空凭据并删除会话（终态不再倒计时）。
func (db *DB) ApplyCredentialRefresh(ctx context.Context, expected *WechatAccount, loginBuffer string, credentials map[string]any, status string) (bool, error) {
	if expected == nil {
		return false, errors.New("apply credential refresh requires an expected account snapshot")
	}
	if status == "expired" {
		loginBuffer, credentials = "", nil
	}
	if status == StatusSessionOnly {
		// 被拒的 token 与登录票据都不能再留着：它们已被上游明确否认，
		// 留在库里只会被下一次调用再拿去撞墙。扫码时间属于审计元数据，
		// 与 token 无关，必须保留——否则「这串授权是哪天扫的」就断了。
		credentials = nil
		if observed := credentialInt64(expected.Credentials, "refresh_token_observed_at"); observed > 0 {
			credentials = map[string]any{"refresh_token_observed_at": observed}
		}
		loginBuffer = ""
	}
	encoded, err := marshalNullable(credentials)
	if err != nil {
		return false, err
	}
	now := time.Now().Unix()
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`UPDATE wechat_accounts SET login_buffer=?, credentials=?, status=?, last_checked_at=?, updated_at=?
WHERE id=? AND openid=? AND credentials IS ? AND login_buffer=? AND status IS ?`,
		loginBuffer, encoded, status, now, now, expected.ID, expected.OpenID, expected.credentialJSON, expected.LoginBuffer, nullableString(expected.Status))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if count == 1 && status == "expired" {
		if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE wechat_account_id=?", expected.ID); err != nil {
			return false, err
		}
	}
	return count == 1, tx.Commit()
}

// SetAccountCredentialStatus 在同一个事务里更新凭据与状态。
// 保活刷新成功后调用，避免出现「凭据已换新但状态仍是 unknown」的中间态。
func (db *DB) SetAccountCredentialStatus(ctx context.Context, id int64, loginBuffer string, credentials map[string]any, status string) error {
	credJSON, err := marshalNullable(credentials)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = db.sql.ExecContext(ctx,
		"UPDATE wechat_accounts SET login_buffer=?, credentials=?, status=?, last_checked_at=?, updated_at=? WHERE id=?",
		loginBuffer, credJSON, status, now, now, id,
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

// GetSessionForRecovery 读取会话记录，**不**按本地到期时间过滤。
//
// 存在的理由：早期版本把缓存的 30 分钟本地期限当成了服务端会话失效，
// 于是正常可用的会话在本地被标成过期。这里把「读得到」和「能不能用」
// 分开：读的时候不看本地期限，能不能用由协议层按服务端下发的 PSK 期限判断。
func (db *DB) GetSessionForRecovery(ctx context.Context, accountID int64, tcpProxy string) (*SessionRow, error) {
	row := db.sql.QueryRowContext(ctx,
		"SELECT id, wechat_account_id, uin, tcp_proxy, session_blob, expires_at, created_at, updated_at FROM sessions WHERE wechat_account_id=? AND tcp_proxy=?",
		accountID, tcpProxy,
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

// InvalidateAccountSessions 删除指定账号在所有代理下的会话。
// 凭据变更或判定失效时需要全量清理，否则换代理后仍可能命中旧登录态。
func (db *DB) InvalidateAccountSessions(ctx context.Context, accountID int64) error {
	_, err := db.sql.ExecContext(ctx, "DELETE FROM sessions WHERE wechat_account_id=?", accountID)
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

// Public 生成对外暴露的安全视图，并推导出两条生命周期信息。
//
// 扫码天数（refresh_token_observed_at）只在扫码成功那一刻写入，保活刷新不会改动它，
// 因此它真实反映「这串凭据是哪天扫码得来的」。历史记录若没有该字段则保持为空，
// 界面显示「未记录」，不做猜测性回填。
func (a *WechatAccount) Public() AccountPublic {
	product := accountScanProduct(a.OpenID, a.Credentials)
	status := ""
	if a.Status != nil {
		status = strings.ToLower(strings.TrimSpace(*a.Status))
	}
	var refreshTokenObservedAt *int64
	// 「仅原会话」本身就意味着授权已被拒，直接建议重扫，
	// 不等 25 天窗口也不看凭据是否到期。
	rescanRecommended := a.SessionOnly()
	var credentialExpiresAtPtr *int64

	credentialExpiresIn := credentialInt64(a.Credentials, "expires_in")
	if credentialExpiresIn <= 0 {
		credentialExpiresIn = 7200
	}
	// 电脑管家没有「扫码 30 天」这回事：它的凭据靠本机原生续期计划滚动，
	// 套用应用宝的 25 天倒计时只会产生「建议重新扫码」的假告警。
	if product != "pcmgr" && (credentialString(a.Credentials, "refreshtoken") != "" || a.SessionOnly()) {
		if observedAt := credentialInt64(a.Credentials, "refresh_token_observed_at"); observedAt > 0 {
			refreshTokenObservedAt = &observedAt
			// 用 || 而不是覆盖：session_only 的判断与 25 天窗口是两个独立条件，
			// 覆盖会让「刚扫的号但授权已被拒」显示成不需要重扫。
			rescanRecommended = rescanRecommended || time.Now().Unix()-observedAt >= rescanRecommendSeconds
		}
	}
	expiresAtValue := credentialExpiresAt(a.Credentials)
	if expiresAtValue <= 0 {
		if refreshedAt := credentialInt64(a.Credentials, "refresh_refreshed_at"); refreshedAt > 0 {
			expiresAtValue = refreshedAt + credentialExpiresIn
		}
	}
	if expiresAtValue > 0 {
		expiresAt := expiresAtValue
		credentialExpiresAtPtr = &expiresAt
		// 保活只把「瞬时失败」记为 unknown 以便重试，但缓存凭据一旦越过到期时刻，
		// 界面就应引导运营者重扫，而不是让账号看上去仍然健康。
		// 电脑管家例外：它的凭据到期就是「该续期了」，续期由原生接口滚动完成，
		// 提示重扫会把人引向完全错误的操作。
		if product != "pcmgr" && !rescanRecommended && (status == "" || status == "unknown") && expiresAtValue <= time.Now().Unix() {
			rescanRecommended = true
		}
	}
	// 已判死的应用宝账号一律建议重扫。
	//
	// expired 是终态：自动重试不会让它复活，唯一出路是重新扫码。
	// 这条兜底必须独立于「凭据里有没有到期时间」——凭据被清空的账号
	// （credentials 为 nil）根本没有到期时间，靠上面两条分支永远推不出来。
	if !rescanRecommended && product != "pcmgr" && status == "expired" {
		rescanRecommended = true
	}

	return AccountPublic{
		ID:                     a.ID,
		OpenID:                 a.OpenID,
		UIN:                    a.UIN,
		Alias:                  a.Alias,
		Nickname:               a.Nickname,
		Avatar:                 a.Avatar,
		Status:                 a.Status,
		DeviceID:               a.DeviceID,
		Remark:                 a.Remark,
		LastCheckedAt:          a.LastCheckedAt,
		RefreshTokenObservedAt: refreshTokenObservedAt,
		CredentialExpiresAt:    credentialExpiresAtPtr,
		CredentialExpiresIn:    credentialExpiresIn,
		RescanRecommended:      rescanRecommended,
		ScanProduct:            product,
		CreatedAt:              a.CreatedAt,
		UpdatedAt:              a.UpdatedAt,
	}
}

const selectAccountSQL = `SELECT id, openid, uin, alias, nickname, avatar, user_info, login_buffer, credentials, status, device_id, remark, last_checked_at, created_at, updated_at FROM wechat_accounts`

type accountScanner interface {
	Scan(dest ...any) error
}

func (db *DB) scanAccount(row accountScanner) (*WechatAccount, error) {
	return scanAccountRows(row)
}

func scanAccountRows(row accountScanner) (*WechatAccount, error) {
	var (
		a                        WechatAccount
		uin, lastChecked         sql.NullInt64
		alias, nickname, avatar  sql.NullString
		userJSON, credJSON       sql.NullString
		status, deviceID, remark sql.NullString
	)
	err := row.Scan(
		&a.ID, &a.OpenID, &uin, &alias, &nickname, &avatar, &userJSON,
		&a.LoginBuffer, &credJSON, &status, &deviceID, &remark, &lastChecked, &a.CreatedAt, &a.UpdatedAt,
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
	a.Remark = stringPtrFromNull(remark)
	if lastChecked.Valid {
		a.LastCheckedAt = &lastChecked.Int64
	}
	if userJSON.Valid && userJSON.String != "" {
		_ = json.Unmarshal([]byte(userJSON.String), &a.UserInfo)
	}
	if credJSON.Valid && credJSON.String != "" {
		_ = json.Unmarshal([]byte(credJSON.String), &a.Credentials)
	}
	// 保留原始 JSON 快照，供条件更新比对
	a.credentialJSON = credJSON
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
	s.blobJSON = blob
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
