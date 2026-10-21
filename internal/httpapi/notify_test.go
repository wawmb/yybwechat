package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"yybwechat/internal/protocol"
	"yybwechat/internal/store"
)

// webhookRecorder 是一个本地 Webhook 接收端，用来断言「到底有没有推出去」。
type webhookRecorder struct {
	mu       sync.Mutex
	payloads []map[string]any
	status   int
}

func newWebhookRecorder(t *testing.T) (*webhookRecorder, *httptest.Server) {
	t.Helper()
	rec := &webhookRecorder{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		code := rec.status
		rec.mu.Unlock()

		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if code == http.StatusOK {
			rec.mu.Lock()
			rec.payloads = append(rec.payloads, payload)
			rec.mu.Unlock()
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func (r *webhookRecorder) setStatus(code int) {
	r.mu.Lock()
	r.status = code
	r.mu.Unlock()
}

func (r *webhookRecorder) received() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, len(r.payloads))
	copy(out, r.payloads)
	return out
}

// newNotifyTestApp 准备一个带账号与本地 Webhook 的 App。
func newNotifyTestApp(t *testing.T, webhookURL string) (*App, *store.WechatAccount) {
	t.Helper()
	t.Setenv("GIN_MODE", "test")
	app, err := NewApp(Config{
		ResourceRoot:   t.TempDir(),
		RequestTimeout: time.Second,
		AvatarTimeout:  time.Second,
		SessionTTL:     time.Minute,
		QRSessionTTL:   time.Minute,
	})
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	// 凭据已过期：expires_at 在过去，refresh token 仍然存在
	expiredCreds := map[string]any{
		"openid":                    "o_expired_1",
		"refreshtoken":              "rt-value",
		"accesstoken":               "at-value",
		"expires_at":                time.Now().Add(-2 * time.Hour).Unix(),
		"expires_in":                7200,
		"refresh_token_observed_at": time.Now().Add(-40 * 24 * time.Hour).Unix(),
	}
	nick := "过期账号"
	status := "alive"
	acc, err := app.db.UpsertAccount(context.Background(), "o_expired_1", "lb",
		nil, &nick, nil, map[string]any{"nick_name": nick}, expiredCreds, &status)
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}
	app.setWebhookURL(webhookURL)
	return app, acc
}

