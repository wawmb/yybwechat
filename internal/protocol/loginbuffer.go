package protocol

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"
)

// ErrMissingRefreshToken 表示账号凭据里没有 refresh token，无法续期，只能重新扫码。
var ErrMissingRefreshToken = errors.New("missing refresh token")

// RefreshRejectedError 表示刷新凭据接口返回了业务拒绝码（code != 0）。
// 与网络错误、代理错误、JSON 解析错误区分开，供失效判定精确识别微信侧的真实拒绝。
type RefreshRejectedError struct {
	Code    int
	Message string
}

func (e *RefreshRejectedError) Error() string {
	return fmt.Sprintf("refresh failed: code=%d msg=%s", e.Code, e.Message)
}

// LoginBufferRejectedError 表示 login_buffer 接口返回了业务拒绝码（code != 0）。
type LoginBufferRejectedError struct {
	Code    int
	Message string
}

func (e *LoginBufferRejectedError) Error() string {
	return fmt.Sprintf("login_buffer failed: code=%d msg=%s", e.Code, e.Message)
}

// 应用宝登录凭证相关的 URL 和认证密钥常量
const (
	yybHost         = "https://yybadaccess.3g.qq.com"
	loginBufferURL  = yybHost + "/pc_yyb_auth/pcyyb_get_wx_login_buffer_auth"
	refreshTokenURL = yybHost + "/pc_yyb_auth/pcyyb_refresh_token_auth"
	userInfoURL     = yybHost + "/pc_yyb/pcyyb_get_user_info"
	loginBusinessID = "pc_yyb_auth"
	loginAccessKey  = "wgrdg373hy26ww2"
)

type LoginBufferCredentials struct {
	OpenID                 string `json:"openid"`
	AccessToken            string `json:"accesstoken"`
	RefreshToken           string `json:"refreshtoken"`
	RefreshTokenObservedAt int64  `json:"refresh_token_observed_at"`
	LoginType              string `json:"logintype"`
	Nickname               string `json:"nickname"`
	ExpiresAt              int64  `json:"expires_at"`
	ExpiresIn              int64  `json:"expires_in"`
}

func CredentialsFromMap(m map[string]any) LoginBufferCredentials {
	expiresIn := defaultInt64(int64FromMap(m, "expires_in"), 7200)
	expiresAt := int64FromMap(m, "expires_at")
	// 兼容早期没有 expires_at 的记录：用上次刷新时间加有效期推算，
	// 否则这类账号的剩余有效期永远算不出来，界面上只能显示"未记录"。
	if expiresAt <= 0 {
		if refreshedAt := int64FromMap(m, "refresh_refreshed_at"); refreshedAt > 0 {
			expiresAt = refreshedAt + expiresIn
		}
	}
	return LoginBufferCredentials{
		OpenID:                 stringFromMap(m, "openid"),
		AccessToken:            stringFromMap(m, "accesstoken"),
		RefreshToken:           stringFromMap(m, "refreshtoken"),
		RefreshTokenObservedAt: int64FromMap(m, "refresh_token_observed_at"),
		LoginType:              defaultString(stringFromMap(m, "logintype"), "WX"),
		Nickname:               stringFromMap(m, "nickname"),
		ExpiresAt:              expiresAt,
		ExpiresIn:              expiresIn,
	}
}

func (c LoginBufferCredentials) ToMap() map[string]any {
	return map[string]any{
		"openid":                    c.OpenID,
		"accesstoken":               c.AccessToken,
		"refreshtoken":              c.RefreshToken,
		"refresh_token_observed_at": c.RefreshTokenObservedAt,
		"logintype":                 defaultString(c.LoginType, "WX"),
		"nickname":                  c.Nickname,
		"expires_at":                c.ExpiresAt,
		"expires_in":                defaultInt64(c.ExpiresIn, 7200),
		"refresh_refreshed_at":      time.Now().Unix(),
	}
}

// ToMapForScan 序列化一次成功扫码得到的凭据。
//
// 30 天窗口计的是「扫码授权」那一刻，不是保活偶然刷新到某个老账号的时刻。
// 只有这里才补写 refresh_token_observed_at，避免缺少该字段的历史记录被回填成
// 一个「刚刚扫码」的假时间戳。
func (c LoginBufferCredentials) ToMapForScan() map[string]any {
	if c.RefreshToken != "" && c.RefreshTokenObservedAt <= 0 {
		c.RefreshTokenObservedAt = time.Now().Unix()
	}
	return c.ToMap()
}

