package httpapi

import (
	"net/http"

	"yybwechat/internal/oauth"
	"yybwechat/internal/protocol"
)

// 本文件实现 /wxapp/* 下的两个扩展接口：
//   - /wxapp/getUserInfo：直接通过 HTTP 拉取微信用户信息，不走 MMTLS 通道；
//   - /wxapp/oauth：构造公众号网页授权 URL。
//
// 两者都是独立能力，统一挂在 /wxapp/ 命名空间下，不另开平行路径。

// handleGetUserInfo 处理 /wxapp/getUserInfo，拉取指定账号的微信用户信息。
//
// 与 /wxapp/operateWxData 的区别：本接口走普通 HTTPS 通道，
// 不需要建立 MMTLS 会话，因此即使协议会话暂时不可用也能返回基本资料。
// 支持 GET ?ref= 与 POST JSON 两种调用方式。
// 凭据失效时会先尝试刷新一次再重试，避免刚过期的账号直接报错。
func (a *App) handleGetUserInfo(w http.ResponseWriter, r *http.Request) {
	a.serveGetUserInfo(w, r, "/wxapp/getUserInfo")
}

// handleWXGetUserInfo 是 /wxapp/getUserInfo 的兼容入口（小程序端叫 getUserInfo）。
func (a *App) handleWXGetUserInfo(w http.ResponseWriter, r *http.Request) {
	a.serveGetUserInfo(w, r, "/wx/getuserinfo")
}

// serveGetUserInfo 同时服务 /wxapp/getUserInfo 与 /wx/getuserinfo。
//
// 路径校验必须带上具体入口：这两个地址共用一份实现，
// 如果沿用写死的路径判断，另一个入口会恒定返回 404。
func (a *App) serveGetUserInfo(w http.ResponseWriter, r *http.Request, path string) {
	if r.URL.Path != path {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	ref := ""
	switch r.Method {
	case http.MethodGet:
		ref = r.URL.Query().Get("ref")
	case http.MethodPost:
		var body accountRefIn
		if err := decodeOptionalJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		ref = body.Ref
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	acc, ok := a.resolveAccountRef(w, r, ref)
	if !ok {
		return
	}
	if acc.Credentials == nil {
		writeError(w, http.StatusConflict, "account has no credentials; re-scan required")
		return
	}
	info, err := a.qr.LoginBuffers().FetchUserInfo(r.Context(), protocol.CredentialsFromMap(acc.Credentials))
	if err != nil {
		// 首次失败先尝试刷新凭据，再重试一次
		status, _, _ := a.refreshLiveness(r.Context(), acc)
		if status != "alive" {
			writeError(w, http.StatusConflict, "account credentials expired; re-scan required")
			return
		}
		fresh, gerr := a.db.GetAccount(r.Context(), acc.ID)
		if gerr != nil || fresh.Credentials == nil {
			writeError(w, http.StatusConflict, "account credentials unavailable; re-scan required")
			return
		}
		info, err = a.qr.LoginBuffers().FetchUserInfo(r.Context(), protocol.CredentialsFromMap(fresh.Credentials))
		if err != nil {
			writeError(w, http.StatusBadGateway, "fetch user info failed: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"openid": acc.OpenID, "result": info})
}

// oauthRequest 是 /wxapp/oauth 的请求体。
type oauthRequest struct {
	Ref            string `json:"ref"`             // 可选，账号引用，仅做存在性校验
	AppID          string `json:"app_id"`          // 公众号 AppID，18 位且 wx 开头
	LegacyAppID    string `json:"appid"`           // 旧字段名，与 app_id 等价
	RedirectURI    string `json:"redirect_uri"`    // 授权回调地址，必须为绝对 http/https URL
	Scope          string `json:"scope"`           // snsapi_base 或 snsapi_userinfo，默认前者
	State          string `json:"state"`           // 可选，留空自动生成随机值防 CSRF
	ComponentAppID string `json:"component_appid"` // 可选，第三方平台开发时使用
}

// handleOAuth 构造公众号网页授权 URL。
//
// 重要边界：本接口只生成授权链接，不会产生 code。
// code 必须由用户在微信客户端内完成授权后，由回调地址携带返回，
// 服务端无法也不应伪造任何授权结果。响应里的 code=null 是正常状态，
// 因此额外给出 authorization_required / callback_code_source / next_step
// 三个提示字段，让自动化脚本能直接判断「这一步应该等回调，而不是重试」。
func (a *App) handleOAuth(w http.ResponseWriter, r *http.Request) {
	// /wx/oauth 是早期脚本使用的路径，与 /wxapp/oauth 行为完全一致
	if r.URL.Path != "/wxapp/oauth" && r.URL.Path != "/wx/oauth" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body oauthRequest
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.AppID == "" {
		body.AppID = body.LegacyAppID
	}
	// ref 为可选项，传入时校验账号存在，便于调用方确认授权关联的账号，
	// 并把 account_id / openid 一并回填到响应里。
	var accountID int64
	openID := ""
	if body.Ref != "" {
		acc, ok := a.resolveAccountRef(w, r, body.Ref)
		if !ok {
			return
		}
		accountID, openID = acc.ID, acc.OpenID
	}
	res, err := oauth.Build(oauth.Request{
		AppID:          body.AppID,
		RedirectURI:    body.RedirectURI,
		Scope:          body.Scope,
		State:          body.State,
		ComponentAppID: body.ComponentAppID,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url":                    res.URL,
		"full_url":               res.FullURL,
		"scope":                  res.Scope,
		"state":                  res.State,
		"appid":                  body.AppID,
		"account_id":             accountID,
		"openid":                 openID,
		"code":                   nil,
		"authorization_required": true,
		"callback_code_source":   "redirect_uri",
		"mode":                   "authorization_url",
		"next_step":              "请在微信内打开 url 并完成授权；授权后微信会请求 redirect_uri?code=...&state=...，当前响应的 code=null 是正常状态。",
		"callback_params":        []string{"code", "state"},
		"authorize":              "需要用户在微信内打开该链接完成授权，成功后 code 将回传至 redirect_uri",
	})
}
