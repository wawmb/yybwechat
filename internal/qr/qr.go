// Package qr 提供微信 OAuth 扫码登录的完整流程实现。
// 包括：创建扫码会话、获取二维码图片、轮询扫码状态、
// 获取 login_buffer 以及刷新登录凭证等功能。
package qr

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"yybwechat/internal/pcmgr"
	"yybwechat/internal/protocol"
)

// 微信 OAuth 相关常量。
// AppID、回调地址、scope 与 state 抽成常量，便于公众号授权等场景复用同一套参数。
const (
	AppID            = "wxd44977328b36e647"
	OAuthRedirectURI = "https://yybadaccess.3g.qq.com/pc_yyb/pcyyb_oauth?login_type=WX"
	OAuthScope       = "snsapi_login,snsapi_runtime_pcsdk"
	OAuthState       = "web"

	// oauthURL 由上述常量拼接而成，拼接结果与旧版写法逐字节一致，避免改变登录行为。
	oauthURL = "https://open.weixin.qq.com/connect/qrconnect" +
		"?appid=" + AppID +
		"&redirect_uri=" + OAuthRedirectURI +
		"&response_type=code&scope=" + OAuthScope +
		"&state=" + OAuthState + "&fast_login=1&self_redirect=true"
	callbackURL  = "https://yybadaccess.3g.qq.com/pc_yyb/pcyyb_oauth"
	qrBase       = "https://open.weixin.qq.com/connect/qrcode/"
	longPollBase = "https://long.open.weixin.qq.com/connect/l/qrconnect"
	qrPollWait   = 35 * time.Second
	// qrPollLimit 是长轮询客户端的超时上限。微信在未扫码时最长挂起到约 35 秒，
	// 客户端超时必须大于这个值，否则每次轮询都会被本地提前掐断。
	qrPollLimit = 40 * time.Second
)

// 正则表达式：分别用于提取二维码 UUID、微信错误码和授权码
var (
	uuidRE = regexp.MustCompile(`/connect/qrcode/([^"'>\s]+)`)
	errRE  = regexp.MustCompile(`wx_errcode\s*=\s*(\d+)`)
	codeRE = regexp.MustCompile(`wx_code\s*=\s*'([^']*)'`)
)

// Session 表示一个微信扫码登录会话，持有与微信服务器的 HTTP 连接和 Cookie。
type Session struct {
	// Product 是本次扫码使用的渠道：空/appstore = 应用宝，pcmgr = 腾讯电脑管家。
	Product       string
	ClientGUID    string
	ID            string
	WXUUID        string
	QRCodeURL     string
	CreatedAt     time.Time
	HTTPClient    *http.Client
	Jar           http.CookieJar
	AuthorizeCode string
	Credentials   *protocol.LoginBufferCredentials
	LoginBuffer   string
	Status        string
	Error         string
	mu            sync.Mutex
}

// Age 返回会话自创建以来经过的时间。
func (s *Session) Age() time.Duration { return time.Since(s.CreatedAt) }

// ImageResult 包含二维码图片数据和对应的会话。
type ImageResult struct {
	Session    *Session
	ImageBytes []byte
}

// PollResult 表示轮询扫码状态的结果。
type PollResult struct {
	Status  string `json:"status"`
	ErrCode *int   `json:"errcode,omitempty"`
	Code    string `json:"-"`
	Message string `json:"-"`
}

// Client 是微信扫码登录客户端，封装了创建会话、获取二维码和轮询等操作。
type Client struct {
	timeout      time.Duration
	transport    http.RoundTripper
	loginBuffers *protocol.LoginBufferClient
}

// NewClient 创建一个走直连的二维码客户端。
func NewClient(timeout time.Duration) *Client {
	client, _ := NewClientWithProxy(timeout, "", false)
	return client
}

// NewClientWithProxy 创建一个绑定指定代理的二维码客户端。
//
// proxyValue 为空表示直连；fallbackDirect 为真时，代理握手失败会自动降级直连，
// 避免单个代理节点故障让扫码流程整体不可用。
func NewClientWithProxy(timeout time.Duration, proxyValue string, fallbackDirect bool) (*Client, error) {
	transport, err := protocol.NewHTTPTransport(proxyValue, fallbackDirect)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Timeout: timeout, Transport: transport}
	return &Client{
		timeout:      timeout,
		transport:    transport,
		loginBuffers: protocol.NewLoginBufferClientWithHTTPClient(httpClient, timeout),
	}, nil
}

