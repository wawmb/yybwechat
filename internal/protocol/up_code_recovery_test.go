package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"yybwechat/internal/store"
)

func TestRejectedCodeRecoveryCommitsOnlyValidatedSession(t *testing.T) {
	for _, outcome := range []string{"success", "ticket_rejected", "new_session_rejected", "empty_code", "route_mismatch", "rescan", "credential_rotation", "manual_expiry"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(filepath.Join(t.TempDir(), "yyb.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			status := "alive"
			acc, err := db.UpsertAccount(ctx, "synthetic-repair-account", "saved-buffer", nil, nil, nil, nil, map[string]any{"accesstoken": "synthetic-access", "refresh_token_observed_at": 123}, &status)
			if err != nil {
				t.Fatal(err)
			}
			st := WmpfSession{Session: AppSession{UIN: 1234, SendKey: []byte("send"), RecvKey: []byte("recv")},
				PSK: pskEntry{PSKType: 1, PreSharedKey: "001122", TicketEntry: "aabbcc", ExpiredAt: time.Now().Add(time.Hour).Unix()}}
			if err = db.PutSession(ctx, acc.ID, nil, st.toBlob(), st.PSK.ExpiredAt, ""); err != nil {
				t.Fatal(err)
			}
			before, _ := db.GetSession(ctx, acc.ID, "")
			beforeJSON, _ := json.Marshal(before.SessionBlob)
			pool := NewPool(DefaultConfig(), db)
			calls := 0
			probe := func(context.Context) (WmpfSession, map[string]any, error) {
				calls++
				st.Session.UIN = 5678
				switch outcome {
				case "ticket_rejected":
					return st, nil, errors.New("synthetic ticket refusal")
				case "new_session_rejected":
					return st, nil, &ShortlinkRejectedError{Code: -14}
				case "empty_code":
					return st, map[string]any{"code": ""}, nil
				case "route_mismatch":
					st.TCPProxy = "socks5://example.invalid:1080"
				case "rescan":
					if err := db.SetAccountCredential(ctx, acc.ID, "new-scan-buffer", map[string]any{"accesstoken": "new-scan"}); err != nil {
						t.Fatal(err)
					}
				case "credential_rotation":
					if _, err := db.ApplyCredentialRefresh(ctx, acc, acc.LoginBuffer, map[string]any{"accesstoken": "new-key"}, "alive"); err != nil {
						t.Fatal(err)
					}
				case "manual_expiry":
					if err := db.SetAccountStatus(ctx, acc.ID, "expired"); err != nil {
						t.Fatal(err)
					}
				}
				return st, map[string]any{"code": "synthetic-code"}, nil
			}
			original := &ShortlinkRejectedError{Code: -14}
			result, err := pool.recoverCodeSession(ctx, acc, "wx-synthetic-app", "", original, probe)
			if outcome == "success" {
				if err != nil || result["code"] != "synthetic-code" {
					t.Fatal("validated code session did not commit", err)
				}
				stored, _ := db.GetSession(ctx, acc.ID, "")
				got, parseErr := sessionFromBlob(stored.SessionBlob)
				if parseErr != nil || got.Session.UIN != 5678 {
					t.Fatal("validated replacement session missing")
				}
				account, _ := db.GetAccount(ctx, acc.ID)
				if !account.SameAuthorization(acc) || account.LoginBuffer != acc.LoginBuffer {
					t.Fatal("session repair changed authorization")
				}
			} else {
				var repair *CodeSessionRecoveryError
				if err == nil || !errors.As(err, &repair) || !errors.Is(err, original) {
					t.Fatal("repair failure lost the original rejection")
				}
				stored, cacheErr := db.GetSession(ctx, acc.ID, "")
				// Manual expiry clears credentials; it may also remove the cache.
				if outcome != "manual_expiry" {
					afterJSON, _ := json.Marshal(stored.SessionBlob)
					if cacheErr != nil || string(afterJSON) != string(beforeJSON) {
						t.Fatal("failed candidate overwrote the original cache")
					}
				}
			}
			// 失败之后必须进冷却：同一账号+出口 5 分钟内不再重复重建。
			// success 例外——恢复成功会把退避清零（见
			// TestSuccessfulCodeRecoveryClearsCooldown），否则账号会退化成
			// 「每 5 分钟才允许修一次」，表现为长时间连续失败。
			if outcome != "rescan" && outcome != "credential_rotation" && outcome != "manual_expiry" && outcome != "success" {
				_, _ = pool.recoverCodeSession(ctx, acc, "wx-synthetic-app", "", original, probe)
				if calls != 1 {
					t.Fatal("repeated rejection bypassed repair cooldown")
				}
			}
		})
	}
}

