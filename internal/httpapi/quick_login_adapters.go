package httpapi

import (
	"context"
	"net/http"

	"yybwechat/internal/protocol"
	"yybwechat/internal/store"
)

// 本项目是单管理员模型，没有账号归属（owner）概念，也没有青龙联动。
// quick-login 原本依赖上游的多用户与青龙同步，这里用同签名的空实现接住：
// 行为等价于「所有账号都属于当前管理员、扫码后不需要同步任何外部面板」。

func (a *App) ensureScannedAccountAllowed(*http.Request, string) error { return nil }

func (a *App) claimScannedAccount(*http.Request, int64) error { return nil }

func (a *App) autoSyncAfterScan(*store.WechatAccount) {}

// exchangeAuthCode 用微信授权 code 直接换 login_buffer（直连通道）。
func (a *App) exchangeAuthCode(ctx context.Context, code string) (protocol.LoginBufferResult, error) {
	return a.qr.GetLoginBufferFromCode(ctx, code)
}

// fetchUserInfo 走直连获取账号资料，供 quick-login 填昵称/头像。
func (a *App) fetchUserInfo(ctx context.Context, creds protocol.LoginBufferCredentials) (map[string]any, error) {
	return a.fetchUserInfoWithProxy(ctx, creds, "", false)
}
