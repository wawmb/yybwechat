package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"yybwechat/internal/protocol"
	"yybwechat/internal/store"
)

// 以下两个助手从上游测试套件移植，供 up_*_test.go 使用。

// newKeepAliveTestApp 建一个只用于保活/凭据持久化用例的最小 App。
func newKeepAliveTestApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("GIN_MODE", "test")
	app, err := NewApp(Config{
		ResourceRoot:   t.TempDir(),
		RequestTimeout: time.Second,
		AvatarTimeout:  time.Second,
		SessionTTL:     time.Minute,
		QRSessionTTL:   time.Minute,
		KeepAliveAhead: 45 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	// legacy_auth 刷新入口遇到 -109 会补一次官方 check-login。测试里必须
	// 把它换成不发网络请求的实现：真实客户端会去打微信接口，拿到 42007
	// 之后账号就被判定为终态，用例会因为「跑了真网络」而随环境飘。
	app.checkLogin = func(context.Context, protocol.LoginBufferCredentials) (protocol.LoginBufferCredentials, error) {
		return protocol.LoginBufferCredentials{}, errors.New("check login disabled in tests")
	}
	// 不注册 t.Cleanup：上游这些用例自己 defer app.Close()，
	// 重复关闭会重复 close(stopHealth) 而 panic。
	return app
}

// insertKeepAliveTestAccount 插入一个带「即将到期 / 已到期」凭据的账号。
func insertKeepAliveTestAccount(t *testing.T, app *App, openID string, expiresAt time.Time) *store.WechatAccount {
	t.Helper()
	status := "alive"
	creds := protocol.LoginBufferCredentials{
		OpenID:       openID,
		AccessToken:  "access-old",
		RefreshToken: "refresh-old",
		ExpiresAt:    expiresAt.Unix(),
		ExpiresIn:    7200,
	}
	acc, err := app.db.UpsertAccount(context.Background(), openID, "buffer-old", nil, nil, nil, nil, creds.ToMap(), &status)
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}
	return acc
}
