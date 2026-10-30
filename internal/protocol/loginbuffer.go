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
	"strconv"
	"strings"
	"time"

	"yybwechat/internal/pcmgr"
)

// paramsInvalidRejectionCode 是应用宝接口表示「参数无效」的业务码。
// 它是含糊错误：既可能是凭据被作废，也可能是我们自己构造的请求有问题，
// 所以不能靠它单独判死，要用 check-login 求证。
const paramsInvalidRejectionCode = -109

// ErrMissingRefreshToken 表示账号凭据里没有 refresh token，无法续期，只能重新扫码。
var ErrMissingRefreshToken = errors.New("missing refresh token")

// RefreshRejectedError 表示刷新凭据接口返回了业务拒绝码（code != 0）。
// 与网络错误、代理错误、JSON 解析错误区分开，供失效判定精确识别微信侧的真实拒绝。
type RefreshRejectedError struct {
	Code    int
	Message string
	// Operation 标明拒绝来自哪一步："refresh_token" 还是 "check_login"。
	// 两者语义不同——check_login 被拒才是「我们要不要判死」的关键证据。
	Operation string
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
	// 扫码主登录拿到的凭据属于 pc_yyb 业务，续期必须走同业务侧接口；
	// 用 _auth 侧续期会被微信以参数无效/令牌失效拒绝（这正是"扫码账户跑一段时间
	// 后突然被判失效"的根因之一，参考 v0.2.38 #103/#105/#107）。
	mainRefreshURL = yybHost + "/pc_yyb/pcyyb_refresh_token"
	// checkLoginURL 用来在续期返回含糊的 -109 时向官方客户端接口求证一次。
	checkLoginURL   = yybHost + "/pc_yyb/pcyyb_check_login"
	userInfoURL     = yybHost + "/pc_yyb/pcyyb_get_user_info"
	loginBusinessID = "pc_yyb_auth"
	loginAccessKey  = "wgrdg373hy26ww2"
)

type LoginBufferCredentials struct {
	// Provider 标记凭据来自哪个产品：空或 appstore = 应用宝扫码，pcmgr = 腾讯电脑管家。
	// 两条链路的续期、取票据实现完全不同，不能混用。
	Provider               string            `json:"provider,omitempty"`
	PCMgr                  pcmgr.Credentials `json:"pcmgr,omitempty"`
	OpenID                 string            `json:"openid"`
	AccessToken            string            `json:"accesstoken"`
	RefreshToken           string            `json:"refreshtoken"`
	RefreshTokenObservedAt int64             `json:"refresh_token_observed_at"`
	// RefreshBusinessID 标记这份凭据属于哪个业务侧，决定续期走哪个端点。
	// 空值按扫码主登录（pc_yyb）处理，兼容升级前没有该字段的旧记录。
	RefreshBusinessID string `json:"refresh_business_id"`
	LoginType         string `json:"logintype"`
	Nickname          string `json:"nickname"`
	ExpiresAt         int64  `json:"expires_at"`
	ExpiresIn         int64  `json:"expires_in"`
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
	var pc pcmgr.Credentials
	if value, ok := m["pcmgr"]; ok {
		raw, _ := json.Marshal(value)
		_ = json.Unmarshal(raw, &pc)
	}
	return LoginBufferCredentials{
		Provider:               stringFromMap(m, "provider"),
		PCMgr:                  pc,
		OpenID:                 stringFromMap(m, "openid"),
		AccessToken:            stringFromMap(m, "accesstoken"),
		RefreshToken:           stringFromMap(m, "refreshtoken"),
		RefreshTokenObservedAt: int64FromMap(m, "refresh_token_observed_at"),
		RefreshBusinessID:      stringFromMap(m, "refresh_business_id"),
		LoginType:              defaultString(stringFromMap(m, "logintype"), "WX"),
		Nickname:               stringFromMap(m, "nickname"),
		ExpiresAt:              expiresAt,
		ExpiresIn:              expiresIn,
	}
}

