// Package oauth 构造微信公众号网页授权 URL。
//
// 本包只负责「构造和校验」授权链接，不做 code 换 token、不发起任何网络请求。
// 账号引用（ref）由 HTTP 层处理，授权完成后由用户在微信内点击产生回调 code。
package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// authorizeEndpoint 是微信公众号网页授权的固定端点。
const authorizeEndpoint = "https://open.weixin.qq.com/connect/oauth2/authorize"

// Request 是构造授权 URL 所需的参数集合。
type Request struct {
	AppID          string // 公众号 AppID，18 位且 wx 开头
	RedirectURI    string // 授权后回调地址，必须是绝对 http/https URL
	Scope          string // snsapi_base（静默）或 snsapi_userinfo（弹窗），默认 snsapi_base
	State          string // 可选，回传状态串，留空则随机生成
	ComponentAppID string // 可选，第三方平台开发时使用
}

// Result 是构造完成的授权 URL 结果。
// URL 是基础链接，FullURL 追加了微信要求的 #wechat_redirect 片段。
type Result struct {
	URL     string
	FullURL string
	Scope   string
	State   string
}

// Build 校验参数并构造公众号网页授权 URL。
func Build(req Request) (Result, error) {
	appID := strings.TrimSpace(req.AppID)
	if len(appID) != 18 || !strings.HasPrefix(appID, "wx") {
		return Result{}, fmt.Errorf("appid must be an 18-character public-account AppID")
	}

	redirectURI, err := validateRedirectURI(req.RedirectURI)
	if err != nil {
		return Result{}, err
	}

	scope := strings.TrimSpace(req.Scope)
	if scope == "" {
		scope = "snsapi_base"
	}
	if scope != "snsapi_base" && scope != "snsapi_userinfo" {
		return Result{}, fmt.Errorf("scope must be snsapi_base or snsapi_userinfo")
	}

	state := strings.TrimSpace(req.State)
	if state == "" {
		state, err = randomState()
		if err != nil {
			return Result{}, fmt.Errorf("generate state: %w", err)
		}
	}
	if len(state) > 128 || strings.ContainsAny(state, "\r\n") {
		return Result{}, fmt.Errorf("state must be at most 128 characters and single-line")
	}

	componentAppID := strings.TrimSpace(req.ComponentAppID)
	if componentAppID != "" && (len(componentAppID) != 18 || !strings.HasPrefix(componentAppID, "wx")) {
		return Result{}, fmt.Errorf("component_appid must be an 18-character AppID")
	}

	values := url.Values{}
	values.Set("appid", appID)
	values.Set("redirect_uri", redirectURI)
	values.Set("response_type", "code")
	values.Set("scope", scope)
	values.Set("state", state)
	if componentAppID != "" {
		values.Set("component_appid", componentAppID)
	}

	base := authorizeEndpoint + "?" + values.Encode()
	return Result{URL: base, FullURL: base + "#wechat_redirect", Scope: scope, State: state}, nil
}

// validateRedirectURI 校验回调地址：必须是绝对 http/https URL，
// 不含 userinfo（防钓鱼）、不含 fragment，长度不超过 2048。
func validateRedirectURI(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n") {
		return "", fmt.Errorf("redirect_uri is required and must be at most 2048 characters")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("redirect_uri must be an absolute URL without credentials or fragments")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("redirect_uri must use http or https")
	}
	return u.String(), nil
}

// randomState 生成 16 字节随机 state，用于防止 CSRF。
func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