func TestSessionOnlyCodeRejectionCannotStartNewLogin(t *testing.T) {
	status := store.StatusSessionOnly
	acc := &store.WechatAccount{ID: 1, Status: &status}
	pool := NewPool(DefaultConfig(), nil)
	original := &ShortlinkRejectedError{Code: -14}
	_, err := pool.recoverCodeSession(context.Background(), acc, "wx-synthetic-app", "", original, func(context.Context) (WmpfSession, map[string]any, error) {
		t.Fatal("rejected authorization attempted a new session")
		return WmpfSession{}, nil, nil
	})
	if !errors.Is(err, original) {
		t.Fatal("limited request lost its error")
	}
}

// TestSuccessfulCodeRecoveryClearsCooldown 守住「成功即解退避」这条策略。
//
// 背景（现场故障）：账号缓存会话已被微信侧作废，每次取码都是应用层 -14。
// 修复本身能成功，但如果成功后仍留着 5 分钟退避，就会出现
// 「第一次成功 → 之后 5 分钟全部 -14」的观感，用户侧等价于「5 分钟只能取一次码」。
// 退避只应该压「连续失败」，不该压已经修好的账号。
func TestSuccessfulCodeRecoveryClearsCooldown(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "yyb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	status := "alive"
	acc, err := db.UpsertAccount(ctx, "synthetic-cooldown-account", "saved-buffer", nil, nil, nil, nil,
		map[string]any{"accesstoken": "synthetic-access", "refresh_token_observed_at": 123}, &status)
	if err != nil {
		t.Fatal(err)
	}
	st := WmpfSession{Session: AppSession{UIN: 1234, SendKey: []byte("send"), RecvKey: []byte("recv")},
		PSK: pskEntry{PSKType: 1, PreSharedKey: "001122", TicketEntry: "aabbcc", ExpiredAt: time.Now().Add(time.Hour).Unix()}}
	if err = db.PutSession(ctx, acc.ID, nil, st.toBlob(), st.PSK.ExpiredAt, ""); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(DefaultConfig(), db)
	original := &ShortlinkRejectedError{Code: -14}
	calls := 0
	probe := func(context.Context) (WmpfSession, map[string]any, error) {
		calls++
		return st, map[string]any{"code": "synthetic-code"}, nil
	}
	if _, err = pool.recoverCodeSession(ctx, acc, "wx-synthetic-app", "", original, probe); err != nil {
		t.Fatal("successful repair rejected", err)
	}
	// 修复成功后该目标的退避必须被摘掉，否则会退化成「每 5 分钟只能取一次码」。
	// 注意不能再用「紧接着的第二次 -14 能不能重建」来断言：v0.2.60 起，
	// 会话刚验证成功时的 -14 会被判成目标级拒绝而主动跳过重建（见下一个测试）。
	key := fmt.Sprintf("%d\x00%s\x00%s", acc.ID, "", "wx-synthetic-app")
	pool.mu.Lock()
	_, armed := pool.codeRepairs[key]
	pool.mu.Unlock()
	if armed {
		t.Fatal("successful repair left the cooldown armed")
	}
	if calls != 1 {
		t.Fatalf("repair calls = %d, want 1", calls)
	}
	// 换个 AppID 仍可立即恢复：退避是「账号+出口+AppID」粒度，不是全局开关。
	// 先清掉「刚成功」标记，否则 2 分钟窗口会先一步跳过重建，测不到退避粒度。
	pool.mu.Lock()
	pool.verified = map[string]verifiedSession{}
	pool.mu.Unlock()
	if _, err = pool.recoverCodeSession(ctx, acc, "wx-other-app", "", original, probe); err != nil {
		t.Fatal("another AppID was blocked by the previous target's cooldown", err)
	}
	if calls != 2 {
		t.Fatalf("repair calls = %d, want 2", calls)
	}
}

