package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// requests 统计收到的请求次数（含失败的），用于断言「失败后有没有立刻重试」
	requests int
	// sequence 是预设的响应码队列，用完后退回 status
	sequence []int
}

func newWebhookRecorder(t *testing.T) (*webhookRecorder, *httptest.Server) {
	t.Helper()
	rec := &webhookRecorder{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		code := rec.status
		if len(rec.sequence) > 0 {
			code = rec.sequence[0]
			rec.sequence = rec.sequence[1:]
		}
		rec.requests++
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

// setStatusSequence 预设接下来几次请求的响应码。
func (r *webhookRecorder) setStatusSequence(codes ...int) {
	r.mu.Lock()
	r.sequence = codes
	r.mu.Unlock()
}

// requestCount 返回累计收到的请求次数（含被拒绝的）。
func (r *webhookRecorder) requestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
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

	// 紧接着再点一次：重提醒窗口内不应重复打扰。
	//
	// 这次不强制刷新，走的是「按有效期判断」的常规路径；账号此时可能已被上一次
	// 网络失败改成待确认，于是这里真的会再去撞一次网络并失败——这属于预期的自愈重试，
	// 本用例只关心「有没有重复推送」。
	_, _, _ = app.refreshAccount(context.Background(), acc, false)
	if n := len(rec.received()); n != len(got) {
		t.Fatalf("窗口内重复推送：%d 条 → %d 条", len(got), n)
	}
}

// TestManualStatusChangeToExpiredPushesOnce 覆盖控制台「手动标记失效」这条路。
//
// 手动置为 expired 会推一条状态变更通知。如果只写 status_notified 而忘了写
// expiry_alerted_at，下一轮保活扫描的 alertExpired 会认为「还没告警过」，
// 立刻再推一条内容重复的「账号已失效」——运营者连着收到两条同一件事。
func TestManualStatusChangeToExpiredPushesOnce(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	body := `{"ref":"` + acc.OpenID + `","status":"expired"}`
	req := httptest.NewRequest(http.MethodPost, "/accounts/status", strings.NewReader(body))
	w := httptest.NewRecorder()
	app.handleAccountStatus(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /accounts/status = %d %s", w.Code, w.Body.String())
	}

	first := rec.received()
	if len(first) != 1 {
		t.Fatalf("手动标记失效应推 1 条，实际 %d 条: %v", len(first), first)
	}

	// 下一轮扫描会调用 alertExpired：已记账，必须保持沉默
	app.alertExpired(acc)
	if n := len(rec.received()); n != len(first) {
		t.Fatalf("手动标记失效后重复推送：%d 条 → %d 条", len(first), n)
	}
}

// TestManualStatusAliveClearsExpiryWindow 覆盖「手动置回可用」这条路。
//
// 恢复可用意味着上一轮故障周期结束。若不清 expiry_alerted_at，
// 它再次失效时 alertExpired 会被上一轮遗留的 2 小时窗口静音，
// 只剩「状态变更」一条通知，重提醒彻底消失。
func TestManualStatusAliveClearsExpiryWindow(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	ctx := context.Background()
	if err := app.db.SetAccountStatus(ctx, acc.ID, "expired"); err != nil {
		t.Fatalf("SetAccountStatus() error = %v", err)
	}
	if err := app.db.MarkExpiryAlerted(ctx, acc.ID, time.Now().Unix()); err != nil {
		t.Fatalf("MarkExpiryAlerted() error = %v", err)
	}

	body := `{"ref":"` + acc.OpenID + `","status":"alive"}`
	req := httptest.NewRequest(http.MethodPost, "/accounts/status", strings.NewReader(body))
	w := httptest.NewRecorder()
	app.handleAccountStatus(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /accounts/status = %d %s", w.Code, w.Body.String())
	}
	if n := len(rec.received()); n != 1 {
		t.Fatalf("expired → alive 应推 1 条恢复通知，实际 %d 条", n)
	}

	last, err := app.db.ExpiryAlertedAt(ctx, acc.ID)
	if err != nil {
		t.Fatalf("ExpiryAlertedAt() error = %v", err)
	}
	if last != 0 {
		t.Fatalf("手动置回 alive 未清理告警窗口：expiry_alerted_at = %d", last)
	}
}

// TestStartupAlertsPushExpiredAccount 覆盖「服务重启」这条最容易漏报的路径。
//
// 两个后台定时器的第一次触发都要等满一个间隔（健康检查默认 30 分钟），
// 若被配置成 0 则永远不触发。重启期间已经失效的账号、停机前没送达的告警，
// 只能靠启动时这一轮补推发出去。
func TestStartupAlertsPushExpiredAccount(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	ctx := context.Background()
	if err := app.db.SetAccountStatus(ctx, acc.ID, "expired"); err != nil {
		t.Fatalf("SetAccountStatus() error = %v", err)
	}
	// 模拟「停机前已经推送过跃迁通知，但失效重提醒还没发过」
	if err := app.db.MarkStatusNotified(ctx, acc.ID, "expired"); err != nil {
		t.Fatalf("MarkStatusNotified() error = %v", err)
	}
	if err := app.db.ClearExpiryAlerted(ctx, acc.ID); err != nil {
		t.Fatalf("ClearExpiryAlerted() error = %v", err)
	}

	app.pushStartupAlerts()

	got := rec.received()
	if len(got) != 1 {
		t.Fatalf("启动补推应发 1 条失效告警，实际 %d 条: %v", len(got), got)
	}
	if got[0]["event"] != "account_expired" {
		t.Fatalf("启动补推事件 = %v, want account_expired", got[0]["event"])
	}

	// 再启动一次：2 小时窗口内不应重复打扰
	app.pushStartupAlerts()
	if n := len(rec.received()); n != 1 {
		t.Fatalf("重启重复推送：1 条 → %d 条", n)
	}
}

// TestBackoffSkipStillAlertsExpiredCredentials 覆盖「失败退避期间」的静默窗口。
//
// 账号进入退避后，扫描会直接跳过、不再调用 refreshAccount，
// 因此也不会再有 notifyRenewalFailure 的机会。如果第一次告警没送达，
// 账号就会一直不可用却收不到任何消息——这条用例锁住「跳过路径也要检查过期」。
func TestBackoffSkipStillAlertsExpiredCredentials(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	ctx := context.Background()
	if err := app.db.SetAccountStatus(ctx, acc.ID, "unknown"); err != nil {
		t.Fatalf("SetAccountStatus() error = %v", err)
	}
	// 进入退避窗口：下一次可重试时间在未来一小时
	app.markKeepAliveRetry(acc.ID, false)

	if !app.keepAliveShouldSkip(acc) {
		t.Fatalf("前置条件不成立：账号应处于退避跳过状态")
	}

	app.refreshDueAccounts()

	got := rec.received()
	if len(got) != 1 {
		t.Fatalf("退避期间应发 1 条续期告警，实际 %d 条: %v", len(got), got)
	}
	if got[0]["event"] != "renewal_failed" {
		t.Fatalf("退避告警事件 = %v, want renewal_failed", got[0]["event"])
	}
	// 必须说明「账号当前不可用」，不能只写个原因让运营者自己去猜
	reason, _ := got[0]["reason"].(string)
	if !strings.Contains(reason, "不可用") {
		t.Fatalf("退避告警原因 = %q, want 含「不可用」", reason)
	}
}

// TestWebhookRetriesImmediatelyOnServerError 验证一次推送失败后会立刻重试。
//
// 定时器下一轮要等 60 秒，一次网络抖动就让掉线告警晚一分钟到达。
// 5xx 属于接收端暂时不可用，值得立刻再试一次；重试成功后不再留给下一轮。
func TestWebhookRetriesImmediatelyOnServerError(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	// 第一次 500，第二次 200
	rec.setStatusSequence(http.StatusInternalServerError, http.StatusOK)

	ok := app.notifyWebhookExpired(acc)
	if !ok {
		t.Fatalf("5xx 后重试应当成功送达")
	}
	if n := rec.requestCount(); n != 2 {
		t.Fatalf("请求次数 = %d, want 2（一次失败 + 一次重试）", n)
	}
	if n := len(rec.received()); n != 1 {
		t.Fatalf("送达消息数 = %d, want 1", n)
	}
}

// TestWebhookDoesNotRetryOnBusinessRejection 验证「接收端明确拒绝」不重试。
//
// 企业微信 errcode≠0（如 45009 频率限制）重试也会被同样拒绝，
// 白白拖长扫描时间，交给下一轮定时任务即可。
func TestWebhookDoesNotRetryOnBusinessRejection(t *testing.T) {
	app, acc := newNotifyTestApp(t, "")

	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		_, _ = w.Write([]byte(`{"errcode":45009,"errmsg":"api freq out of limit"}`))
	}))
	defer srv.Close()
	app.setWebhookURL(srv.URL)

	if ok := app.notifyWebhookExpired(acc); ok {
		t.Fatalf("errcode≠0 不应被当成送达")
	}
	if attempts != 1 {
		t.Fatalf("业务拒绝后请求次数 = %d, want 1（不重试）", attempts)
	}
}