// LoginBuffers 返回 LoginBufferClient 实例，用于获取和刷新 login_buffer。
func (c *Client) LoginBuffers() *protocol.LoginBufferClient { return c.loginBuffers }

// GetQRCodeImage 创建扫码会话并获取二维码图片。
func (c *Client) GetQRCodeImage(ctx context.Context) (ImageResult, error) {
	return c.GetQRCodeImageForProduct(ctx, "appstore")
}

// GetQRCodeImageForProduct 按渠道创建扫码会话。
func (c *Client) GetQRCodeImageForProduct(ctx context.Context, product string) (ImageResult, error) {
	sess, err := c.CreateSessionForProduct(ctx, product)
	if err != nil {
		return ImageResult{}, err
	}
	data, err := c.FetchQRCodeImage(ctx, sess)
	if err != nil {
		return ImageResult{}, err
	}
	// 校验返回内容确实是图片。代理或网络拦截时微信会返回 HTML/JSON，
	// 若不校验会把脏数据当成二维码展示，用户看到的是一张坏图却无从判断原因。
	if err := validateQRCodeImage(data); err != nil {
		return ImageResult{}, err
	}
	return ImageResult{Session: sess, ImageBytes: data}, nil
}

// CreateSession 向微信 OAuth 端点发起请求，解析并返回新的扫码会话。
// OAuthURL 按渠道生成扫码授权地址。
//
// 两个渠道的 appid / redirect_uri / scope 完全不同，混用会拿到一张
// 扫了也换不出凭据的二维码。
func OAuthURL(product string) (string, error) {
	switch product {
	case "", "appstore":
		return oauthURL, nil
	case "pcmgr":
		return "https://open.weixin.qq.com/connect/qrconnect?" + url.Values{
			"appid": {pcmgr.HostAppID}, "redirect_uri": {"https://security.guanjia.qq.com/login"},
			"response_type": {"code"}, "scope": {"snsapi_login,snsapi_runtime_pcsdk"},
			"state": {"233"}, "self_redirect": {"true"},
		}.Encode(), nil
	default:
		return "", fmt.Errorf("unsupported scan channel")
	}
}

func (c *Client) CreateSession(ctx context.Context) (*Session, error) {
	return c.CreateSessionForProduct(ctx, "appstore")
}

// CreateSessionForProduct 按渠道创建扫码会话；GUID 是管家渠道的身份标识，后续换票要用。
func (c *Client) CreateSessionForProduct(ctx context.Context, product string) (*Session, error) {
	oauth, err := OAuthURL(product)
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: c.timeout, Jar: jar, Transport: c.transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oauth, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	m := uuidRE.FindStringSubmatch(string(body))
	if len(m) < 2 {
		return nil, fmt.Errorf("failed to parse WeChat QR uuid")
	}
	id, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	wxuuid := m[1]
	guid, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	return &Session{
		Product:    product,
		ClientGUID: guid,
		ID:         id,
		WXUUID:     wxuuid,
		QRCodeURL:  qrBase + wxuuid,
		CreatedAt:  time.Now(),
		HTTPClient: hc,
		Jar:        jar,
		Status:     "pending",
	}, nil
}

// FetchQRCodeImage 下载指定会话的二维码图片数据。
func (c *Client) FetchQRCodeImage(ctx context.Context, sess *Session) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sess.QRCodeURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := sess.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("QR image HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// 校验返回内容确实是图片。代理或网络拦截时微信会返回 HTML/JSON，
	// 若不校验会把脏数据当成二维码展示，用户看到的是一张坏图却无从判断原因。
	if err := validateQRCodeImage(data); err != nil {
		return nil, err
	}
	return data, nil
}

