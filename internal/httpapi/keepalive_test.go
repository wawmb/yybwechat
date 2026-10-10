package httpapi

import (
	"errors"
	"testing"
	"time"

	"yybwechat/internal/store"
)

// TestDefinitiveCredentialFailure 校验「凭据彻底失效」的启发式判定：
// 明确命中微信拒绝码或失效描述时才认定为彻底失效，网络抖动一律不算。
func TestDefinitiveCredentialFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil 错误不算失效", nil, false},
		{"网络超时不算失效", errors.New("dial tcp 10.0.0.1:443: i/o timeout"), false},
		{"连接被重置不算失效", errors.New("read: connection reset by peer"), false},
		{"42007 + refresh_token 判为失效", errors.New("errcode 42007: refresh_token expired"), true},
		{"40188 + invalid scope 判为失效", errors.New("errcode 40188, invalid scope"), true},
		{"明确要求重新扫码判为失效", errors.New("请重新登录"), true},
		{"失效+凭据同时命中判为失效", errors.New("access_token 已过期"), true},
		{"仅失效词但无凭据词不算", errors.New("request expired"), false},
		{"仅凭据词但无失效词不算", errors.New("token fetch busy"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := definitiveCredentialFailure(tc.err); got != tc.want {
				t.Fatalf("definitiveCredentialFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
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