// TestUnusableAlertIgnoresHealthyAccount 防止「宁可多推」变成刷屏。
//
// 凭据仍在有效期内的账号是健康的：它可能只是没到刷新时间。
// 这类账号一旦被当成异常推送，每轮扫描都会刷一条，告警很快就没人看了。
func TestUnusableAlertIgnoresHealthyAccount(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, _ := newNotifyTestApp(t, srv.URL)

	freshCreds := map[string]any{
		"openid":       "o_fresh",
		"refreshtoken": "rt-value",
		"accesstoken":  "at-value",
		"expires_at":   time.Now().Add(2 * time.Hour).Unix(),
	}
	nick := "健康账号"
	status := "alive"
	acc, err := app.db.UpsertAccount(context.Background(), "o_fresh", "lb",
		nil, &nick, nil, nil, freshCreds, &status)
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}

	app.alertUnusableCredentials(acc, "alive")
	if n := len(rec.received()); n != 0 {
		t.Fatalf("健康账号不该被推送，实际推了 %d 条", n)
	}
}

// TestMissingCredentialsAlerts 覆盖「完全没有凭据」的账号。
//
// 这类账号连刷新都发起不了，永远不会自愈，也没有任何网络动作会暴露问题。
// 不主动提醒的话，它只会安静地留在列表里，谁都不知道它其实用不了。
func TestMissingCredentialsAlerts(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, _ := newNotifyTestApp(t, srv.URL)

	nick := "缺凭据账号"
	status := "unknown"
	acc, err := app.db.UpsertAccount(context.Background(), "o_nocred", "",
		nil, &nick, nil, nil, nil, &status)
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}

	app.alertUnusableCredentials(acc, "unknown")

	got := rec.received()
	if len(got) != 1 {
		t.Fatalf("缺少凭据应发 1 条告警，实际 %d 条", len(got))
	}
	reason, _ := got[0]["reason"].(string)
	if !strings.Contains(reason, "缺少登录凭据") {
		t.Fatalf("告警原因 = %q, want 含「缺少登录凭据」", reason)
	}

	// 2 小时窗口内不重复
	app.alertUnusableCredentials(acc, "unknown")
	if n := len(rec.received()); n != 1 {
		t.Fatalf("缺少凭据告警重复推送：1 条 → %d 条", n)
	}
}