// Expired 判断凭据是否已在 skew 容差内过期。
func (c LoginBufferCredentials) Expired(skew time.Duration) bool {
	return time.Now().Add(skew).Unix() >= c.ExpiresAt
}

type LoginBufferResult struct {
	LoginBuffer string
	Credentials LoginBufferCredentials
	Refreshed   bool
}

type LoginBufferClient struct {
	httpClient *http.Client
	timeout    time.Duration
}

func NewLoginBufferClient(timeout time.Duration) *LoginBufferClient {
	return NewLoginBufferClientWithHTTPClient(&http.Client{Timeout: timeout}, timeout)
}

// NewLoginBufferClientWithHTTPClient 允许调用方注入自定义 HTTP 客户端，
// 例如绑定了账号代理的客户端。
func NewLoginBufferClientWithHTTPClient(httpClient *http.Client, timeout time.Duration) *LoginBufferClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	return &LoginBufferClient{
		httpClient: httpClient,
		timeout:    timeout,
	}
}

func (c *LoginBufferClient) FetchLoginBuffer(ctx context.Context, creds LoginBufferCredentials) (string, error) {
	body, err := json.Marshal(loginBufferRequest{
		ExtInfo: loginExtInfo{
			ListS: loginListS{
				UnionID:     loginValueString{Value: []string{creds.OpenID}},
				UserID:      loginValueString{Value: []string{creds.OpenID}},
				AccessToken: loginValueString{Value: []string{creds.AccessToken}},
			},
			ListI: loginListI{UserType: loginValueInt{Value: []int{0}}},
		},
	})
	if err != nil {
		return "", err
	}
	ts, nonce := timestampMS(), nonce4()
	var data map[string]any
	if err = c.requestJSON(ctx, http.MethodPost, loginBufferURL, body, map[string]string{
		"Content-Type":          "application/json",
		"Ual-Access-Businessid": loginBusinessID,
		"Ual-Access-Timestamp":  ts,
		"Ual-Access-Nonce":      nonce,
		"Ual-Access-Signature":  sign(string(body), nonce, ts, loginAccessKey),
		"Cookie":                fmt.Sprintf("openid=%s; accesstoken=%s; refreshtoken=%s", creds.OpenID, creds.AccessToken, creds.RefreshToken),
	}, &data); err != nil {
		return "", err
	}
	if code := intFromAny(data["code"]); code != 0 {
		return "", &LoginBufferRejectedError{Code: code, Message: fmt.Sprint(data["msg"])}
	}
	values := stringSlicePath(data, "ext_info", "list_s", "login_buffer", "value")
	if len(values) == 0 || values[0] == "" {
		return "", fmt.Errorf("login_buffer response is empty")
	}
	return values[0], nil
}

func (c *LoginBufferClient) RefreshCredentials(ctx context.Context, creds LoginBufferCredentials) (LoginBufferCredentials, error) {
	if creds.RefreshToken == "" {
		return LoginBufferCredentials{}, ErrMissingRefreshToken
	}
	body, err := json.Marshal(refreshTokenRequest{UserInfo: refreshTokenUserInfo{
		OpenID:       creds.OpenID,
		RefreshToken: creds.RefreshToken,
		AccessToken:  creds.AccessToken,
		LoginType:    defaultString(creds.LoginType, "WX"),
	}})
	if err != nil {
		return LoginBufferCredentials{}, err
	}
	ts, nonce := timestampMS(), nonce4()
	var data map[string]any
	if err = c.requestJSON(ctx, http.MethodPost, refreshTokenURL, body, map[string]string{
		"Content-Type":          "application/json",
		"Ual-Access-Businessid": loginBusinessID,
		"Ual-Access-Timestamp":  ts,
		"Ual-Access-Nonce":      nonce,
		"Ual-Access-Signature":  sign(string(body), nonce, ts, loginAccessKey),
	}, &data); err != nil {
		return LoginBufferCredentials{}, err
	}
	if code := intFromAny(data["code"]); code != 0 {
		return LoginBufferCredentials{}, &RefreshRejectedError{Code: code, Message: fmt.Sprint(data["msg"])}
	}
	info, _ := data["user_info"].(map[string]any)
	expiresIn := defaultInt64(int64FromMap(info, "expires_in"), 7200)
	refreshed := creds
	refreshed.AccessToken = stringFromMap(info, "access_token")
	// 微信侧轮换 refresh token 时才更新；30 天扫码窗口随之重新开始计时。
	if rt := stringFromMap(info, "refresh_token"); rt != "" && rt != refreshed.RefreshToken {
		refreshed.RefreshToken = rt
		refreshed.RefreshTokenObservedAt = time.Now().Unix()
	}
	refreshed.ExpiresIn = expiresIn
	refreshed.ExpiresAt = time.Now().Unix() + expiresIn
	if refreshed.AccessToken == "" {
		return LoginBufferCredentials{}, fmt.Errorf("refresh response missing access_token")
	}
	return refreshed, nil
}

