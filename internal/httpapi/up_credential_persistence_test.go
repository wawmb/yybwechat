// 说明：上游还有一条 TestRefreshRetryFitsInsideExpiryWindow，断言退避公式为
// min(5min, max(30s, 剩余有效期/2))。本项目刻意用另一套策略——
// 凭据进入续期窗口（默认最后 4 分钟）后固定按 30 秒重试，而不是按剩余时间比例，
// 因为窗口本身只有几分钟，按比例算出来的等待会吃掉最后一次补救机会。
// 该策略由 keepalive_test.go 中的退避用例覆盖，这里不引入上游的公式。

package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"yybwechat/internal/protocol"
)

func TestRefreshSavesRotationBeforeBufferFailureOrCancellation(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffer failure", true: "cancelled request"}[cancelRequest], func(t *testing.T) {
			app := newKeepAliveTestApp(t)
			defer app.Close()
			acc := insertKeepAliveTestAccount(t, app, "rotation", time.Now().Add(time.Minute))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			app.refreshCredentials = func(_ context.Context, creds protocol.LoginBufferCredentials) (protocol.LoginBufferCredentials, error) {
				creds.AccessToken, creds.RefreshToken = "new-access", "new-refresh"
				creds.ExpiresAt = time.Now().Add(2 * time.Hour).Unix()
				if cancelRequest {
					cancel()
				}
				return creds, nil
			}
			app.fetchLoginBuffer = func(context.Context, protocol.LoginBufferCredentials) (string, error) {
				stored, err := app.db.GetAccount(context.Background(), acc.ID)
				if err != nil || stored.Credentials["refreshtoken"] != "new-refresh" {
					t.Fatal("buffer request started before token was saved")
				}
				return "", context.DeadlineExceeded
			}
			status, refreshed, err := app.refreshAccount(ctx, acc, true)
			if status != "alive" || !refreshed || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("refresh = %q, %v, %v", status, refreshed, err)
			}
			stored, err := app.db.GetAccount(context.Background(), acc.ID)
			if err != nil || stored.Credentials["refreshtoken"] != "new-refresh" || stored.LoginBuffer != acc.LoginBuffer {
				t.Fatal("successful rotation was lost on subsequent failure")
			}
		})
	}
}

func TestBackgroundRefreshDoesNotFetchLoginBuffer(t *testing.T) {
	app := newKeepAliveTestApp(t)
	defer app.Close()
	acc := insertKeepAliveTestAccount(t, app, "token-only", time.Now().Add(time.Minute))
	app.refreshCredentials = func(_ context.Context, creds protocol.LoginBufferCredentials) (protocol.LoginBufferCredentials, error) {
		creds.AccessToken = "new-access"
		creds.ExpiresAt = time.Now().Add(2 * time.Hour).Unix()
		return creds, nil
	}
	app.fetchLoginBuffer = func(context.Context, protocol.LoginBufferCredentials) (string, error) {
		t.Fatal("background token renewal must not fetch a login buffer")
		return "", nil
	}
	if status, renewed, err := app.refreshAccount(context.Background(), acc, false); status != "alive" || !renewed || err != nil {
		t.Fatalf("background refresh = %q, %v, %v", status, renewed, err)
	}
}

func TestInFlightRefreshCannotOverwriteRescanOrManualExpiration(t *testing.T) {
	for _, transition := range []string{"rescan-success", "rescan-rejection", "manual-expiration"} {
		t.Run(transition, func(t *testing.T) {
			app := newKeepAliveTestApp(t)
			defer app.Close()
			acc := insertKeepAliveTestAccount(t, app, "concurrent-account", time.Now().Add(time.Minute))
			ctx := context.Background()
			app.refreshCredentials = func(_ context.Context, creds protocol.LoginBufferCredentials) (protocol.LoginBufferCredentials, error) {
				if transition == "manual-expiration" {
					if err := app.setAccountStatus(ctx, acc.ID, "expired"); err != nil {
						t.Fatal(err)
					}
				} else {
					rescanned := creds
					rescanned.AccessToken, rescanned.RefreshToken = "scanned-access", "scanned-refresh"
					status := "alive"
					if _, err := app.db.UpsertAccount(ctx, acc.OpenID, "scanned-buffer", nil, nil, nil, nil, rescanned.ToMapForScan(), &status); err != nil {
						t.Fatal(err)
					}
				}
				if transition == "rescan-rejection" {
					return protocol.LoginBufferCredentials{}, &protocol.RefreshRejectedError{Code: 40030, Message: "invalid refresh_token"}
				}
				creds.AccessToken = "old-request-access"
				return creds, nil
			}
			if _, _, err := app.refreshAccount(ctx, acc, false); err == nil {
				t.Fatal("stale request should report a conflict")
			}
			stored, err := app.db.GetAccount(ctx, acc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if transition == "manual-expiration" {
				// 与上游的差异：本项目手动标记失效时**保留**凭据，
				// 目的是让「刷新存活」还有一次把账号救回来的机会（README 已写明这条路）。
				// 因此这里只断言最关键的一点——那次迟到的刷新没能把状态改回 alive。
				if accountStatus(stored) != "expired" {
					t.Fatal("old request revived a manually expired account")
				}
			} else if accountStatus(stored) != "alive" || stored.Credentials["accesstoken"] != "scanned-access" || stored.LoginBuffer != "scanned-buffer" {
				t.Fatal("old refresh overwrote or expired a rescanned account")
			}
		})
	}
}