// TestRefreshFailureChainEndToEnd 走完整的 refreshAccount 判定链，而不是直接调通知函数。
//
// 覆盖的是真实链路：读库 → 解析代理 → 发起刷新（必然超时失败）→ 判定状态 →
// 落库 → 决定要不要推送。这条链上任何一环写错（比如把错误吞掉、把状态判成
// alive、或提前 return 掉通知），这里都会红。
func TestRefreshFailureChainEndToEnd(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	// 凭据 2 小时前就过期了，刷新又拿不到网络：账号此刻确实不可用
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	status, refreshed, err := app.refreshAccount(ctx, acc, true)
	if err == nil {
		t.Fatalf("刷新应当失败（测试环境访问不到微信服务器）")
	}
	if refreshed {
		t.Fatalf("刷新失败却报告了续期成功")
	}
	if status == "expired" {
		t.Fatalf("网络失败不是凭据失效的证据，不应判死，得到 %q", status)
	}

	got := rec.received()
	if len(got) == 0 {
		t.Fatalf("凭据已过期且刷新失败，却没有推送任何告警")
	}
	events := make([]any, 0, len(got))
	sawAlert := false
	for _, payload := range got {
		events = append(events, payload["event"])
		if payload["event"] == "renewal_failed" || payload["event"] == "account_expired" {
			sawAlert = true
		}
	}
	if !sawAlert {
		t.Fatalf("推送事件 = %v, want 含 renewal_failed 或 account_expired", events)
	}

	// 状态必须落库为「待确认」，后续还能继续自愈重试
	stored, gerr := app.db.GetAccount(context.Background(), acc.ID)
	if gerr != nil {
		t.Fatalf("GetAccount() error = %v", gerr)
	}
	if got := accountStatus(stored); got != status {
		t.Fatalf("落库状态 = %q, 返回状态 = %q, 两者应一致", got, status)
	}
}

