package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newLinkTestDB 建一个带两个账号的库，用于授权替换相关测试。
func newLinkTestDB(t *testing.T) (*DB, *WechatAccount, *WechatAccount) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "yyb.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	// 刻意在创建时就带上别名：替换身份时必须保留运营者设过的别名，
	// 只在别名为空时才用新昵称补上。
	alias := "我的别名"
	target, err := db.UpsertAccount(ctx, "openid-target", "buffer-old", &alias, nil, nil, nil,
		map[string]any{"openid": "openid-target"}, statusPtr("alive"))
	if err != nil {
		t.Fatalf("UpsertAccount(target) error = %v", err)
	}
	other, err := db.UpsertAccount(ctx, "openid-other", "buffer-other", nil, nil, nil, nil,
		map[string]any{"openid": "openid-other"}, statusPtr("alive"))
	if err != nil {
		t.Fatalf("UpsertAccount(other) error = %v", err)
	}
	return db, target, other
}

// TestUpdateAccountFromLinkReplacesInPlaceAndPreservesConfig 锁住「原子替换」的承诺：
// 身份相关字段换掉，按账号 ID 挂的配置一个都不能丢，而且不新增账号。
//
// 这条测试的价值在于反向验证：把 UPDATE 里 COALESCE(alias) 去掉、或者忘记删
// sessions，测试都会立刻变红——而这两种错误在人工点一遍时几乎发现不了。
func TestUpdateAccountFromLinkReplacesInPlaceAndPreservesConfig(t *testing.T) {
	db, target, _ := newLinkTestDB(t)
	ctx := context.Background()

	// 给目标账号配上「运营者花时间设置过」的东西
	if _, err := db.SetAccountRemark(ctx, target.ID, "主力号-勿删"); err != nil {
		t.Fatalf("SetAccountRemark() error = %v", err)
	}
	if _, err := db.UpsertAccountProxySetting(ctx, target.ID, "static", "http",
		"http://proxy.example:8080", "", nil, "", "", "", 600, nil); err != nil {
		t.Fatalf("UpsertAccountProxySetting() error = %v", err)
	}
	uin := int64(123456789)
	if err := db.PutSession(ctx, target.ID, &uin, map[string]any{"k": "v"}, time.Now().Add(time.Hour).Unix(), ""); err != nil {
		t.Fatalf("PutSession() error = %v", err)
	}

	link, err := db.CreateAccountLinkForProduct(ctx, "hash-1", "", "update", target.ID, nil,
		"openid-target", time.Now().Add(10*time.Minute).Unix(), "appstore", false)
	if err != nil {
		t.Fatalf("CreateAccountLinkForProduct() error = %v", err)
	}

	nick := "新昵称"
	updated, err := db.UpdateAccountFromLink(ctx, link, "openid-target", "buffer-new",
		&nick, nil, map[string]any{"nickname": nick}, map[string]any{"openid": "openid-target", "provider": "appstore"})
	if err != nil {
		t.Fatalf("UpdateAccountFromLink() error = %v", err)
	}

	if updated.ID != target.ID {
		t.Fatalf("要求原 ID 内替换，got ID=%d want %d", updated.ID, target.ID)
	}
	if updated.LoginBuffer != "buffer-new" {
		t.Fatalf("login_buffer 未替换，got %q", updated.LoginBuffer)
	}
	if updated.Nickname == nil || *updated.Nickname != nick {
		t.Fatalf("nickname 未替换，got %v", updated.Nickname)
	}
	if updated.UIN != nil {
		t.Fatalf("uin 必须清空（旧身份的会话产物），got %d", *updated.UIN)
	}
	if updated.Status == nil || *updated.Status != "alive" {
		t.Fatalf("替换后状态应为 alive，got %v", updated.Status)
	}
	// 保留项
	if updated.Remark == nil || *updated.Remark != "主力号-勿删" {
		t.Fatalf("备注必须保留，got %v", updated.Remark)
	}
	if updated.Alias == nil || *updated.Alias != "我的别名" {
		t.Fatalf("别名必须保留（不能被新昵称覆盖），got %v", updated.Alias)
	}
	proxy, err := db.GetAccountProxySetting(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetAccountProxySetting() error = %v", err)
	}
	if proxy == nil || proxy.StaticProxy != "http://proxy.example:8080" {
		t.Fatalf("代理配置必须保留，got %+v", proxy)
	}
	// 旧身份的会话必须清掉
	if _, err := db.GetSession(ctx, target.ID, ""); err == nil {
		t.Fatal("旧会话必须删除")
	}
	// 链接必须被消费掉（同账号 update 链接全部删除）
	if _, err := db.GetAccountLinkByHash(ctx, "hash-1"); err == nil {
		t.Fatal("替换完成后链接必须失效")
	}
	// 账号总数不变
	accounts, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("要求不新增账号，got %d 个", len(accounts))
	}
}