// PollQRCode 轮询微信长连接端点，获取当前扫码状态。
func (c *Client) PollQRCode(ctx context.Context, sess *Session) (PollResult, error) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.LoginBuffer != "" {
		return PollResult{Status: "confirmed"}, nil
	}
	if sess.AuthorizeCode != "" {
		code := 405
		return PollResult{Status: "authorized", ErrCode: &code, Code: sess.AuthorizeCode}, nil
	}
	u := longPollBase + "?" + url.Values{
		"uuid": {sess.WXUUID},
		"_":    {strconv.FormatInt(time.Now().UnixMilli(), 10)},
	}.Encode()
	reqCtx, cancel := context.WithTimeout(ctx, qrPollWait)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return PollResult{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	// 长轮询必须使用比常规短连接更长的客户端超时。微信在用户未扫码时会把请求挂起
	// 到 35 秒左右，若沿用 RequestTimeout（默认 8 秒）会先被本地掐断，导致扫码页反复报错。
	pollClient := *sess.HTTPClient
	if pollClient.Timeout == 0 || pollClient.Timeout < qrPollLimit {
		pollClient.Timeout = qrPollLimit
	}
	resp, err := pollClient.Do(req)
	if err != nil {
		// 长轮询超时是预期行为：用户还没扫码时微信会一直挂起连接。
		// 降级为 pending 而不是报错，前端可继续轮询，避免扫码页面无故中断。
		// 只有外层 ctx 被取消（用户离开页面）时才如实返回错误。
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			sess.Status = "pending"
			return PollResult{Status: "pending"}, nil
		}
		return PollResult{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	errcode, code := parsePoll(text)
	switch {
	case errcode != nil && *errcode == 408:
		sess.Status = "pending"
		return PollResult{Status: "pending", ErrCode: errcode, Code: code}, nil
	case errcode != nil && *errcode == 404:
		sess.Status = "scanned"
		return PollResult{Status: "scanned", ErrCode: errcode, Code: code}, nil
	case errcode != nil && *errcode == 403:
		sess.Status = "cancelled"
		return PollResult{Status: "cancelled", ErrCode: errcode, Code: code}, nil
	case errcode != nil && *errcode == 402:
		sess.Status = "expired"
		return PollResult{Status: "expired", ErrCode: errcode, Code: code}, nil
	case errcode != nil && *errcode == 405 && code != "":
		sess.AuthorizeCode = code
		sess.Status = "authorized"
		return PollResult{Status: "authorized", ErrCode: errcode, Code: code}, nil
	default:
		sess.Status = "unknown"
		if len(text) > 200 {
			text = text[:200]
		}
		sess.Error = text
		return PollResult{Status: "unknown", ErrCode: errcode, Code: code, Message: text}, nil
	}
}

// GetLoginBuffer 在用户授权后获取 login_buffer 和登录凭证。
func (c *Client) GetLoginBuffer(ctx context.Context, sess *Session) (protocol.LoginBufferResult, error) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.LoginBuffer != "" && sess.Credentials != nil {
		return protocol.LoginBufferResult{LoginBuffer: sess.LoginBuffer, Credentials: *sess.Credentials}, nil
	}
	if sess.AuthorizeCode == "" {
		return protocol.LoginBufferResult{}, fmt.Errorf("QR session is not authorized yet")
	}
	// 管家渠道的换票与取票据是完全不同的一套接口：先用 3078 指令把授权 code
	// 换成管家侧的登录密钥，再用它取小程序票据。走应用宝的回调会直接失败。
	if sess.Product == "pcmgr" {
		if sess.Credentials == nil {
			pc, err := pcmgr.ExchangeCode(ctx, sess.HTTPClient, sess.AuthorizeCode, sess.ClientGUID)
			if err != nil {
				return protocol.LoginBufferResult{}, err
			}
			sess.Credentials = &protocol.LoginBufferCredentials{
				Provider: "pcmgr", PCMgr: pc, OpenID: pc.Identity(), Nickname: pc.Nickname,
				ExpiresAt: pc.NextRenewAt(), ExpiresIn: pc.NextRenewAt() - pc.RenewedAt,
				RefreshTokenObservedAt: time.Now().Unix(),
			}
		}
		buffer, err := c.loginBuffers.FetchLoginBuffer(ctx, *sess.Credentials)
		if err != nil {
			return protocol.LoginBufferResult{}, err
		}
		sess.LoginBuffer = buffer
		sess.Status = "confirmed"
		return protocol.LoginBufferResult{LoginBuffer: buffer, Credentials: *sess.Credentials}, nil
	}
	cb := callbackURL + "?" + url.Values{
		"login_type": {"WX"},
		"code":       {sess.AuthorizeCode},
		"state":      {OAuthState},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cb, nil)
	if err != nil {
		return protocol.LoginBufferResult{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := sess.HTTPClient.Do(req)
	if err != nil {
		return protocol.LoginBufferResult{}, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	creds, err := credentialsFromJar(sess.Jar, cb)
	if err != nil {
		return protocol.LoginBufferResult{}, err
	}
	lb, err := c.loginBuffers.FetchLoginBuffer(ctx, creds)
	if err != nil {
		return protocol.LoginBufferResult{}, err
	}
	sess.Credentials = &creds
	sess.LoginBuffer = lb
	sess.Status = "confirmed"
	return protocol.LoginBufferResult{LoginBuffer: lb, Credentials: creds}, nil
}

// GetLoginBufferFromCode 直接用已有的微信授权 code 换取 login_buffer，
// 跳过扫码轮询环节。用于需要由外部服务回传 code 的场景。
func (c *Client) GetLoginBufferFromCode(ctx context.Context, code string) (protocol.LoginBufferResult, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return protocol.LoginBufferResult{}, fmt.Errorf("authorize code is empty")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return protocol.LoginBufferResult{}, err
	}
	sess := &Session{
		HTTPClient:    &http.Client{Timeout: c.timeout, Jar: jar, Transport: c.transport},
		Jar:           jar,
		AuthorizeCode: code,
		Status:        "authorized",
		CreatedAt:     time.Now(),
	}
	return c.GetLoginBuffer(ctx, sess)
}

// RefreshLoginBuffer 刷新登录凭证并获取新的 login_buffer。
func (c *Client) RefreshLoginBuffer(ctx context.Context, creds protocol.LoginBufferCredentials) (protocol.LoginBufferResult, error) {
	return c.loginBuffers.RefreshLoginBuffer(ctx, creds)
}

// DataURIImage 按数据的真实 MIME 类型生成 data URI。
// 相比硬编码 image/jpeg，PNG 等格式也能被浏览器正确渲染。
func DataURIImage(data []byte) string {
	mime := http.DetectContentType(data)
	if strings.HasPrefix(mime, "application/") || mime == "text/plain; charset=utf-8" {
		// 探测失败时退化为 jpeg，与历史行为保持一致
		mime = "image/jpeg"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// validateQRCodeImage 校验二维码响应是否为可解码的图片。
// 网络或代理拦截时微信会返回 HTML 错误页，这里提前拦截并给出可诊断的提示。
func validateQRCodeImage(data []byte) error {
	if len(data) == 0 {
		return errors.New("二维码响应为空，可能被网络或代理拦截")
	}
	if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("二维码响应不是有效图片（可能被网络或代理拦截）: %w", err)
	}
	return nil
}

func parsePoll(text string) (*int, string) {
	var out *int
	if m := errRE.FindStringSubmatch(text); len(m) == 2 {
		n, _ := strconv.Atoi(m[1])
		out = &n
	}
	code := ""
	if m := codeRE.FindStringSubmatch(text); len(m) == 2 {
		code = m[1]
	}
	return out, code
}

func credentialsFromJar(jar http.CookieJar, rawURL string) (protocol.LoginBufferCredentials, error) {
	u, _ := url.Parse(rawURL)
	var cookies []*http.Cookie
	if u != nil {
		cookies = append(cookies, jar.Cookies(u)...)
	}
	hostURL, _ := url.Parse("https://yybadaccess.3g.qq.com/")
	cookies = append(cookies, jar.Cookies(hostURL)...)
	values := map[string]string{}
	for _, ck := range cookies {
		values[ck.Name] = ck.Value
	}
	openid := values["openid"]
	accessToken := values["accesstoken"]
	if openid == "" || accessToken == "" {
		return protocol.LoginBufferCredentials{}, fmt.Errorf("OAuth callback did not set required cookies")
	}
	expiresIn := int64(7200)
	if values["expires_in"] != "" {
		if n, err := strconv.ParseInt(values["expires_in"], 10, 64); err == nil && n > 0 {
			expiresIn = n
		}
	}
	return protocol.LoginBufferCredentials{
		OpenID:       openid,
		AccessToken:  accessToken,
		RefreshToken: values["refreshtoken"],
		// 扫码走的是 /pc_yyb/pcyyb_oauth，凭据属于 pc_yyb 业务侧。
		// 标下来源，续期才会走对端点——走 _auth 侧续期会被微信拒绝，
		// 表现就是"扫码账户用一段时间后突然需要重扫"。
		RefreshBusinessID: "pc_yyb",
		LoginType:         defaultString(values["logintype"], "WX"),
		Nickname:          values["nickname"],
		ExpiresAt:         time.Now().Unix() + expiresIn,
		ExpiresIn:         expiresIn,
	}, nil
}

func defaultString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