func (c LoginBufferCredentials) ToMap() map[string]any {
	result := map[string]any{
		"openid":                    c.OpenID,
		"accesstoken":               c.AccessToken,
		"refreshtoken":              c.RefreshToken,
		"refresh_token_observed_at": c.RefreshTokenObservedAt,
		"refresh_business_id":       c.RefreshBusinessID,
		"logintype":                 defaultString(c.LoginType, "WX"),
		"nickname":                  c.Nickname,
		"expires_at":                c.ExpiresAt,
		"expires_in":                defaultInt64(c.ExpiresIn, 7200),
		"refresh_refreshed_at":      time.Now().Unix(),
	}
	if c.Provider != "" {
		result["provider"] = c.Provider
	}
	if c.Provider == "pcmgr" {
		result["pcmgr"] = c.PCMgr
	}
	return result
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
	// 电脑管家的 ExpiresAt 是「下次续期计划时间」，不是凭据有效期。
	// 拿它做时间比较会把「该续期了」误判成「凭据已失效」，
	// 尤其在计划时间还没写入（ExpiresAt=0）时，now >= 0 恒成立，
	// 一个刚扫码的账号会立刻被判死。这里只按凭据自身是否完整来判断。
	if c.Provider == "pcmgr" {
		return c.PCMgr.Validate() != nil
	}
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
	if creds.Provider == "pcmgr" {
		return pcmgr.FetchBuffer(ctx, c.httpClient, creds.PCMgr)
	}
	if creds.Provider != "" && creds.Provider != "appstore" {
		return "", fmt.Errorf("unsupported credential provider")
	}
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
	var data loginCredentialResponse
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
	code, err := credentialResponseCode(data.Code)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", &LoginBufferRejectedError{Code: code, Message: redactCredentials(fmt.Sprint(data.Message), creds)}
	}
	values := stringSlicePath(data.ExtInfo, "list_s", "login_buffer", "value")
	if len(values) == 0 || values[0] == "" {
		return "", fmt.Errorf("login_buffer response is empty")
	}
	return values[0], nil
}

func (c *LoginBufferClient) RefreshCredentials(ctx context.Context, creds LoginBufferCredentials) (LoginBufferCredentials, error) {
	if creds.Provider == "pcmgr" {
		// 管家的续期是 key 轮换：换完把新 key 存回去，有效期跟着它走。
		pc, err := pcmgr.Renew(ctx, c.httpClient, creds.PCMgr)
		if err != nil {
			return LoginBufferCredentials{}, err
		}
		creds.PCMgr = pc
		creds.ExpiresAt = pc.NextRenewAt()
		creds.ExpiresIn = creds.ExpiresAt - pc.RenewedAt
		return creds, nil
	}
	if creds.Provider != "" && creds.Provider != "appstore" {
		return LoginBufferCredentials{}, fmt.Errorf("unsupported credential provider")
	}
	// 扫码主登录拿到的凭据来自 /pc_yyb/pcyyb_oauth，续期必须留在 pc_yyb 业务侧；
	// 早期记录没有来源标记，按扫码主登录处理（这正是升级前的默认行为）。
	// 独立 wxapp 授权的凭据才走 _auth 侧。
	business, endpoint, key := "pc_yyb", mainRefreshURL, ""
	switch creds.RefreshBusinessID {
	case "", "pc_yyb":
	case loginBusinessID:
		business, endpoint, key = loginBusinessID, refreshTokenURL, loginAccessKey
	default:
		return LoginBufferCredentials{}, fmt.Errorf("unsupported credential refresh business")
	}

	refreshed, err := c.requestCredentials(ctx, creds, endpoint, business, key)
	var rejected *RefreshRejectedError
	if business == "pc_yyb" && errors.As(err, &rejected) && rejected.Code == paramsInvalidRejectionCode && ctx.Err() == nil {
		// -109 的字面语义是「参数不合法」，不足以断定凭据被作废。
		// 先用官方客户端的 check-login 求证一次：它能恢复/轮换凭据，
		// 也能把微信真正的拒绝码暴露出来，避免我们把含糊错误当成终态。
		checked, checkErr := c.CheckLogin(ctx, creds)
		if checkErr == nil {
			return checked, nil
		}
		// 两段都用 %w：refresh 与 check-login 是各自独立的拒绝原因。
		// 以前第一段用 %v 拍平成字符串，判定时只剩最后一段的类型——
		// refresh 明确 42007、check-login 含糊 -109 时就会漏判成"可重试"。
		return LoginBufferCredentials{}, fmt.Errorf("%w; check login: %w", err, checkErr)
	}
	return refreshed, err
}

// CheckLogin 调用官方客户端的 check-login 接口，用于在续期返回含糊错误时求证凭据状态。
func (c *LoginBufferClient) CheckLogin(ctx context.Context, creds LoginBufferCredentials) (LoginBufferCredentials, error) {
	return c.requestCredentials(ctx, creds, checkLoginURL, "pc_yyb", "")
}

