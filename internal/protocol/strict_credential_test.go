package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestCredentialResponseCodeRejectsMalformedShapes 覆盖 v0.2.41 的核心：
// 只有整数（含整数字符串）才能进入成功分支。
//
// 旧实现把响应解到 map 再用 intFromAny 取 code，缺失/null/布尔/小数一律
// 变成 0——而 0 就是"成功"。于是一份格式异常的响应会被当成成功，
// 把里面来路不明的 token 写进库。
func TestCredentialResponseCodeRejectsMalformedShapes(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{"正常整数", `0`, 0, false},
		{"正常拒绝码", `-109`, -109, false},
		{"整数字符串", `"0"`, 0, false},
		{"缺失", ``, 0, true},
		{"null", `null`, 0, true},
		{"布尔", `true`, 0, true},
		{"小数", `0.0`, 0, true},
		{"指数", `1e2`, 0, true},
		{"对象", `{}`, 0, true},
		{"超出 32 位", `99999999999`, 0, true},
		{"非数字字符串", `"ok"`, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := credentialResponseCode(json.RawMessage(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("raw=%q 应报格式错误，实际接受为 %d", tc.raw, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("raw=%q -> (%d, %v), want (%d, nil)", tc.raw, got, err, tc.want)
			}
		})
	}
}

// TestSessionExpiryPrefersServerPSK 覆盖 v0.2.46：
// 会话到期时间以服务端下发的 PSK 为准，不再被本地 30 分钟 TTL 截断。
//
// 被截断的后果是每 30 分钟把一个仍然可用的会话判成过期、强制重新登录。
func TestSessionExpiryPrefersServerPSK(t *testing.T) {
	pshExpiry := time.Now().Add(3 * time.Hour).Unix()
	st := WmpfSession{}
	st.PSK.ExpiredAt = pshExpiry

	if got := sessionExpiresAt(st, 30*time.Minute); got != pshExpiry {
		t.Fatalf("应使用服务端 PSK 到期时间 %d，实际 %d（被本地 TTL 截断了）", pshExpiry, got)
	}

	// 服务端没给期限时才退回本地 TTL
	st.PSK.ExpiredAt = 0
	got := sessionExpiresAt(st, 30*time.Minute)
	want := time.Now().Add(30 * time.Minute).Unix()
	if got < want-5 || got > want+5 {
		t.Fatalf("缺少 PSK 期限时应退回本地 TTL（约 %d），实际 %d", want, got)
	}
}

// TestEmptyLoginCodeDoesNotResetSession 覆盖 v0.2.44：
// 空 code 只报错，不重建会话。
//
// 旧实现遇到空 code 就 Invalidate 会话并重试，等于因为一个应用拿不到 code，
// 把同账号其他应用正在用的会话一起销毁。
func TestEmptyLoginCodeDoesNotResetSession(t *testing.T) {
	invalidated := 0
	_, err := retryLoginCode(context.Background(),
		func() (map[string]any, error) { return map[string]any{"code": ""}, nil },
		func() error { invalidated++; return nil })
	if !errors.Is(err, ErrLoginCodeUnavailable) {
		t.Fatalf("空 code 应返回 ErrLoginCodeUnavailable，实际 %v", err)
	}
	if invalidated != 0 {
		t.Fatalf("空 code 不应重建会话，实际重建了 %d 次", invalidated)
	}

	// 载荷解析失败才是「会话真坏了」，允许重建一次
	calls, invalidated := 0, 0
	result, err := retryLoginCode(context.Background(),
		func() (map[string]any, error) {
			calls++
			if calls == 1 {
				return nil, ErrShortlinkPayload
			}
			return map[string]any{"code": "0123456789abcdef"}, nil
		},
		func() error { invalidated++; return nil })
	if err != nil || result["code"] != "0123456789abcdef" {
		t.Fatalf("载荷失败后重建会话应成功取到 code，实际 %v / %v", result, err)
	}
	if invalidated != 1 || calls != 2 {
		t.Fatalf("载荷失败应重建一次并重试一次，实际 rebuild=%d calls=%d", invalidated, calls)
	}
}