func (c *LoginBufferClient) RefreshLoginBuffer(ctx context.Context, creds LoginBufferCredentials) (LoginBufferResult, error) {
	refreshed, err := c.RefreshCredentials(ctx, creds)
	if err != nil {
		return LoginBufferResult{}, err
	}
	lb, err := c.FetchLoginBuffer(ctx, refreshed)
	if err != nil {
		return LoginBufferResult{}, err
	}
	return LoginBufferResult{LoginBuffer: lb, Credentials: refreshed, Refreshed: true}, nil
}

func (c *LoginBufferClient) FetchUserInfo(ctx context.Context, creds LoginBufferCredentials) (map[string]any, error) {
	ts, nonce := timestampMS(), nonce4()
	var data map[string]any
	err := c.requestJSON(ctx, http.MethodGet, userInfoURL, nil, map[string]string{
		"Ual-Access-Access-Token": creds.AccessToken,
		"Ual-Access-Login-Type":   "2",
		"Ual-Access-Openid":       creds.OpenID,
		"Ual-Access-Businessid":   "pc_yyb",
		"Ual-Access-Guid":         "web",
		"Ual-Access-Nonce":        nonce,
		"Ual-Access-Requestid":    fmt.Sprintf("%d", rand.Intn(9000)+1000),
		"Ual-Access-Signature":    sign("", nonce, ts, ""),
		"Ual-Access-Timestamp":    ts,
	}, &data)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (c *LoginBufferClient) requestJSON(ctx context.Context, method, url string, body []byte, headers map[string]string, out any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data[:min(len(data), 200)]))
	}
	if err = json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode JSON: %w: %s", err, string(data[:min(len(data), 200)]))
	}
	return nil
}

type loginBufferRequest struct {
	ExtInfo loginExtInfo `json:"extInfo"`
}
type loginExtInfo struct {
	ListS loginListS `json:"listS"`
	ListI loginListI `json:"listI"`
}
type loginListS struct {
	UnionID     loginValueString `json:"unionid"`
	UserID      loginValueString `json:"user_id"`
	AccessToken loginValueString `json:"access_token"`
}
type loginListI struct {
	UserType loginValueInt `json:"user_type"`
}
type loginValueString struct {
	Value []string `json:"value"`
}
type loginValueInt struct {
	Value []int `json:"value"`
}
type refreshTokenRequest struct {
	UserInfo refreshTokenUserInfo `json:"userInfo"`
}
type refreshTokenUserInfo struct {
	OpenID       string `json:"openId"`
	RefreshToken string `json:"refreshToken"`
	AccessToken  string `json:"accessToken"`
	LoginType    string `json:"loginType"`
}

func timestampMS() string {
	return fmt.Sprintf("%d", time.Now().UnixMilli())
}

func nonce4() string {
	return fmt.Sprintf("%d", rand.Intn(10000))
}

func sign(body, nonce, ts, key string) string {
	sum := md5.Sum([]byte(body + ts + key + nonce))
	return hex.EncodeToString(sum[:])
}

func stringSlicePath(m map[string]any, path ...string) []string {
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[p]
	}
	arr, ok := cur.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func stringFromMap(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func int64FromMap(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	default:
		return 0
	}
}

func defaultString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func defaultInt64(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}