// TestUpdateAccountFromLinkRejectsDuplicateIdentity 验证新身份已属于其他账号时拒绝覆盖。
//
// 两个账号共享同一个 openid 会让保活互相覆盖凭据，最后一起失效，
// 所以这里必须是硬拒绝，而不是覆盖或合并。
func TestUpdateAccountFromLinkRejectsDuplicateIdentity(t *testing.T) {
	db, target, other := newLinkTestDB(t)
	ctx := context.Background()

	link, err := db.CreateAccountLinkForProduct(ctx, "hash-dup", "", "update", target.ID, nil,
		"openid-target", time.Now().Add(10*time.Minute).Unix(), "appstore", false)
	if err != nil {
		t.Fatalf("CreateAccountLinkForProduct() error = %v", err)
	}
	_, err = db.UpdateAccountFromLink(ctx, link, other.OpenID, "buffer-x", nil, nil, nil, nil)
	if !errors.Is(err, ErrAccountIdentityConflict) {
		t.Fatalf("期望 ErrAccountIdentityConflict，got %v", err)
	}
	// 原账号不能被改动
	after, err := db.GetAccount(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if after.LoginBuffer != "buffer-old" {
		t.Fatalf("冲突时原账号不应被修改，got %q", after.LoginBuffer)
	}
}

// TestUpdateAccountFromLinkRejectsStaleLink 验证链接被消费/过期后不能再替换。
func TestUpdateAccountFromLinkRejectsStaleLink(t *testing.T) {
	db, target, _ := newLinkTestDB(t)
	ctx := context.Background()

	// 已过期
	expired, err := db.CreateAccountLinkForProduct(ctx, "hash-expired", "", "update", target.ID, nil,
		"openid-target", time.Now().Add(-time.Minute).Unix(), "appstore", false)
	if err != nil {
		t.Fatalf("CreateAccountLinkForProduct() error = %v", err)
	}
	if _, err := db.UpdateAccountFromLink(ctx, expired, "openid-target", "buffer-new", nil, nil, nil, nil); !errors.Is(err, ErrAccountLinkChanged) {
		t.Fatalf("过期链接应返回 ErrAccountLinkChanged，got %v", err)
	}

	// 已被消费（ConsumeAccountLink 抢到之后再用同一条替换）
	live, err := db.CreateAccountLinkForProduct(ctx, "hash-live", "", "update", target.ID, nil,
		"openid-target", time.Now().Add(10*time.Minute).Unix(), "appstore", false)
	if err != nil {
		t.Fatalf("CreateAccountLinkForProduct() error = %v", err)
	}
	ok, err := db.ConsumeAccountLink(ctx, live.ID)
	if err != nil || !ok {
		t.Fatalf("首次消费应成功，got ok=%v err=%v", ok, err)
	}
	if _, err := db.UpdateAccountFromLink(ctx, live, "openid-target", "buffer-new", nil, nil, nil, nil); !errors.Is(err, ErrAccountLinkChanged) {
		t.Fatalf("已消费链接应返回 ErrAccountLinkChanged，got %v", err)
	}
}

// TestConsumeAccountLinkIsOneShot 验证一次性：并发情况下只有一方能抢到。
func TestConsumeAccountLinkIsOneShot(t *testing.T) {
	db, target, _ := newLinkTestDB(t)
	ctx := context.Background()
	link, err := db.CreateAccountLinkForProduct(ctx, "hash-once", "", "update", target.ID, nil,
		"openid-target", time.Now().Add(10*time.Minute).Unix(), "appstore", false)
	if err != nil {
		t.Fatalf("CreateAccountLinkForProduct() error = %v", err)
	}
	ok, err := db.ConsumeAccountLink(ctx, link.ID)
	if err != nil || !ok {
		t.Fatalf("首次消费应成功，got ok=%v err=%v", ok, err)
	}
	again, err := db.ConsumeAccountLink(ctx, link.ID)
	if err != nil {
		t.Fatalf("ConsumeAccountLink() error = %v", err)
	}
	if again {
		t.Fatal("同一链接不能被消费两次")
	}
}

// TestPutSessionForAccountRejectsReplacedAuthorization 验证迟到的旧流程不能覆盖新身份。
//
// 场景：一次扫码还没走完，运营者用更新链接把账号换成了另一个微信号。
// 此时旧流程拿到的 uin 属于旧身份，写进去会让账号声称自己是别人。
func TestPutSessionForAccountRejectsReplacedAuthorization(t *testing.T) {
	db, target, _ := newLinkTestDB(t)
	ctx := context.Background()

	// 模拟「读到旧快照」
	stale, err := db.GetAccount(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	// 期间账号被替换（login_buffer 变化）
	link, err := db.CreateAccountLinkForProduct(ctx, "hash-cas", "", "update", target.ID, nil,
		"openid-target", time.Now().Add(10*time.Minute).Unix(), "appstore", false)
	if err != nil {
		t.Fatalf("CreateAccountLinkForProduct() error = %v", err)
	}
	if _, err := db.UpdateAccountFromLink(ctx, link, "openid-target", "buffer-replaced", nil, nil, nil, nil); err != nil {
		t.Fatalf("UpdateAccountFromLink() error = %v", err)
	}

	uin := int64(999)
	if err := db.PutSessionForAccount(ctx, stale, uin, map[string]any{"k": "v"}, time.Now().Add(time.Hour).Unix(), ""); !errors.Is(err, ErrAccountAuthorizationChanged) {
		t.Fatalf("旧快照写入会话应被拒绝，got %v", err)
	}
}

// TestMergeAccountAuthorizationKeepsTargetConfig 锁住合并的承诺：
// 保留账号的 ID 与配置不变，只拿走来源的授权，然后删掉来源。
func TestMergeAccountAuthorizationKeepsTargetConfig(t *testing.T) {
	db, target, _ := newLinkTestDB(t)
	ctx := context.Background()

	// 来源账号：刚扫完码，带着可用授权
	source, err := db.UpsertAccount(ctx, "openid-source", "buffer-source", nil, nil, nil, nil,
		map[string]any{"openid": "openid-source", "provider": "appstore", "refreshtoken": "rt"}, statusPtr("alive"))
	if err != nil {
		t.Fatalf("UpsertAccount(source) error = %v", err)
	}
	// 目标账号：运营者已经配好备注与代理
	if _, err := db.SetAccountRemark(ctx, target.ID, "保留我"); err != nil {
		t.Fatalf("SetAccountRemark() error = %v", err)
	}
	if _, err := db.UpsertAccountProxySetting(ctx, target.ID, "static", "http",
		"http://keep.example:8080", "", nil, "", "", "", 600, nil); err != nil {
		t.Fatalf("UpsertAccountProxySetting() error = %v", err)
	}
	uin := int64(1)
	if err := db.PutSession(ctx, target.ID, &uin, map[string]any{"k": "v"}, time.Now().Add(time.Hour).Unix(), ""); err != nil {
		t.Fatalf("PutSession() error = %v", err)
	}
	// 目标上还挂着一条待用链接，身份换了必须一起作废
	if _, err := db.CreateAccountLinkForProduct(ctx, "hash-merge", "", "update", target.ID, nil,
		target.OpenID, time.Now().Add(10*time.Minute).Unix(), "appstore", false); err != nil {
		t.Fatalf("CreateAccountLinkForProduct() error = %v", err)
	}

	// 用「读到的最新快照」执行合并
	srcSnap, err := db.GetAccount(ctx, source.ID)
	if err != nil {
		t.Fatalf("GetAccount(source) error = %v", err)
	}
	tgtSnap, err := db.GetAccount(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetAccount(target) error = %v", err)
	}
	merged, err := db.MergeAccountAuthorization(ctx, srcSnap, tgtSnap)
	if err != nil {
		t.Fatalf("MergeAccountAuthorization() error = %v", err)
	}

	if merged.ID != target.ID {
		t.Fatalf("必须保留目标 ID，got %d", merged.ID)
	}
	if merged.OpenID != "openid-source" || merged.LoginBuffer != "buffer-source" {
		t.Fatalf("应采用来源授权，got openid=%q buffer=%q", merged.OpenID, merged.LoginBuffer)
	}
	if merged.Remark == nil || *merged.Remark != "保留我" {
		t.Fatalf("目标备注必须保留，got %v", merged.Remark)
	}
	proxy, err := db.GetAccountProxySetting(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetAccountProxySetting() error = %v", err)
	}
	if proxy == nil || proxy.StaticProxy != "http://keep.example:8080" {
		t.Fatalf("目标代理必须保留，got %+v", proxy)
	}
	// 来源必须删掉
	if _, err := db.GetAccount(ctx, source.ID); err == nil {
		t.Fatal("来源账号必须被删除")
	}
	// 目标的旧会话与旧链接必须清掉
	if _, err := db.GetSession(ctx, target.ID, ""); err == nil {
		t.Fatal("目标账号的旧会话必须删除")
	}
	if _, err := db.GetAccountLinkByHash(ctx, "hash-merge"); err == nil {
		t.Fatal("目标账号上待用的链接必须作废")
	}
}

// TestMergeAccountAuthorizationRejectsStaleSnapshot 验证预览之后授权被改动时拒绝合并。
//
// 这是「两步式」的核心保护：确认请求携带的是预览时的快照，
// 期间保活刷新过凭据（或别人重新扫过码）就必须要求重新预览。
func TestMergeAccountAuthorizationRejectsStaleSnapshot(t *testing.T) {
	db, target, _ := newLinkTestDB(t)
	ctx := context.Background()
	source, err := db.UpsertAccount(ctx, "openid-s2", "buffer-s2", nil, nil, nil, nil,
		map[string]any{"openid": "openid-s2"}, statusPtr("alive"))
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}
	srcSnap, _ := db.GetAccount(ctx, source.ID)
	tgtSnap, _ := db.GetAccount(ctx, target.ID)

	// 模拟「预览之后，来源被保活刷新了凭据」
	if _, err := db.UpsertAccount(ctx, "openid-s2", "buffer-s2-new", nil, nil, nil, nil,
		map[string]any{"openid": "openid-s2", "refreshtoken": "changed"}, statusPtr("alive")); err != nil {
		t.Fatalf("UpsertAccount(refresh) error = %v", err)
	}
	if _, err := db.MergeAccountAuthorization(ctx, srcSnap, tgtSnap); !errors.Is(err, ErrAccountAuthorizationChanged) {
		t.Fatalf("快照过期应返回 ErrAccountAuthorizationChanged，got %v", err)
	}
}

// TestMergeAccountAuthorizationRejectsUnusableSource 验证不可用授权不能被搬过去。
func TestMergeAccountAuthorizationRejectsUnusableSource(t *testing.T) {
	db, target, _ := newLinkTestDB(t)
	ctx := context.Background()
	// 缺少 login_buffer 的来源
	source, err := db.UpsertAccount(ctx, "openid-empty", "", nil, nil, nil, nil,
		map[string]any{"openid": "openid-empty"}, statusPtr("alive"))
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}
	if _, err := db.MergeAccountAuthorization(ctx, source, target); err == nil {
		t.Fatal("来源授权不可用时应拒绝合并")
	}
	// 同一账号
	if _, err := db.MergeAccountAuthorization(ctx, target, target); err == nil {
		t.Fatal("来源与目标相同时应拒绝")
	}
}
