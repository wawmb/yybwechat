package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
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

func statusPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// TestCompactAccountIDsMigratesProxySettings 锁定「紧凑化必须迁移全部子表」。
//
// 早期实现只迁移了 sessions，漏掉 account_proxy_settings，而该表的外键没有
// ON UPDATE CASCADE。后果有两种：提交时被外键检查拒绝（接口 500），
// 或旧 ID 恰好落到另一条父行上，代理配置被静默错绑到别的账号——后者更危险，
// 取码和手机号会走别人的出口。
func TestCompactAccountIDsMigratesProxySettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yyb.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	// 三个账号按顺序插入（ID 1/2/3），各自带一份代理配置。
	// 用切片而不是 map：map 迭代顺序随机，会打乱「删中间那个」的意图。
	type owner struct {
		openid string
		proxy  string
	}
	owners := []owner{
		{"openid-a", "http://proxy-a.example:8080"},
		{"openid-b", "http://proxy-b.example:8080"},
		{"openid-c", "http://proxy-c.example:8080"},
	}
	for _, o := range owners {
		acc, err := db.UpsertAccount(ctx, o.openid, "buffer", nil, nil, nil, nil,
			map[string]any{"openid": o.openid}, statusPtr("alive"))
		if err != nil {
			t.Fatalf("UpsertAccount(%s) error = %v", o.openid, err)
		}
		if _, err = db.UpsertAccountProxySetting(ctx, acc.ID, "static", "http", o.proxy,
			"", nil, "", "", "", 600, nil); err != nil {
			t.Fatalf("UpsertAccountProxySetting(%s) error = %v", o.openid, err)
		}
	}
	// 删掉中间的，制造 ID 空洞，逼出真实的重排
	if err = db.DeleteAccount(ctx, 2); err != nil {
		t.Fatalf("DeleteAccount() error = %v", err)
	}

	mapping, err := db.CompactAccountIDs(ctx)
	if err != nil {
		t.Fatalf("CompactAccountIDs() error = %v", err)
	}
	if len(mapping) == 0 {
		t.Fatal("删除中间账号后应存在 ID 空洞，期望产生迁移映射")
	}

	// 剩下来的两个账号，代理配置必须仍然跟着自己：不能丢，更不能错绑
	for _, o := range []owner{owners[0], owners[2]} {
		acc, err := db.GetAccountByOpenID(ctx, o.openid)
		if err != nil || acc == nil {
			t.Fatalf("紧凑化后 %s 不存在 (err=%v)", o.openid, err)
		}
		setting, err := db.GetAccountProxySetting(ctx, acc.ID)
		if err != nil {
			t.Fatalf("%s 的代理配置丢失: %v", o.openid, err)
		}
		if setting.StaticProxy != o.proxy {
			t.Fatalf("%s 的代理 = %q, want %q（子表未随 ID 迁移，可能错绑到别的账号）",
				o.openid, setting.StaticProxy, o.proxy)
		}
	}
}