// TestRecentlyVerifiedSessionSkipsAnotherLogin 守住 v0.2.60 的 2 分钟窗口：
// 会话刚在这条出口上成功过，那么紧接着的 -14 只可能是这个 AppID 自己被拒，
// 再做一次 ManualAuth 帮不上忙，只会让上游多记一次登录。
func TestRecentlyVerifiedSessionSkipsAnotherLogin(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "yyb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	status := "alive"
	acc, err := db.UpsertAccount(ctx, "synthetic-verified-account", "saved-buffer", nil, nil, nil, nil,
		map[string]any{"accesstoken": "synthetic-access"}, &status)
	if err != nil {
		t.Fatal(err)
	}
	st := WmpfSession{
		Session: AppSession{UIN: 1234, SendKey: []byte("send"), RecvKey: []byte("recv")},
		PSK:     pskEntry{PSKType: 1, PreSharedKey: "001122", TicketEntry: "aabbcc", ExpiredAt: time.Now().Add(time.Hour).Unix()},
	}
	if err := db.PutSession(ctx, acc.ID, nil, st.toBlob(), st.PSK.ExpiredAt, ""); err != nil {
		t.Fatal(err)
	}
	pool := NewPool(DefaultConfig(), db)
	// 「刚验证过」现在带会话指纹：必须记录的就是缓存里那一条，
	// 只按账号+路径记会让重建出来的新会话白白继承旧记录。
	pool.markSessionVerified(acc, acc.ID, st)

	calls := 0
	probe := func(context.Context) (WmpfSession, map[string]any, error) {
		calls++
		return WmpfSession{}, map[string]any{"code": "synthetic-code"}, nil
	}
	original := &ShortlinkRejectedError{Code: -14}
	if _, err := pool.recoverCodeSession(ctx, acc, "wx-synthetic-app", "", original, probe); !errors.Is(err, original) {
		t.Fatalf("err = %v, want the original rejection passed through", err)
	}
	if calls != 0 {
		t.Fatalf("repair calls = %d, want 0 within the fresh-session window", calls)
	}

	// 窗口过期后又应该允许恢复。
	pool.mu.Lock()
	pool.verified = map[string]verifiedSession{}
	pool.mu.Unlock()
	if _, err := pool.recoverCodeSession(ctx, acc, "wx-synthetic-app", "", original, probe); err != nil {
		t.Fatalf("repair after the window expired: %v", err)
	}
	if calls != 1 {
		t.Fatalf("repair calls = %d, want 1 after the window expired", calls)
	}
}

// TestReplacedSessionDoesNotInheritVerifiedMark 守住 v0.2.61 的加固：
// 「刚验证过」必须绑在这条会话本身。缓存被替换（重建 / 重扫 / 换出口）之后，
// 新会话不能继承旧会话的成功记录——否则紧接着的会话级 -14 会被误判成
// 目标级拒绝，本该做的恢复被跳过。
func TestReplacedSessionDoesNotInheritVerifiedMark(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "yyb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	status := "alive"
	acc, err := db.UpsertAccount(ctx, "synthetic-replaced-account", "saved-buffer", nil, nil, nil, nil,
		map[string]any{"accesstoken": "synthetic-access"}, &status)
	if err != nil {
		t.Fatal(err)
	}
	oldSession := WmpfSession{
		Session: AppSession{UIN: 1, SendKey: []byte("send-old"), RecvKey: []byte("recv-old")},
		PSK:     pskEntry{PSKType: 1, PreSharedKey: "001122", TicketEntry: "aabbcc", ExpiredAt: time.Now().Add(time.Hour).Unix()},
	}
	if err := db.PutSession(ctx, acc.ID, nil, oldSession.toBlob(), oldSession.PSK.ExpiredAt, ""); err != nil {
		t.Fatal(err)
	}
	pool := NewPool(DefaultConfig(), db)
	pool.markSessionVerified(acc, acc.ID, oldSession)

	// 会话被重建：缓存换成另一套密钥。
	newSession := WmpfSession{
		Session: AppSession{UIN: 2, SendKey: []byte("send-new"), RecvKey: []byte("recv-new")},
		PSK:     pskEntry{PSKType: 1, PreSharedKey: "334455", TicketEntry: "ddeeff", ExpiredAt: time.Now().Add(time.Hour).Unix()},
	}
	if err := db.PutSession(ctx, acc.ID, nil, newSession.toBlob(), newSession.PSK.ExpiredAt, ""); err != nil {
		t.Fatal(err)
	}

	calls := 0
	probe := func(context.Context) (WmpfSession, map[string]any, error) {
		calls++
		return newSession, map[string]any{"code": "synthetic-code"}, nil
	}
	original := &ShortlinkRejectedError{Code: -14}
	if _, err := pool.recoverCodeSession(ctx, acc, "wx-synthetic-app", "", original, probe); err != nil {
		t.Fatalf("recovery must run for a replaced session: %v", err)
	}
	if calls != 1 {
		t.Fatalf("repair calls = %d, want 1: a replaced session must not inherit the mark", calls)
	}
}