// TestExpiredCredentialPushesWebhook 是「凭据过期会不会推送」的主用例。
//
// 复现的正是历史 bug 的现场：账号状态还是 alive，但凭据已经越过 expires_at，
// 刷新被判为彻底失效。断言状态落库、Webhook 真的收到、已通知标记同步更新。
func TestExpiredCredentialPushesWebhook(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	// 校验前置条件：凭据确实已过期，且刷新判定会认为「该刷新了」
	creds := protocol.CredentialsFromMap(acc.Credentials)
	if !credentialsDueForRefresh(creds, time.Now(), time.Minute) {
		t.Fatalf("前置条件不成立：凭据应处于待刷新状态")
	}
	if got := accountStatus(acc); got != "alive" {
		t.Fatalf("accountStatus() = %q, want alive", got)
	}

	// 模拟 refreshAccount 的失效分支：先落状态，再发通知
	if err := app.setAccountStatus(context.Background(), acc.ID, "expired"); err != nil {
		t.Fatalf("setAccountStatus() error = %v", err)
	}
	app.notifyIfStatusChanged(acc, "alive", "expired")

	got := rec.received()
	if len(got) != 1 {
		t.Fatalf("Webhook 收到 %d 条推送, want 1", len(got))
	}
	if got[0]["current"] != "expired" {
		t.Errorf("current = %v, want expired", got[0]["current"])
	}
	if got[0]["previous"] != "alive" {
		t.Errorf("previous = %v, want alive", got[0]["previous"])
	}

	// 已通知标记必须落库，否则下一轮会重复推送
	pending, err := app.db.ListPendingNotifications(context.Background())
	if err != nil {
		t.Fatalf("ListPendingNotifications() error = %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("推送成功后仍存在 %d 条待补推记录: %+v", len(pending), pending)
	}
}

// TestFailedPushIsRetried 验证推送失败不会吞掉告警。
//
// 这是「过期了但没收到推送」的第二类原因：状态已落库为 expired，此后保活与
// 健康检查都会跳过该账号，若那次推送失败且不重试，告警就永久丢失。
func TestFailedPushIsRetried(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	// 第一次推送：接收端返回 500
	rec.setStatus(http.StatusInternalServerError)
	if err := app.setAccountStatus(context.Background(), acc.ID, "expired"); err != nil {
		t.Fatalf("setAccountStatus() error = %v", err)
	}
	app.notifyIfStatusChanged(acc, "alive", "expired")

	if got := rec.received(); len(got) != 0 {
		t.Fatalf("接收端返回 500 时不应记录成功推送，实收 %d 条", len(got))
	}
	pending, err := app.db.ListPendingNotifications(context.Background())
	if err != nil {
		t.Fatalf("ListPendingNotifications() error = %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("推送失败后待补推记录 = %d, want 1", len(pending))
	}
	if pending[0].Status != "expired" || pending[0].NotifiedStatus != "alive" {
		t.Errorf("待补推记录 = %+v, want alive → expired", pending[0])
	}

	// 接收端恢复后补推
	rec.setStatus(http.StatusOK)
	app.retryPendingNotifications(context.Background())

	got := rec.received()
	if len(got) != 1 {
		t.Fatalf("补推后 Webhook 收到 %d 条, want 1", len(got))
	}
	if got[0]["current"] != "expired" {
		t.Errorf("补推 current = %v, want expired", got[0]["current"])
	}
	pending, err = app.db.ListPendingNotifications(context.Background())
	if err != nil {
		t.Fatalf("ListPendingNotifications() error = %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("补推成功后仍有 %d 条待补推", len(pending))
	}

	// 幂等：再补推一轮不应重复打扰
	app.retryPendingNotifications(context.Background())
	if got := rec.received(); len(got) != 1 {
		t.Errorf("重复补推产生了额外推送，累计 %d 条", len(got))
	}
}

// TestNoWebhookConfiguredSkipsRetry 确认未配置 Webhook 时不会堆积无意义的补推记录。
func TestNoWebhookConfiguredSkipsRetry(t *testing.T) {
	app, acc := newNotifyTestApp(t, "")

	if err := app.setAccountStatus(context.Background(), acc.ID, "expired"); err != nil {
		t.Fatalf("setAccountStatus() error = %v", err)
	}
	app.notifyIfStatusChanged(acc, "alive", "expired")
	// 未配置 Webhook：不应把记录标成已通知，但补推在无地址时会直接返回
	app.retryPendingNotifications(context.Background())

	pending, err := app.db.ListPendingNotifications(context.Background())
	if err != nil {
		t.Fatalf("ListPendingNotifications() error = %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("待补推记录 = %d, want 1（等待运营者补齐 Webhook 后补推）", len(pending))
	}
}

// TestUpsertAccountDoesNotNotifyOnScan 确认扫码入库不会触发「恢复」推送。
func TestUpsertAccountDoesNotNotifyOnScan(t *testing.T) {
	app, _ := newNotifyTestApp(t, "")

	pending, err := app.db.ListPendingNotifications(context.Background())
	if err != nil {
		t.Fatalf("ListPendingNotifications() error = %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("刚扫码入库就产生了 %d 条待补推记录（会给每个新账号推一条无意义的恢复通知）", len(pending))
	}
}

// TestExpiredCredentialNetworkFailureAlerts 覆盖「凭据已过期 + 刷新失败原因只是
// 网络/代理」的情形。
//
// 这类账号状态为了能自愈会留在 unknown 继续重试，但「续不上」这个事实必须推送：
// 否则一个配错的代理就能让账号静默瘫掉，运营者永远等不到消息。
func TestExpiredCredentialNetworkFailureAlerts(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	creds := protocol.CredentialsFromMap(acc.Credentials)
	if creds.ExpiresAt > time.Now().Unix() {
		t.Fatalf("前置条件不成立：凭据应已过期")
	}

	app.notifyRenewalFailure(acc, "unknown", creds, errors.New("dial tcp 10.0.0.1:443: i/o timeout"))

	got := rec.received()
	if len(got) != 1 {
		t.Fatalf("Webhook 收到 %d 条, want 1", len(got))
	}
	if got[0]["event"] != "renewal_failed" {
		t.Errorf("event = %v, want renewal_failed", got[0]["event"])
	}
	if reason, _ := got[0]["reason"].(string); reason == "" {
		t.Error("告警应带上失败原因，便于判断是网络问题还是账号问题")
	}

	// 同一故障周期内不重复打扰
	app.notifyRenewalFailure(acc, "unknown", creds, errors.New("still broken"))
	if n := len(rec.received()); n != 1 {
		t.Errorf("同一故障周期内重复告警，累计 %d 条", n)
	}
}

// TestRenewalAlertRetriesUntilDelivered 确认告警发送失败后会持续重试。
func TestRenewalAlertRetriesUntilDelivered(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)
	creds := protocol.CredentialsFromMap(acc.Credentials)

	rec.setStatus(http.StatusServiceUnavailable)
	app.notifyRenewalFailure(acc, "unknown", creds, errors.New("proxy unreachable"))
	if last, err := app.db.ExpiryAlertedAt(context.Background(), acc.ID); err != nil || last != 0 {
		t.Fatalf("推送失败时不应记账, expiry_alerted_at=%d (err=%v)", last, err)
	}

	rec.setStatus(http.StatusOK)
	app.notifyRenewalFailure(acc, "unknown", creds, errors.New("proxy unreachable"))
	if n := len(rec.received()); n != 1 {
		t.Fatalf("恢复后应补发 1 条, 实收 %d", n)
	}
	if last, err := app.db.ExpiryAlertedAt(context.Background(), acc.ID); err != nil || last == 0 {
		t.Fatalf("送达后应记账, expiry_alerted_at=%d (err=%v)", last, err)
	}
}

// TestRenewalAlertSkipsWhenCredentialStillValid 确认凭据未过期时不告警。
func TestRenewalAlertSkipsWhenCredentialStillValid(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	fresh := protocol.LoginBufferCredentials{ExpiresAt: time.Now().Add(time.Hour).Unix()}
	app.notifyRenewalFailure(acc, "alive", fresh, errors.New("i/o timeout"))
	if n := len(rec.received()); n != 0 {
		t.Fatalf("凭据未过期时不该告警，实收 %d 条", n)
	}
}

// TestRenewalAlertRemindsAfterInterval 确认长期瘫痪会按间隔再次提醒。
func TestRenewalAlertRemindsAfterInterval(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)
	creds := protocol.CredentialsFromMap(acc.Credentials)

	// 假装 7 小时前已经告警过
	past := time.Now().Add(-7 * time.Hour).Unix()
	if err := app.db.MarkExpiryAlerted(context.Background(), acc.ID, past); err != nil {
		t.Fatalf("MarkExpiryAlerted() error = %v", err)
	}
	app.notifyRenewalFailure(acc, "unknown", creds, errors.New("still broken"))
	if n := len(rec.received()); n != 1 {
		t.Fatalf("超过重提醒间隔应再次提醒，实收 %d 条", n)
	}

	// 续期成功后标记应清零，下个故障周期重新开始
	if err := app.db.ClearExpiryAlerted(context.Background(), acc.ID); err != nil {
		t.Fatalf("ClearExpiryAlerted() error = %v", err)
	}
	if last, err := app.db.ExpiryAlertedAt(context.Background(), acc.ID); err != nil || last != 0 {
		t.Fatalf("清除后 expiry_alerted_at=%d (err=%v), want 0", last, err)
	}
}

// TestExpiredAccountAlertsOnRefresh 锁定用户要的行为：
// 「只要是 expired 的账号，刷新时就一定要推送」。
//
// 过去 expired 是终态，保活与健康检查都直接 continue，而状态跃迁式通知
// 又只认 alive → expired 这一次变化；一旦跃迁没被捕捉到，账号就永久沉默。
func TestExpiredAccountAlertsOnRefresh(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	// 让账号处于终态：模拟「升级前就已失效」或「上一轮判死」
	ctx := context.Background()
	if err := app.db.SetAccountStatus(ctx, acc.ID, "expired"); err != nil {
		t.Fatalf("SetAccountStatus() error = %v", err)
	}

	// 保活路径：非强制刷新，命中 expired 早退分支
	if _, _, err := app.refreshAccount(ctx, acc, false); err != nil {
		t.Fatalf("refreshAccount() error = %v", err)
	}
	got := rec.received()
	if len(got) != 1 {
		t.Fatalf("失效账号刷新后推送 %d 条, want 1", len(got))
	}
	if got[0]["event"] != "account_expired" && got[0]["msgtype"] == nil {
		if got[0]["current"] != "expired" {
			t.Fatalf("推送内容不是失效告警: %+v", got[0])
		}
	}

	// 紧接着再刷一轮：重提醒窗口内不应重复打扰
	if _, _, err := app.refreshAccount(ctx, acc, false); err != nil {
		t.Fatalf("second refreshAccount() error = %v", err)
	}
	if got = rec.received(); len(got) != 1 {
		t.Fatalf("窗口内重复推送：%d 条, want 1", len(got))
	}

	// 超过重提醒窗口后仍未重扫：必须再推一次，不能响过一声就沉默
	if err := app.db.MarkExpiryAlerted(ctx, acc.ID,
		time.Now().Add(-expiredAlertInterval-time.Minute).Unix()); err != nil {
		t.Fatalf("MarkExpiryAlerted() error = %v", err)
	}
	if _, _, err := app.refreshAccount(ctx, acc, false); err != nil {
		t.Fatalf("third refreshAccount() error = %v", err)
	}
	if got = rec.received(); len(got) != 2 {
		t.Fatalf("超过重提醒窗口后推送 %d 条, want 2", len(got))
	}
}

// TestExpiredAccountAlertsDuringScanLoop 验证后台扫描不会漏掉失效账号。
//
// 这是「保活/健康检查直接 continue 掉 expired」那个旧分支的回归测试：
// 跳过刷新可以，但跳过告警不行。
func TestExpiredAccountAlertsDuringScanLoop(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	ctx := context.Background()
	if err := app.db.SetAccountStatus(ctx, acc.ID, "expired"); err != nil {
		t.Fatalf("SetAccountStatus() error = %v", err)
	}

	app.refreshDueAccounts()

	if got := rec.received(); len(got) != 1 {
		t.Fatalf("后台扫描对失效账号推送 %d 条, want 1", len(got))
	}
	// 已记账，下一轮扫描不应重复
	app.refreshDueAccounts()
	if got := rec.received(); len(got) != 1 {
		t.Fatalf("第二轮扫描重复推送：%d 条, want 1", len(got))
	}
	_ = acc
}

// TestManualRefreshOnExpiredAccountAlerts 覆盖用户最常走的这条路：
// 在控制台点「刷新存活」。
//
// 手动刷新是 force=true，会跳过 expired 早退分支真的去撞一次网络，
// 而网络失败会把状态判成 unknown——跃迁式通知不会触发，历史上就是「点了刷新、
// 页面转一圈、什么都没收到」。这里断言：只要点的是失效账号，就一定发失效告警。
func TestManualRefreshOnExpiredAccountAlerts(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	if err := app.db.SetAccountStatus(context.Background(), acc.ID, "expired"); err != nil {
		t.Fatalf("SetAccountStatus() error = %v", err)
	}

	// 用一个几乎立刻到期的 ctx 模拟「刷新请求失败」，避免测试真的访问外网
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, _ = app.refreshAccount(ctx, acc, true)

	got := rec.received()
	if len(got) == 0 {
		t.Fatalf("手动刷新失效账号后一条告警都没发")
	}
	events := make([]string, 0, len(got))
	for _, payload := range got {
		if ev, _ := payload["event"].(string); ev != "" {
			events = append(events, ev)
		} else if _, ok := payload["markdown_v2"]; ok {
			events = append(events, "wework_card")
		}
	}
	// 允许「状态变更」与「失效告警」二选一，但必须是失效相关的告警
	sawExpiredAlert := false
	for _, payload := range got {
		if payload["event"] == "account_expired" || payload["current"] == "expired" {
			sawExpiredAlert = true
		}
	}
	if !sawExpiredAlert {
		t.Fatalf("推送内容为 %v / 事件 %v，want 含 account_expired", got, events)
	}

	// 紧接着再点一次：重提醒窗口内不应重复打扰
	if _, _, err := app.refreshAccount(context.Background(), acc, false); err != nil {
		t.Fatalf("second refreshAccount() error = %v", err)
	}
	if n := len(rec.received()); n != len(got) {
		t.Fatalf("窗口内重复推送：%d 条 → %d 条", len(got), n)
	}
}