func (c *LoginBufferClient) requestCredentials(ctx context.Context, creds LoginBufferCredentials, endpoint, business, key string) (LoginBufferCredentials, error) {
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
	var data loginCredentialResponse
	if err = c.requestJSON(ctx, http.MethodPost, endpoint, body, map[string]string{
		"Content-Type":          "application/json",
		"Ual-Access-Businessid": business,
		"Ual-Access-Timestamp":  ts,
		"Ual-Access-Nonce":      nonce,
		"Ual-Access-Signature":  sign(string(body), nonce, ts, key),
	}, &data); err != nil {
		return LoginBufferCredentials{}, err
	}
	code, err := credentialResponseCode(data.Code)
	if err != nil {
		return LoginBufferCredentials{}, err
	}
	if code != 0 {
		operation := "refresh_token"
		if endpoint == checkLoginURL {
			operation = "check_login"
		}
		return LoginBufferCredentials{}, &RefreshRejectedError{
			Code:      code,
			Message:   redactCredentials(fmt.Sprint(data.Message), creds),
			Operation: operation,
		}
	}
	info := data.UserInfo
	// 上游偶尔会返回另一个账号的凭据。写进库里就等于把账号 A 的 token
	// 记到账号 B 名下，后续所有调用都会莫名失败，这里必须硬拒。
	if openID := stringFromMap(info, "open_id"); openID != "" && openID != creds.OpenID {
		return LoginBufferCredentials{}, fmt.Errorf("credential response account mismatch")
	}
	if loginType := stringFromMap(info, "login_type"); loginType != "" && loginType != defaultString(creds.LoginType, "WX") {
		return LoginBufferCredentials{}, fmt.Errorf("credential response login type mismatch")
	}
	expiresIn := defaultInt64(int64FromMap(info, "expires_in"), 7200)
	refreshed := creds
	refreshed.AccessToken = stringFromMap(info, "access_token")
	// 微信侧轮换 refresh token 时更新令牌值，但**不重置 RefreshTokenObservedAt**。
	// 那一列记的是「首次看到该账号 refresh token 的时间」，也就是扫码窗口的起点；
	// 把它当成"最近一次轮换"来刷新，会让界面上的「扫码 30 天」倒计时无限续命，
	// 掩盖账号其实早就该重扫的事实。
	if rt := stringFromMap(info, "refresh_token"); rt != "" && rt != refreshed.RefreshToken {
		refreshed.RefreshToken = rt
	}
	refreshed.RefreshBusinessID = business
	refreshed.LoginType = defaultString(creds.LoginType, "WX")
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
		// 续期已经成功、token 可能已被轮换。取票据失败（网络/上游抖动）不能把
		// 新凭据一起丢掉——否则库里留着的是已作废的旧 token，下一轮必然被拒。
		return LoginBufferResult{Credentials: refreshed, Refreshed: true}, err
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
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err = json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
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

// loginCredentialResponse 用 json.RawMessage 承接 code。
//
// 以前把响应解到 map[string]any 再用 intFromAny 取 code：缺失、null、布尔、
// 小数都会被当成 0，而 0 就是"成功"。于是一份格式异常的响应会被当成成功，
// 把里面那些来路不明的 token 写进库。
type loginCredentialResponse struct {
	Code     json.RawMessage `json:"code"`
	Message  any             `json:"msg"`
	UserInfo map[string]any  `json:"user_info"`
	ExtInfo  map[string]any  `json:"ext_info"`
}

// credentialResponseCode 只接受整数码（含整数字符串 "0"）。
// 其它形态一律返回格式错误，调用方据此报错而不是当作成功。
func credentialResponseCode(raw json.RawMessage) (int, error) {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, fmt.Errorf("credential response code must be an integer")
	}
	code, err := strconv.ParseInt(number.String(), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("credential response code must be an integer")
	}
	return int(code), nil
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

// redactionMinSecretLen 是参与脱敏的最短密钥长度。
const redactionMinSecretLen = 8

// redactCredentials 把错误信息里可能回显的凭据替换掉。
//
// 上游的 msg 偶尔会把 access token / refresh token / openid 原样带回来，
// 这些字符串会进日志、进告警、进接口响应，等于把凭据撒的到处都是。
func redactCredentials(message string, creds LoginBufferCredentials) string {
	for _, secret := range []string{creds.AccessToken, creds.RefreshToken, creds.OpenID} {
		// 过短的"密钥"不参与替换：真实 token 都有几十个字符，而像 "refresh"
		// 这种短值会把 "refresh_token" 这类正常措辞一起打碎，反而看不懂。
		if len([]rune(secret)) < redactionMinSecretLen {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	return message
}
