package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestOpenMigratesWMPFSessionsTableToSessions 验证 Open 函数
// 能正确将旧版 wmpf_sessions 表迁移到新的 sessions 表。
func TestOpenMigratesWMPFSessionsTableToSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yyb.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	ctx := context.Background()
	if _, err = raw.ExecContext(ctx, `
CREATE TABLE wechat_accounts (
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
    last_checked_at INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE TABLE wmpf_sessions (
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
CREATE INDEX idx_sess_expires ON wmpf_sessions(expires_at);
INSERT INTO wechat_accounts(id, openid, login_buffer, created_at, updated_at)
VALUES(1, 'openid-1', 'login-buffer', 10, 10);
INSERT INTO wmpf_sessions(id, wechat_account_id, uin, tcp_proxy, session_blob, expires_at, created_at, updated_at)
VALUES(7, 1, 12345, '', '{"ready":true}', ?, 20, 20);
`, time.Now().Add(time.Hour).Unix()); err != nil {
		_ = raw.Close()
		t.Fatalf("seed old schema: %v", err)
	}
	if err = raw.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()

	oldExists, err := sqliteTableExists(ctx, db.sql, "wmpf_sessions")
	if err != nil {
		t.Fatalf("check old table: %v", err)
	}
	if oldExists {
		t.Fatalf("old wmpf_sessions table still exists")
	}
	newExists, err := sqliteTableExists(ctx, db.sql, "sessions")
	if err != nil {
		t.Fatalf("check new table: %v", err)
	}
	if !newExists {
		t.Fatalf("new sessions table does not exist")
	}

	session, err := db.GetSession(ctx, 1, "")
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if session.ID != 7 {
		t.Fatalf("session id = %d, want 7", session.ID)
	}
	if ready, ok := session.SessionBlob["ready"].(bool); !ok || !ready {
		t.Fatalf("session blob = %#v", session.SessionBlob)
	}
}

// TestOpenBackfillsStatusNotified 验证升级路径：旧库补上 status_notified 列时
// 会把现有状态记为「已通知」，避免升级后把所有历史账号补推一遍。
func TestOpenBackfillsStatusNotified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yyb.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	ctx := context.Background()
	// 旧库：没有 status_notified 列，且已存在一个 expired 账号
	if _, err = raw.ExecContext(ctx, `
CREATE TABLE wechat_accounts (
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
    last_checked_at INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
INSERT INTO wechat_accounts(id, openid, login_buffer, status, created_at, updated_at)
VALUES(1, 'openid-old', 'login-buffer', 'expired', 10, 10),
      (2, 'openid-old-alive', 'login-buffer', 'alive', 10, 10);
`); err != nil {
		_ = raw.Close()
		t.Fatalf("seed old schema: %v", err)
	}
	if err = raw.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 回填后：alive 历史账号不补推（否则升级即告警刷屏）；
	// expired 历史账号由 migrateRepairExpiredNotifications 清成待推送、各补推一次
	//（该行为由 TestOpenRepairsLegacyExpiredNotifications 单独覆盖）。
	pending, err := db.ListPendingNotifications(ctx)
	if err != nil {
		t.Fatalf("ListPendingNotifications() error = %v", err)
	}
	for _, item := range pending {
		if item.Status != "expired" {
			t.Fatalf("历史 %s 账号不应进入待补推: %+v", item.Status, item)
		}
	}
	if len(pending) > 1 {
		t.Fatalf("待补推 %d 条，超过了「仅历史 expired 账号各一条」的范围", len(pending))
	}
}

// TestStatusNotifiedDrivesPendingList 验证待补推列表只认终态、且以标记为准。
func TestStatusNotifiedDrivesPendingList(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "yyb.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	nick := "A"
	status := "alive"
	acc, err := db.UpsertAccount(ctx, "oid-a", "lb", nil, &nick, nil, nil, nil, &status)
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}

	// 扫码入库即「已通知」，不该有待补推
	if pending, err := db.ListPendingNotifications(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("新账号待补推 = %v (err=%v), want 0", pending, err)
	}

	// 状态收敛到 expired 但还没通知
	if err = db.SetAccountStatus(ctx, acc.ID, "expired"); err != nil {
		t.Fatalf("SetAccountStatus() error = %v", err)
	}
	pending, err := db.ListPendingNotifications(ctx)
	if err != nil {
		t.Fatalf("ListPendingNotifications() error = %v", err)
	}
	if len(pending) != 1 || pending[0].Status != "expired" || pending[0].NotifiedStatus != "alive" {
		t.Fatalf("待补推 = %+v, want 单条 alive → expired", pending)
	}

	// 记账后消失
	if err = db.MarkStatusNotified(ctx, acc.ID, "expired"); err != nil {
		t.Fatalf("MarkStatusNotified() error = %v", err)
	}
	if pending, err = db.ListPendingNotifications(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("记账后待补推 = %v (err=%v), want 0", pending, err)
	}

	// unknown 不参与补推
	if err = db.SetAccountStatus(ctx, acc.ID, "unknown"); err != nil {
		t.Fatalf("SetAccountStatus() error = %v", err)
	}
	if pending, err = db.ListPendingNotifications(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("unknown 状态待补推 = %v (err=%v), want 0", pending, err)
	}
}

// TestOpenRepairsLegacyExpiredNotifications 验证一次性修复迁移：
//
// status_notified 列首次回填时把存量 expired 账号当成了「已推送」，
// 而 expired 是终态，之后不会再有任何跃迁，这些账号的掉线告警就永远沉默了。
// 修复迁移应把它们清成待推送（重启后各补推一次），且只在部署生命周期内执行一次。
func TestOpenRepairsLegacyExpiredNotifications(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yyb.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	ctx := context.Background()
	// 旧库：没有 status_notified 列，存在一个 expired 与一个 alive 账号
	if _, err = raw.ExecContext(ctx, `
CREATE TABLE wechat_accounts (
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
    last_checked_at INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
INSERT INTO wechat_accounts(id, openid, login_buffer, status, created_at, updated_at)
VALUES(1, 'openid-dead', 'lb-1', 'expired', 10, 10),
      (2, 'openid-live', 'lb-2', 'alive',   10, 10);
`); err != nil {
		_ = raw.Close()
		t.Fatalf("seed old schema: %v", err)
	}
	if err = raw.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	// 修复后：expired 账号重新变成待推送，alive 账号不受影响
	pending, err := db.ListPendingNotifications(ctx)
	if err != nil {
		t.Fatalf("ListPendingNotifications() error = %v", err)
	}
	if len(pending) != 1 || pending[0].OpenID != "openid-dead" || pending[0].Status != "expired" {
		t.Fatalf("待补推 = %+v, want 仅 openid-dead 的 expired 一条", pending)
	}

	// 推送成功后记账，重启不应再出现
	if err = db.MarkStatusNotified(ctx, pending[0].AccountID, "expired"); err != nil {
		t.Fatalf("MarkStatusNotified() error = %v", err)
	}
	if err = db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	if pending, err = db2.ListPendingNotifications(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("重启后待补推 = %v (err=%v), want 0", pending, err)
	}
	// 修复标记已写入，后续重启不会再次清标记
	flag, err := db2.GetSetting(ctx, expiredNotifyRepairFlag)
	if err != nil || flag != "1" {
		t.Fatalf("repair flag = %q (err=%v), want \"1\"", flag, err)
	}
}