// TestRecoverCodeWithTicketsOnlyRefetchesOnManualAuthRejection 守住 v0.2.55 的顺序：
// 先试保存票据；只有 ManualAuth 明确回 -10 才去取新票据。
// 把「目标级拒绝」「网络失败」也当成换票理由，会把一次应用拒绝放大成整轮重登。
func TestRecoverCodeWithTicketsOnlyRefetchesOnManualAuthRejection(t *testing.T) {
	session := WmpfSession{Session: AppSession{UIN: 42}}
	cases := []struct {
		name      string
		loginErr  error
		wantFetch bool
		wantCalls int
		wantErr   bool
	}{
		{"saved ticket works", nil, false, 1, false},
		{"manual auth rejected", &ManualAuthRejectedError{Code: -10}, true, 2, false},
		{"goal level rejection", &ManualAuthRejectedError{Code: -14}, false, 1, true},
		{"network failure", errors.New("connection reset"), false, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logins, fetches := 0, 0
			login := func(_ context.Context, buffer string) (WmpfSession, error) {
				logins++
				// 第一次用保存票据，第二次（若有）用新票据。
				if logins == 1 && tc.loginErr != nil {
					return WmpfSession{}, tc.loginErr
				}
				return session, nil
			}
			fetch := func(context.Context) (string, error) {
				fetches++
				return "fresh-buffer", nil
			}
			request := func(context.Context, WmpfSession) (map[string]any, error) {
				return map[string]any{"code": "synthetic-code"}, nil
			}
			st, result, err := recoverCodeWithTickets(context.Background(), "saved-buffer", login, fetch, request)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected the original login failure to be returned")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if result["code"] != "synthetic-code" || st.Session.UIN != 42 {
					t.Fatal("validated session was not returned")
				}
			}
			if (fetches > 0) != tc.wantFetch {
				t.Fatalf("fetch called = %v, want %v", fetches > 0, tc.wantFetch)
			}
			if logins != tc.wantCalls {
				t.Fatalf("login calls = %d, want %d", logins, tc.wantCalls)
			}
		})
	}
}

// TestFreshTicketAccessRejectionIsTyped 守住「明确拒绝」与「推断过期」的分界：
// 只有票据接口自己说 access 被拒，才包成 CodeCredentialRecoveryError 放行凭据刷新。
func TestFreshTicketAccessRejectionIsTyped(t *testing.T) {
	for _, code := range []int{-101, 40014, 42001} {
		t.Run(fmt.Sprintf("code_%d", code), func(t *testing.T) {
			login := func(context.Context, string) (WmpfSession, error) {
				return WmpfSession{}, &ManualAuthRejectedError{Code: -10}
			}
			fetch := func(context.Context) (string, error) {
				return "", &LoginBufferRejectedError{Code: code, Message: "synthetic"}
			}
			request := func(context.Context, WmpfSession) (map[string]any, error) {
				t.Fatal("request must not run when the ticket cannot be fetched")
				return nil, nil
			}
			_, _, err := recoverCodeWithTickets(context.Background(), "saved", login, fetch, request)
			var typed *CodeCredentialRecoveryError
			if !errors.As(err, &typed) {
				t.Fatalf("err = %v, want CodeCredentialRecoveryError", err)
			}
		})
	}
}

// TestSessionOnlyRecoveryNeverFallsBackToNewLogin 守住 session_only 的边界：
// 授权已被明确拒绝的账号只允许走独立原生恢复，不允许借「取新票据」绕过
// 「禁止新握手」的约束。v0.2.55 把这层分流写进了 RecoverCodeSession 的分支，
// 原来那个 nativeFallbackAllowed 回落函数已经被上游的重排方案取代。
func TestSessionOnlyRecoveryNeverFallsBackToNewLogin(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "yyb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	limited := store.StatusSessionOnly
	acc, err := db.UpsertAccount(ctx, "synthetic-limited-account", "saved-buffer", nil, nil, nil, nil,
		map[string]any{"accesstoken": "synthetic-access"}, &limited)
	if err != nil {
		t.Fatal(err)
	}
	pool := NewPool(DefaultConfig(), db)
	fetched := 0
	_, err = pool.RecoverCodeSession(ctx, acc, "wx-synthetic-app", "", true, &ShortlinkRejectedError{Code: -14},
		func(context.Context) (string, error) {
			fetched++
			return "fresh-buffer", nil
		})
	if err == nil {
		t.Fatal("session_only account must not recover without independent native material")
	}
	if fetched != 0 {
		t.Fatalf("fresh ticket fetched %d times, want 0 for a session_only account", fetched)
	}
}

func strPtr(v string) *string { return &v }
