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
)

// 轮询长连接的超时时间。微信在未扫码时会挂起连接直到超时，属正常现象。
const qrPollWait = 35 * time.Second

// 正则表达式：分别用于提取二维码 UUID、微信错误码和授权码
var (
	uuidRE = regexp.MustCompile(`/connect/qrcode/([^"'>\s]+)`)
	errRE  = regexp.MustCompile(`wx_errcode\s*=\s*(\d+)`)
	codeRE = regexp.MustCompile(`wx_code\s*=\s*'([^']*)'`)
)

// Session 表示一个微信扫码登录会话，持有与微信服务器的 HTTP 连接和 Cookie。
type Session struct {
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
	loginBuffers *protocol.LoginBufferClient
}

// NewClient 创建一个新的二维码客户端。
func NewClient(timeout time.Duration) *Client {
	return &Client{
		timeout:      timeout,
		loginBuffers: protocol.NewLoginBufferClient(timeout),
	}
}

// LoginBuffers 返回 LoginBufferClient 实例，用于获取和刷新 login_buffer。
func (c *Client) LoginBuffers() *protocol.LoginBufferClient { return c.loginBuffers }

// GetQRCodeImage 创建扫码会话并获取二维码图片。
func (c *Client) GetQRCodeImage(ctx context.Context) (ImageResult, error) {
	sess, err := c.CreateSession(ctx)
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
func (c *Client) CreateSession(ctx context.Context) (*Session, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: c.timeout, Jar: jar}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oauthURL, nil)
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
	return &Session{
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
	return io.ReadAll(resp.Body)
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
	resp, err := sess.HTTPClient.Do(req)
	if err != nil {
		// 长轮询超时是预期行为：用户还没扫码时微信会一直挂起连接。
		// 降级为 pending 而不是报错，前端可继续轮询，避免扫码页面无故中断。
		if errors.Is(err, context.DeadlineExceeded) {
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
		LoginType:    defaultString(values["logintype"], "WX"),
		Nickname:     values["nickname"],
		ExpiresAt:    time.Now().Unix() + expiresIn,
		ExpiresIn:    expiresIn,
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