// TestWeworkCardPayloadsAreValid 校验企业微信卡片的结构。
//
// 这条分支本地跑不到（要真的 qyapi 地址才会走），但它是实际部署里最常用的格式：
// msgtype 写错、外层 key 与 msgtype 不一致，企业微信会回 errcode=40008，
// 而我们的日志只会记录「推送被拒绝」——消息压根到不了人。
// 所以这里把四个卡片的 JSON 结构、关键字段和长度上限都钉住。
func TestWeworkCardPayloadsAreValid(t *testing.T) {
	nick := "测试昵称"
	acc := &store.WechatAccount{ID: 7, OpenID: "o_card_1", Nickname: &nick}

	cases := []struct {
		name    string
		body    []byte
		mustHas []string
	}{
		{"状态变更卡片", buildWeworkPayload("status_changed", "alive", "expired", acc), []string{"账号状态变更", "测试昵称", "o_card_1", "alive", "expired"}},
		{"失效提醒卡片", buildWeworkExpiredPayload(acc), []string{"账号已失效", "测试昵称", "o_card_1", "expired"}},
		{"续期失败卡片", buildWeworkRenewalPayload(acc, "unknown", "刷新请求超时"), []string{"账号无法续期", "测试昵称", "o_card_1", "刷新请求超时"}},
		{"测试推送卡片", buildWeworkTestPayload(), []string{"Webhook 测试", "yybwechat"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var parsed map[string]any
			if err := json.Unmarshal(tc.body, &parsed); err != nil {
				t.Fatalf("不是合法 JSON: %v, body=%s", err, tc.body)
			}
			if parsed["msgtype"] != "markdown_v2" {
				t.Fatalf("msgtype = %v, want markdown_v2", parsed["msgtype"])
			}
			// 企业微信要求内层 key 与 msgtype 同名，写错就是 40008
			payload, ok := parsed["markdown_v2"].(map[string]any)
			if !ok {
				t.Fatalf("缺少与 msgtype 同名的 markdown_v2 对象: %v", parsed)
			}
			content, _ := payload["content"].(string)
			if content == "" {
				t.Fatalf("content 为空")
			}
			// markdown_v2 content 上限 4096 字节，超了会被拒
			if len([]byte(content)) > 4096 {
				t.Fatalf("content 长度 %d 字节，超过企业微信 4096 上限", len([]byte(content)))
			}
			for _, want := range tc.mustHas {
				if !strings.Contains(content, want) {
					t.Fatalf("content 缺少 %q:/n%s", want, content)
				}
			}
		})
	}
}

// TestIsWeworkWebhook 确认企业微信地址识别不会误判。
func TestIsWeworkWebhook(t *testing.T) {
	cases := map[string]bool{
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx":       true,
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/upload_media?key=x": false,
		"https://oapi.dingtalk.com/robot/send?access_token=x":            false,
		"http://127.0.0.1:8080/hook":                                     false,
	}
	for url, want := range cases {
		if got := isWeworkWebhook(url); got != want {
			t.Fatalf("isWeworkWebhook(%q) = %v, want %v", url, got, want)
		}
	}
}

