package httpapi

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"yybwechat/internal/protocol"
	"yybwechat/internal/store"
)

// TestDefinitiveCredentialFailure 校验「凭据彻底失效」的判定。
//
// 判定基于接口返回的结构化业务码：只有能证明是微信侧拒绝时才认定失效。
// 传输层错误（超时、代理握手失败、JSON 解析失败）即使文本里带 token expired
// 也不算，因为那可能是代理供应商自己的措辞。
func TestDefinitiveCredentialFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil 错误不算失效", nil, false},
		{"网络超时不算失效", errors.New("dial tcp 10.0.0.1:443: i/o timeout"), false},
		{"连接被重置不算失效", errors.New("read: connection reset by peer"), false},
		// 传输层错误即便文本像凭据失效，也不能据此判账号失效
		{"代理返回 token expired 不算失效", errors.New("HTTP 502: proxy token expired"), false},
		{"缺少 refresh token 判为失效", protocol.ErrMissingRefreshToken, true},
		{"42007 + refresh_token 判为失效",
			&protocol.RefreshRejectedError{Code: 42007, Message: "refresh_token expired"}, true},
		{"40188 + invalid scope 判为失效",
			&protocol.LoginBufferRejectedError{Code: 40188, Message: "invalid scope"}, true},
		{"明确要求重新登录判为失效",
			&protocol.RefreshRejectedError{Code: -109, Message: "请重新登录"}, true},
		{"中文刷新令牌失效判为失效",
			&protocol.RefreshRejectedError{Code: -109, Message: "刷新令牌已失效"}, true},
		// 线上实测：失效/被轮换的令牌在这套接口上返回 -109 + RC_PARAMS_INVALID。
		// 它不含 refresh token 与失效字样，措辞匹配抓不住；但字面语义是「参数不合法」，
		// 我们自己构造请求出错时也会命中，所以不在这里判终态，
		// 交由 refreshFailureStatus 的连续计数收敛（见 TestRepeatedParamsInvalidRejectionConverges）
		{"-109 RC_PARAMS_INVALID 不在此判失效",
			&protocol.RefreshRejectedError{Code: -109, Message: "RC_PARAMS_INVALID"}, false},
		{"仅 access_token 过期不算失效",
			&protocol.RefreshRejectedError{Code: -109, Message: "access_token expired"}, false},
		{"仅失效词但无 refresh token 不算",
			&protocol.RefreshRejectedError{Code: -109, Message: "request expired"}, false},
		{"仅 refresh token 无失效词不算",
			&protocol.RefreshRejectedError{Code: -109, Message: "refresh token busy"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := definitiveCredentialFailure(tc.err); got != tc.want {
				t.Fatalf("definitiveCredentialFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestDefinitiveCredentialFailureWrapped 校验错误被上层包装后仍能识别。
func TestDefinitiveCredentialFailureWrapped(t *testing.T) {
	err := fmt.Errorf("refresh account: %w",
		&protocol.RefreshRejectedError{Code: 42007, Message: "refresh_token exception"})
	if !definitiveCredentialFailure(err) {
		t.Fatal("包装后的 42007 仍应判为失效")
	}
}

// TestAccountWithoutCredentialsBecomesRescanRequired 锁住用户实际遇到的场景：
// 扫码中断留下的残缺账号（credentials 为空）在列表里必须显示「需重扫」，
// 而不是「待确认」。
//
// 「无凭据」是终态——刷新连输入都没有，任何重试都不可能成功。以前记成 unknown，
// 健康检查于是每两分钟重试一次、永远失败在同一行，而「建议重扫」要靠凭据里的
// 到期时间推导，没有凭据就永远点不亮，账号看上去只是在"等网络恢复"。
func TestAccountWithoutCredentialsBecomesRescanRequired(t *testing.T) {
	_, srv := newWebhookRecorder(t)
	app, _ := newNotifyTestApp(t, srv.URL)
	ctx := context.Background()

	// 建一个没有凭据的账号：模拟扫码没走完就退出的残留
	nick := "残缺账号"
	status := "unknown"
	acc, err := app.db.UpsertAccount(ctx, "o_no_creds", "", nil, &nick, nil,
		map[string]any{"nick_name": nick}, nil, &status)
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}

	gotStatus, _, err := app.refreshAccount(ctx, acc, false)
	if err == nil {
		t.Fatal("无凭据账号刷新应返回错误")
	}
	if gotStatus != "expired" {
		t.Fatalf("无凭据账号状态 = %s, want expired", gotStatus)
	}

	current, err := app.db.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if !current.Public().RescanRecommended {
		t.Fatal("无凭据账号必须提示重新扫码")
	}
	// 判死后必须给出可诊断的原因，否则界面只有一个"需重扫"，看不出为什么
	if reason := app.lastRefreshError(acc.ID); reason == "" {
		t.Fatal("判死后应保留失败原因")
	}
}

// TestDefinitiveCredentialFailureCoversLoginBufferScope 锁住真实线上形态：
// 取票据返回 -101，消息里才是微信的业务码 [40188] [invalid scope]。
//
// 这条是「凭据能续期但取不了码」的唯一证据来源——外层 -101 是通用错误码，
// 只有消息里的 scope 码能证明这套授权已经不具备小程序能力。
func TestDefinitiveCredentialFailureCoversLoginBufferScope(t *testing.T) {
	rejected := &protocol.LoginBufferRejectedError{
		Code:    -101,
		Message: "GetLoginBuffer error [40188] [invalid scope, rid: 6ae40805-1a47e41d-18c833cf] || [0] []",
	}
	if !definitiveCredentialFailure(rejected) {
		t.Fatal("40188 invalid scope 应判为终态失效")
	}
	if !definitiveCredentialFailure(fmt.Errorf("fetch login buffer: %w", rejected)) {
		t.Fatal("包装后仍应判为终态失效")
	}
	// 只有 -101 而没有 scope 结论时不能判死：网络抖动同样是这个外层码
	if definitiveCredentialFailure(&protocol.LoginBufferRejectedError{Code: -101, Message: "request timeout"}) {
		t.Fatal("缺少 scope 证据的 -101 不应判死")
	}
}

// TestRefreshFailureStatus 校验失败原因到账号状态的映射。
func TestRefreshFailureStatus(t *testing.T) {
	now := time.Now()
	rejected := &protocol.RefreshRejectedError{Code: 42007, Message: "refresh_token expired"}
	if got := refreshFailureStatus("alive", protocol.LoginBufferCredentials{ExpiresAt: now.Add(time.Hour).Unix()}, rejected, now, 0); got != "expired" {
		t.Fatalf("被拒绝时应为 expired，实际 %s", got)
	}

	// 凭据还没到期时，瞬时错误保持原状态，避免抖动产生假告警
	transient := errors.New("i/o timeout")
	if got := refreshFailureStatus("alive", protocol.LoginBufferCredentials{ExpiresAt: now.Add(time.Hour).Unix()}, transient, now, 0); got != "alive" {
		t.Fatalf("未过期时应保持原状态，实际 %s", got)
	}

	// 凭据已过期且原因不明，标记 unknown 交给下一轮重试
	if got := refreshFailureStatus("alive", protocol.LoginBufferCredentials{ExpiresAt: now.Add(-time.Hour).Unix()}, transient, now, 0); got != "unknown" {
		t.Fatalf("已过期且原因不明时应为 unknown，实际 %s", got)
	}
}

// TestCredentialsDueForRefresh 校验续期窗口的判断，含 ExpiresAt 未知的兜底。
func TestCredentialsDueForRefresh(t *testing.T) {
	now := time.Now()
	ahead := 45 * time.Minute
	if !credentialsDueForRefresh(protocol.LoginBufferCredentials{ExpiresAt: 0}, now, ahead) {
		t.Fatal("有效期未知时应按需要刷新处理")
	}
	if credentialsDueForRefresh(protocol.LoginBufferCredentials{ExpiresAt: now.Add(2 * time.Hour).Unix()}, now, ahead) {
		t.Fatal("剩余 2 小时不应进入续期窗口")
	}
	if !credentialsDueForRefresh(protocol.LoginBufferCredentials{ExpiresAt: now.Add(10 * time.Minute).Unix()}, now, ahead) {
		t.Fatal("剩余 10 分钟应进入续期窗口")
	}
}

// TestShouldNotifyStatusChange 校验只有收敛到终态（alive/expired）才推送 Webhook，
// unknown 这类「本轮无法判定」的中间态不应制造告警噪音。
func TestShouldNotifyStatusChange(t *testing.T) {
	cases := []struct {
		old, new string
		want     bool
	}{
		{"alive", "expired", true},
		{"unknown", "alive", true},
		{"alive", "unknown", false},
		{"unknown", "expired", true},
		{"alive", "alive", false},
		{"", "alive", false},
	}
	for _, tc := range cases {
		if got := shouldNotifyStatusChange(tc.old, tc.new); got != tc.want {
			t.Fatalf("shouldNotifyStatusChange(%q, %q) = %v, want %v", tc.old, tc.new, got, tc.want)
		}
	}
}

// TestCredentialExpiresIn 校验剩余有效期的计算，无法判断时返回 0。
func TestCredentialExpiresIn(t *testing.T) {
	acc := &store.WechatAccount{ID: 1, OpenID: "o-test"}

	acc.Credentials = nil
	if got := credentialExpiresIn(acc); got != 0 {
		t.Fatalf("无凭据时期望 0，实际 %d", got)
	}

	acc.Credentials = map[string]any{"expires_at": float64(0)}
	if got := credentialExpiresIn(acc); got != 0 {
		t.Fatalf("无有效期时期望 0，实际 %d", got)
	}

	future := time.Now().Add(2 * time.Hour).Unix()
	acc.Credentials = map[string]any{"expires_at": float64(future)}
	got := credentialExpiresIn(acc)
	if got <= 0 || got > 2*3600 {
		t.Fatalf("剩余有效期应在 (0, 7200] 秒内，实际 %d", got)
	}
}

// TestKeepAliveBackoffEscalates 校验连续失败时退避按 2 倍递增并封顶。
func TestKeepAliveBackoffEscalates(t *testing.T) {
	app := &App{
		keepAliveNext:  map[int64]time.Time{},
		keepAliveFails: map[int64]int{},
	}
	const id = int64(7)

	// 期望序列（单位：分钟）：5 → 10 → 20 → 40 → 60(封顶) → 60 → 60 → 60
	wantMinutes := []int64{5, 10, 20, 40, 60, 60, 60, 60}
	for i, want := range wantMinutes {
		app.markKeepAliveRetry(id, false)
		got := int64(time.Until(app.keepAliveNext[id]).Round(time.Minute) / time.Minute)
		if got != want {
			t.Fatalf("第 %d 次退避 = %d 分钟，期望 %d 分钟", i+1, got, want)
		}
	}
	if app.keepAliveFails[id] != len(wantMinutes) {
		t.Fatalf("连续失败计数 = %d，期望 %d", app.keepAliveFails[id], len(wantMinutes))
	}

	// 刷新成功后应同时清除退避时间与失败计数
	app.clearKeepAliveRetry(id)
	if _, ok := app.keepAliveNext[id]; ok {
		t.Fatal("清除后不应残留退避时间")
	}
	if _, ok := app.keepAliveFails[id]; ok {
		t.Fatal("清除后不应残留失败计数")
	}
}

// TestDefinitiveRefreshMessageIgnoresWordingForStandardCodes 校验标准业务码
// 本身就是结论，不再依赖 errmsg 的措辞。
//
// 背景：errmsg 可能是空的、本地化的，或换了一版文案；早期实现要求 errmsg 里
// 必须出现 "refresh token"，一旦措辞变化，真实失效就会被漏判成 unknown，
// 账号按退避无限重试却永远不告警。
func TestDefinitiveRefreshMessageIgnoresWordingForStandardCodes(t *testing.T) {
	cases := []struct {
		name string
		code int
		msg  string
		want bool
	}{
		{"42007 空 errmsg", 42007, "", true},
		{"42007 无关措辞", 42007, "系统繁忙", true},
		{"42007 标准措辞", 42007, "refresh_token expired", true},
		// 40030 / 42002 是 v0.2.39 补进来的已知失效码：
		// 官方 check-login 有时只回 "error 42007 | 42007"，靠措辞会漏判成正常。
		{"40030 refresh_token 无效", 40030, "", true},
		{"42002 token 失效", 42002, "", true},
		{"check-login 只回错误码串", 0, "error 40030 | 40030", true},
		// 40188 只在措辞明确指向 scope 时才算结论；裸码继续保留重试（宁可漏判也不误杀）
		{"40188 明确 invalid scope", 40188, "invalid scope", true},
		{"40188 裸码不判死", 40188, "whatever", false},
		{"其它码仍要求措辞命中", -109, "系统繁忙", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := definitiveRefreshMessage(tc.code, tc.msg); got != tc.want {
				t.Fatalf("definitiveRefreshMessage(%d, %q) = %v, want %v", tc.code, tc.msg, got, tc.want)
			}
		})
	}
}

// TestRejectionWithoutEvidenceStaysRetryable 锁定「没有服务端证据就不判死」。
//
// 实测：拿失效的 refresh token 调刷新接口，返回的是
// `refresh failed: code=-109 msg=RC_PARAMS_INVALID`，而不是标准的 42007。
//
// 但本地凭据到期只是推断、不是证据；-109 的字面语义是「参数不合法」，
// 我们自己构造请求出错时同样会命中。因此单次出现一律落到可重试状态，
// 只有连续命中达到阈值、或服务端给出明确失效语义时才判终态。
func TestRejectionWithoutEvidenceStaysRetryable(t *testing.T) {
	now := time.Now()
	observed := &protocol.RefreshRejectedError{Code: -109, Message: "RC_PARAMS_INVALID"}
	expiredCreds := protocol.LoginBufferCredentials{ExpiresAt: now.Add(-2 * time.Hour).Unix()}
	freshCreds := protocol.LoginBufferCredentials{ExpiresAt: now.Add(time.Hour).Unix()}

	// 凭据已过期 + 单次参数无效拒绝 → unknown，继续自愈重试
	if got := refreshFailureStatus("alive", expiredCreds, observed, now, 0); got != "unknown" {
		t.Fatalf("凭据已过期但只有单次参数无效拒绝时应为 unknown，实际 %s", got)
	}
	// 凭据未过期 → 维持原状态，本轮不告警
	if got := refreshFailureStatus("alive", freshCreds, observed, now, 0); got != "alive" {
		t.Fatalf("凭据未过期时应保持原状态，实际 %s", got)
	}

	// 纯网络错误即便凭据已过期也只算 unknown，避免代理抖动制造假告警
	networkErr := errors.New("dial tcp: i/o timeout")
	if got := refreshFailureStatus("alive", expiredCreds, networkErr, now, 0); got != "unknown" {
		t.Fatalf("网络错误应为 unknown，实际 %s", got)
	}

	// login_buffer 阶段的拒绝发生在令牌已换新之后，不能反推旧凭据状态
	bufferRejected := &protocol.LoginBufferRejectedError{Code: -109, Message: "RC_PARAMS_INVALID"}
	if got := refreshFailureStatus("alive", expiredCreds, bufferRejected, now, 0); got != "unknown" {
		t.Fatalf("login_buffer 拒绝应为 unknown，实际 %s", got)
	}
}

// TestRepeatedParamsInvalidRejectionConverges 校验「参数无效」码的连续收敛。
//
// 单次 -109 RC_PARAMS_INVALID 不足以判死（可能是我们自己构造请求的问题），
// 但连续命中同一个账号、同一个形态，就是服务端在反复说「不认这套凭据」。
// 连续计数是这条路径唯一的收敛手段——否则账号会一直挂到 expires_at 才报。
func TestRepeatedParamsInvalidRejectionConverges(t *testing.T) {
	now := time.Now()
	observed := &protocol.RefreshRejectedError{Code: -109, Message: "RC_PARAMS_INVALID"}
	freshCreds := protocol.LoginBufferCredentials{ExpiresAt: now.Add(time.Hour).Unix()}

	// 未达阈值前一律保持原状态：单次拒绝不足以排除偶发异常
	for n := 1; n < credentialRejectLimit; n++ {
		if got := refreshFailureStatus("alive", freshCreds, observed, now, n); got != "alive" {
			t.Fatalf("第 %d 次被拒（未达阈值 %d）应保持原状态，实际 %s", n, credentialRejectLimit, got)
		}
	}
	// 凭据仍未过期时，即便连续被拒也不判死：-109 可能来自我们自己的请求构造，
	// 而续期窗口内重试只有几十秒间隔，一次抖动就能凑满阈值。
	// 判死最多推迟到凭据真实过期（默认只差 4 分钟），换的是零误杀。
	if got := refreshFailureStatus("alive", freshCreds, observed, now, credentialRejectLimit); got != "alive" {
		t.Fatalf("凭据未过期时连续 %d 次被拒不应判死，实际 %s", credentialRejectLimit, got)
	}
	// 凭据真的过期之后，阈值一到就收敛为 expired
	expiredCreds := protocol.LoginBufferCredentials{ExpiresAt: now.Add(-time.Minute).Unix()}
	if got := refreshFailureStatus("alive", expiredCreds, observed, now, credentialRejectLimit); got != "expired" {
		t.Fatalf("凭据已过期且连续被拒应收敛为 expired，实际 %s", got)
	}

	// 网络错误再多次也不能收敛：拿不到业务码就没有「服务端不认凭据」的证据，
	// 否则一次代理故障连撞几次就会把活着的账号钉死
	if got := refreshFailureStatus("alive", freshCreds, errors.New("i/o timeout"), now, credentialRejectLimit); got != "alive" {
		t.Fatalf("传输层错误不应因计数收敛，实际 %s", got)
	}

	// 同一个码但消息不是「参数无效」（系统繁忙、限流等）不参与这条计数，
	// 否则一次服务端抖动连撞几次就会把活着的账号钉死
	busy := &protocol.RefreshRejectedError{Code: -109, Message: "系统繁忙"}
	if got := refreshFailureStatus("alive", freshCreds, busy, now, credentialRejectLimit); got != "alive" {
		t.Fatalf("非「参数无效」的拒绝不应收敛，实际 %s", got)
	}
}

// TestCredentialRejectionCounter 校验连续计数只在「同类拒绝」时累加。
func TestCredentialRejectionCounter(t *testing.T) {
	app := &App{credentialRejects: map[int64]int{}}
	const id = int64(7)
	rejected := &protocol.RefreshRejectedError{Code: -109, Message: "RC_PARAMS_INVALID"}

	if n := app.noteCredentialRejection(id, rejected); n != 1 {
		t.Fatalf("首次疑似拒绝计数 = %d，期望 1", n)
	}
	if n := app.noteCredentialRejection(id, rejected); n != 2 {
		t.Fatalf("第二次疑似拒绝计数 = %d，期望 2", n)
	}
	// 失败原因换成网络错误：连续性被打断，计数必须归零，
	// 否则「两次旧拒绝 + 一次新故障」会被拼成三次而误判终态
	if n := app.noteCredentialRejection(id, errors.New("i/o timeout")); n != 0 {
		t.Fatalf("失败原因变化后计数 = %d，期望 0", n)
	}
	if n := app.noteCredentialRejection(id, rejected); n != 1 {
		t.Fatalf("原因变化后应重新从 1 计数，实际 %d", n)
	}

	app.clearCredentialRejections(id)
	if _, ok := app.credentialRejects[id]; ok {
		t.Fatal("清除后不应残留计数")
	}
}

// TestDefinitiveFailureWalksFullErrorChain 覆盖 v0.2.43。
//
// refresh 与 check-login 是两段独立原因，两段都用 %w 包在一条链里。
// 旧实现用 errors.As（只取第一个匹配）且第一段用 %v 拍平成字符串，
// 于是「refresh 明确 42007、check-login 含糊 -109」这种组合会漏判成可重试，
// 本该判死的账号一直挂着，既不提示重扫也不收敛。
func TestDefinitiveFailureWalksFullErrorChain(t *testing.T) {
	rejected42007 := &protocol.RefreshRejectedError{Code: 42007, Message: "refresh token expired", Operation: "refresh_token"}
	ambiguous109 := &protocol.RefreshRejectedError{Code: -109, Message: "RC_PARAMS_INVALID", Operation: "check_login"}
	checkLogin42007 := &protocol.RefreshRejectedError{Code: -109, Message: "error 42007 | 42007", Operation: "check_login"}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"单独明确拒绝", rejected42007, true},
		{"refresh 明确 + check-login 含糊", fmt.Errorf("%w; check login: %w", rejected42007, ambiguous109), true},
		{"refresh 含糊 + check-login 明确", fmt.Errorf("%w; check login: %w", ambiguous109, checkLogin42007), true},
		{"两段都含糊则继续重试", fmt.Errorf("%w; check login: %w", ambiguous109, ambiguous109), false},
		{"纯传输错误不判死", errors.New("dial tcp: i/o timeout"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := definitiveCredentialFailure(tc.err); got != tc.want {
				t.Fatalf("definitiveCredentialFailure() = %v, want %v（err=%v）", got, tc.want, tc.err)
			}
		})
	}
}