// TestAccountPublicKeepsChannelSemanticsSeparate 锁住两条渠道的生命周期语义：
// 应用宝有「扫码 30 天」窗口，电脑管家只有「下次续期」计划。
//
// 这里最容易出的错不是崩溃，而是把应用宝的倒计时套到电脑管家账号上——
// 于是界面对一个完全正常的管家账号报「建议重新扫码」，运营者照着提示去重扫，
// 反而把好好的授权换掉。所以两个方向都要断言：管家不报重扫，应用宝照报。
func TestAccountPublicKeepsChannelSemanticsSeparate(t *testing.T) {
	// 扫码发生在 40 天前：应用宝的 30 天窗口早就过了，必须提示重扫。
	observedAt := time.Now().Add(-40 * 24 * time.Hour).Unix()
	// 凭据也已过期：应用宝在这种情况下同样要提示重扫。
	expiredAt := time.Now().Add(-time.Hour).Unix()

	getString := func(v string) *string { return &v }

	cases := []struct {
		name           string
		account        WechatAccount
		wantProduct    string
		wantRescan     bool
		wantObservedAt bool
	}{
		{
			name: "应用宝：窗口已过，提示重扫",
			account: WechatAccount{
				OpenID: "o_appstore_1",
				Credentials: map[string]any{
					"refreshtoken":              "rt-value",
					"refresh_token_observed_at": float64(observedAt),
					"expires_at":                float64(expiredAt),
					"expires_in":                float64(7200),
				},
			},
			wantProduct:    "appstore",
			wantRescan:     true,
			wantObservedAt: true,
		},
		{
			name: "电脑管家：OpenID 前缀判定，不报重扫",
			account: WechatAccount{
				OpenID: "pcmgr:abc123",
				Credentials: map[string]any{
					// 历史数据里可能残留应用宝风格的字段，不能因此误判
					"refreshtoken":              "rt-value",
					"refresh_token_observed_at": float64(observedAt),
					"expires_at":                float64(expiredAt),
					"expires_in":                float64(86400),
				},
			},
			wantProduct: "pcmgr",
			wantRescan:  false,
			// 管家没有 30 天窗口，扫码时间不参与界面推算
			wantObservedAt: false,
		},
		{
			name: "电脑管家：仅凭 provider 字段也能判定",
			account: WechatAccount{
				OpenID: "o_unknown_origin",
				Credentials: map[string]any{
					"provider": "pcmgr",
				},
			},
			wantProduct:    "pcmgr",
			wantRescan:     false,
			wantObservedAt: false,
		},
		{
			name: "无凭据且状态未判死：不报重扫（空凭据本身不算证据）",
			account: WechatAccount{
				OpenID:      "o_empty",
				Credentials: nil,
			},
			wantProduct:    "appstore",
			wantRescan:     false,
			wantObservedAt: false,
		},
		{
			name: "无凭据且已判死：必须建议重扫",
			account: WechatAccount{
				OpenID:      "o_empty_dead",
				Status:      getString("expired"),
				Credentials: nil,
			},
			wantProduct:    "appstore",
			wantRescan:     true,
			wantObservedAt: false,
		},
		{
			name: "电脑管家已判死：不按扫码链路建议重扫",
			account: WechatAccount{
				OpenID: "pcmgr:dead",
				Status: getString("expired"),
			},
			wantProduct:    "pcmgr",
			wantRescan:     false,
			wantObservedAt: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := tc.account
			public := acc.Public()
			if public.ScanProduct != tc.wantProduct {
				t.Fatalf("ScanProduct = %q, want %q", public.ScanProduct, tc.wantProduct)
			}
			if public.RescanRecommended != tc.wantRescan {
				t.Fatalf("RescanRecommended = %v, want %v", public.RescanRecommended, tc.wantRescan)
			}
			if got := public.RefreshTokenObservedAt != nil; got != tc.wantObservedAt {
				t.Fatalf("RefreshTokenObservedAt present = %v, want %v", got, tc.wantObservedAt)
			}
		})
	}

	// 兜底：status=unknown 且凭据已过期时，应用宝账号也必须被引导重扫，
	// 这条和上面的 refresh_token 路径是两条独立触发路径，别只修了一半。
	appstoreUnknown := WechatAccount{
		OpenID: "o_appstore_2",
		Status: getString("unknown"),
		Credentials: map[string]any{
			"expires_at": float64(expiredAt),
			"expires_in": float64(7200),
		},
	}
	if !appstoreUnknown.Public().RescanRecommended {
		t.Fatal("应用宝账号凭据过期且状态未知时应提示重扫")
	}
	pcmgrUnknown := WechatAccount{
		OpenID: "pcmgr:def456",
		Status: getString("unknown"),
		Credentials: map[string]any{
			"expires_at": float64(expiredAt),
			"expires_in": float64(86400),
		},
	}
	if pcmgrUnknown.Public().RescanRecommended {
		t.Fatal("电脑管家账号凭据到期只表示该续期，不应提示重扫")
	}
}

// TestEnsureAccountDeviceIDIsStablePerAccount 锁住本项目自研的「稳定设备标识」特性：
// 同一账号始终拿到同一个 MAC，且不同账号互不相同。
//
// 上游没有这个特性（它每次请求随机生成设备），所以这条测试同时也是
// 一道防线：任何"对齐上游"的清理都不能把它连带删掉，
// 否则同一账号在短时间内会不断换设备指纹，正是我们要避免的风控信号。
func TestEnsureAccountDeviceIDIsStablePerAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yyb.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	a1, err := db.UpsertAccount(ctx, "openid-dev-1", "buffer", nil, nil, nil, nil,
		map[string]any{"openid": "openid-dev-1"}, statusPtr("alive"))
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}
	a2, err := db.UpsertAccount(ctx, "openid-dev-2", "buffer", nil, nil, nil, nil,
		map[string]any{"openid": "openid-dev-2"}, statusPtr("alive"))
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}

	first, err := db.EnsureAccountDeviceID(ctx, a1.ID)
	if err != nil {
		t.Fatalf("EnsureAccountDeviceID() error = %v", err)
	}
	if len(first) != 17 || strings.Count(first, "-") != 5 {
		t.Fatalf("设备 MAC 格式错误: %q", first)
	}
	// 同一账号反复取，必须是同一个值
	for i := 0; i < 5; i++ {
		again, err := db.EnsureAccountDeviceID(ctx, a1.ID)
		if err != nil {
			t.Fatalf("EnsureAccountDeviceID() error = %v", err)
		}
		if again != first {
			t.Fatalf("同一账号的设备标识必须稳定：%q != %q", again, first)
		}
	}
	// 不同账号必须是不同的值，否则「每个账号唯一」的承诺就不成立
	other, err := db.EnsureAccountDeviceID(ctx, a2.ID)
	if err != nil {
		t.Fatalf("EnsureAccountDeviceID() error = %v", err)
	}
	if other == first {
		t.Fatalf("不同账号不应共用设备标识: %q", other)
	}

	// 通过 GET 读回来时也要看得见（前端卡片与 /accounts 依赖它）
	loaded, err := db.GetAccount(ctx, a1.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if loaded.DeviceID == nil || *loaded.DeviceID != first {
		t.Fatalf("设备标识未持久化，got %v want %q", loaded.DeviceID, first)
	}
	if pub := loaded.Public(); pub.DeviceID == nil || *pub.DeviceID != first {
		t.Fatalf("Public() 应带出设备标识，got %v", pub.DeviceID)
	}
}