// TestExpiredAlertWindowIsTwoHours 锁定「失效重提醒间隔 = 2 小时」。
//
// 用「1 小时前推过」和「3 小时前推过」两个时间点卡边界：
// 间隔若是 6 小时，3 小时那次会被窗口挡住而漏推，这里就会红。
func TestExpiredAlertWindowIsTwoHours(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	ctx := context.Background()
	if err := app.db.SetAccountStatus(ctx, acc.ID, "expired"); err != nil {
		t.Fatalf("SetAccountStatus() error = %v", err)
	}

	// 1 小时前刚推过：还在 2 小时窗口内，不该重复打扰
	if err := app.db.MarkExpiryAlerted(ctx, acc.ID, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatalf("MarkExpiryAlerted() error = %v", err)
	}
	app.alertExpired(acc)
	if n := len(rec.received()); n != 0 {
		t.Fatalf("1 小时前推过，窗口内不应重复推送，实际 %d 条", n)
	}

	// 3 小时前推过：已超过 2 小时窗口，必须再提醒一次
	if err := app.db.MarkExpiryAlerted(ctx, acc.ID, time.Now().Add(-3*time.Hour).Unix()); err != nil {
		t.Fatalf("MarkExpiryAlerted() error = %v", err)
	}
	app.alertExpired(acc)
	if n := len(rec.received()); n != 1 {
		t.Fatalf("3 小时前推过，应再提醒 1 条，实际 %d 条", n)
	}
}

