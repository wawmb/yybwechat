package httpapi

import (
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

// TestRefreshFailureStatus 校验失败原因到账号状态的映射。
func TestRefreshFailureStatus(t *testing.T) {
	now := time.Now()
	rejected := &protocol.RefreshRejectedError{Code: 42007, Message: "refresh_token expired"}
	if got := refreshFailureStatus("alive", protocol.LoginBufferCredentials{ExpiresAt: now.Add(time.Hour).Unix()}, rejected, now); got != "expired" {
		t.Fatalf("被拒绝时应为 expired，实际 %s", got)
	}

	// 凭据还没到期时，瞬时错误保持原状态，避免抖动产生假告警
	transient := errors.New("i/o timeout")
	if got := refreshFailureStatus("alive", protocol.LoginBufferCredentials{ExpiresAt: now.Add(time.Hour).Unix()}, transient, now); got != "alive" {
		t.Fatalf("未过期时应保持原状态，实际 %s", got)
	}

	// 凭据已过期且原因不明，标记 unknown 交给下一轮重试
	if got := refreshFailureStatus("alive", protocol.LoginBufferCredentials{ExpiresAt: now.Add(-time.Hour).Unix()}, transient, now); got != "unknown" {
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
		app.markKeepAliveRetry(id)
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
		{"40188 任意措辞", 40188, "whatever", true},
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

// TestExpiredCredentialsRejectedIsTerminal 复现线上实测到的失败形态。
//
// 实测：拿失效的 refresh token 调刷新接口，返回的是
// `refresh failed: code=-109 msg=RC_PARAMS_INVALID`，而不是标准的 42007。
// 早期实现会把它归入 unknown：凭据明明已经过期、微信侧也明确拒绝，
// 却既不告警也不停止重试，运营者永远等不到「该重扫了」。
func TestExpiredCredentialsRejectedIsTerminal(t *testing.T) {
	now := time.Now()
	observed := &protocol.RefreshRejectedError{Code: -109, Message: "RC_PARAMS_INVALID"}
	expiredCreds := protocol.LoginBufferCredentials{ExpiresAt: now.Add(-2 * time.Hour).Unix()}

	if got := refreshFailureStatus("alive", expiredCreds, observed, now); got != "expired" {
		t.Fatalf("凭据已过期且被微信侧拒绝时应为 expired，实际 %s", got)
	}

	// 凭据尚未过期时不能急着判死：可能只是这次请求参数或环境有问题
	freshCreds := protocol.LoginBufferCredentials{ExpiresAt: now.Add(time.Hour).Unix()}
	if got := refreshFailureStatus("alive", freshCreds, observed, now); got != "alive" {
		t.Fatalf("凭据未过期时应保持原状态，实际 %s", got)
	}

	// 纯网络错误即便凭据已过期也只算 unknown，避免代理抖动制造假告警
	networkErr := errors.New("dial tcp: i/o timeout")
	if got := refreshFailureStatus("alive", expiredCreds, networkErr, now); got != "unknown" {
		t.Fatalf("网络错误应为 unknown，实际 %s", got)
	}

	// login_buffer 阶段的拒绝发生在令牌已换新之后，不能反推旧凭据状态
	bufferRejected := &protocol.LoginBufferRejectedError{Code: -109, Message: "RC_PARAMS_INVALID"}
	if got := refreshFailureStatus("alive", expiredCreds, bufferRejected, now); got != "unknown" {
		t.Fatalf("login_buffer 拒绝应为 unknown，实际 %s", got)
	}
}
