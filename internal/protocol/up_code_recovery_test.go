package protocol

import (
	"context"
	"encoding/json"
	"errors"
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
			result, err := pool.recoverCodeSession(ctx, acc, "", original, probe)
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
				_, _ = pool.recoverCodeSession(ctx, acc, "", original, probe)
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
	_, err := pool.recoverCodeSession(context.Background(), acc, "", original, func(context.Context) (WmpfSession, map[string]any, error) {
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
	if _, err = pool.recoverCodeSession(ctx, acc, "", original, probe); err != nil {
		t.Fatal("successful repair rejected", err)
	}
	// 修复成功后再来一次 -14：必须立刻允许重建，而不是返回退避中的原始错误。
	result, err := pool.recoverCodeSession(ctx, acc, "", original, probe)
	if err != nil || result["code"] != "synthetic-code" {
		t.Fatal("successful repair left the cooldown armed", err)
	}
	if calls != 2 {
		t.Fatalf("repair calls = %d, want 2 (cooldown not cleared)", calls)
	}
}

// TestNativeRecoveryFallbackOnlyForAuthorizedAccounts 守住回落边界：
// 有 OAuth 授权（非 session_only）的账号在原生恢复失败后可以继续取新票据；
// session_only 没有兜底，不允许借回落绕过「禁止新握手」的约束。
func TestNativeRecoveryFallbackOnlyForAuthorizedAccounts(t *testing.T) {
	limited := store.StatusSessionOnly
	cases := []struct {
		name string
		acc  *store.WechatAccount
		want bool
	}{
		{"alive", &store.WechatAccount{ID: 1}, true},
		{"unknown", &store.WechatAccount{ID: 2, Status: strPtr("unknown")}, true},
		{"session_only", &store.WechatAccount{ID: 3, Status: &limited}, false},
		{"expired", &store.WechatAccount{ID: 4, Status: strPtr("expired")}, true},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nativeFallbackAllowed(tc.acc); got != tc.want {
				t.Fatalf("nativeFallbackAllowed = %v, want %v", got, tc.want)
			}
		})
	}
}

func strPtr(v string) *string { return &v }