// TestProxyResolveFailureAlertsWhenExpired 覆盖「代理解析失败」这条静默路径。
//
// 动态代理提取失败（额度用完、白名单变了、供应商接口挂了）会让账号从此刷不动。
// 以前这里只把状态落成 unknown 就返回，不发任何通知——运营者只会看到账号
// 安静地变成「待确认」，凭据到期了也不知道。凭据未到期时不应打扰，
// 所以这里同时验证「未到期不推、已到期必推」。
func TestProxyResolveFailureAlertsWhenExpired(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	// 用一个必定失败的提取 API（返回 500），让 resolveAccountProxy 稳定失败；
	// 不用「连不上的端口」是因为它可能撞上客户端超时，错误类型会漂移。
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer api.Close()
	body := accountProxyIn{Mode: "api", APIURL: api.URL + "/extract", ProxyType: "http"}
	normalized, spec, err := app.normalizeAccountProxyInput(context.Background(), body)
	if err != nil {
		t.Fatalf("normalizeAccountProxyInput() error = %v", err)
	}
	if _, err := app.saveAccountProxyInput(context.Background(), acc.ID, normalized, spec); err != nil {
		t.Fatalf("saveAccountProxyInput() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, refreshErr := app.refreshAccount(ctx, acc, true)
	if refreshErr == nil || !strings.Contains(refreshErr.Error(), "resolve account proxy") {
		t.Fatalf("refreshAccount() error = %v, want 含 resolve account proxy", refreshErr)
	}

	got := rec.received()
	if len(got) != 1 {
		t.Fatalf("凭据已过期时代理解析失败应推 1 条告警，实际 %d 条", len(got))
	}
	if got[0]["event"] != "renewal_failed" {
		t.Fatalf("event = %v, want renewal_failed", got[0]["event"])
	}
	// 原因来自底层的提取错误（如「请求代理 API 失败: ...」），要能看出是代理问题
	reason, _ := got[0]["reason"].(string)
	if !strings.Contains(reason, "代理") {
		t.Fatalf("告警原因应提到代理问题，实际 %q", reason)
	}
}

// TestMissingCredentialsRefreshAlerts 覆盖「账号没有凭据」时手动刷新的路径。
//
// 这条以前也只是落库成 unknown 就返回，用户点「刷新存活」看不到任何反馈、
// 也收不到任何告警，而这类账号是永远自愈不了的。
func TestMissingCredentialsRefreshAlerts(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, _ := newNotifyTestApp(t, srv.URL)

	nick := "缺凭据"
	status := "unknown"
	acc, err := app.db.UpsertAccount(context.Background(), "o_nocred_2", "",
		nil, &nick, nil, nil, nil, &status)
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}

	if _, _, err := app.refreshAccount(context.Background(), acc, true); err == nil {
		t.Fatalf("无凭据账号刷新应当返回错误")
	}

	got := rec.received()
	if len(got) != 1 {
		t.Fatalf("无凭据账号刷新应推 1 条告警，实际 %d 条", len(got))
	}
	reason, _ := got[0]["reason"].(string)
	if !strings.Contains(reason, "缺少登录凭据") {
		t.Fatalf("告警原因 = %q, want 含「缺少登录凭据」", reason)
	}
}

// TestManualRefreshKeepsExpiredStatus 锁定「已判死账号刷新失败不降级」。
//
// 以前点「刷新存活」会把 expired 改成 unknown：卡片在「需重扫 / 待确认」之间
// 来回跳，账号还会重新进入重试队列、再撞三次 -109 才回到终态。
// 失败不构成推翻终态的证据——想救回来只有「刷新成功」这一条路。
func TestManualRefreshKeepsExpiredStatus(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	ctx := context.Background()
	if err := app.db.SetAccountStatus(ctx, acc.ID, "expired"); err != nil {
		t.Fatalf("SetAccountStatus() error = %v", err)
	}

	// 刷新必然失败（凭据是假的），用短超时避免真的等网络
	callCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	status, _, err := app.refreshAccount(callCtx, acc, true)
	if err == nil {
		t.Fatalf("刷新应当失败")
	}
	if status != "expired" {
		t.Fatalf("refreshAccount() 返回 %q，已判死账号刷新失败应保持 expired", status)
	}

	stored, getErr := app.db.GetAccount(ctx, acc.ID)
	if getErr != nil {
		t.Fatalf("GetAccount() error = %v", getErr)
	}
	if got := accountStatus(stored); got != "expired" {
		t.Fatalf("落库状态 = %q，不应被降级", got)
	}

	// 仍然要发声：失效告警不能被吞
	if n := len(rec.received()); n == 0 {
		t.Fatalf("已失效账号刷新失败后应仍推送失效告警")
	}
}

// TestAlertWindowSkipIsLogged 锁定「窗口内跳过要留日志」。
//
// 真实踩过：用户问「为什么没推送」，日志里一条 [notify] 都没有，
// 因为窗口内跳过是完全静默的——「没推」和「被去重」在日志里长得一模一样。
func TestAlertWindowSkipIsLogged(t *testing.T) {
	rec, srv := newWebhookRecorder(t)
	app, acc := newNotifyTestApp(t, srv.URL)

	ctx := context.Background()
	if err := app.db.SetAccountStatus(ctx, acc.ID, "expired"); err != nil {
		t.Fatalf("SetAccountStatus() error = %v", err)
	}
	// 第一次：正常推送并记账
	app.alertExpired(acc)
	if n := len(rec.received()); n != 1 {
		t.Fatalf("首次应推 1 条，实际 %d 条", n)
	}

	old := log.Writer()
	defer log.SetOutput(old)
	var buf bytes.Buffer
	log.SetOutput(&buf)

	// 第二次：窗口内跳过，必须留下说明
	app.alertExpired(acc)
	if n := len(rec.received()); n != 1 {
		t.Fatalf("窗口内不应重复推送，实际 %d 条", n)
	}
	if !strings.Contains(buf.String(), "在去重窗口内") {
		t.Fatalf("窗口内跳过应打日志，实际日志:\n%s", buf.String())
	}

	// 同一账号一小时内不再重复刷这行日志
	buf.Reset()
	app.alertExpired(acc)
	if strings.Contains(buf.String(), "在去重窗口内") {
		t.Fatalf("跳过日志应节流，第二次不应再打:\n%s", buf.String())
	}
}
