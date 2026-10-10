package qr

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// 扫码状态解析
// -----------------------------------------------------------------------------

func intPtr(n int) *int { return &n }

// TestParsePoll 覆盖轮询响应的各种形态。
//
// 微信的轮询响应是一段 JS 片段而非 JSON，只能靠正则提取 wx_errcode 与 wx_code，
// 这里把能确定的形态都固化下来，避免改动正则时静默改变扫码流程的判定。
func TestParsePoll(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantCode *int
		wantWx   string
	}{
		{"已授权带 code", "window.wx_code='ABC123';", nil, "ABC123"},
		{"等待扫码带错误码", "window.wx_errcode=408;", intPtr(408), ""},
		{"成功同时带 0 与 code", "wx_errcode=0;wx_code='XYZ';", intPtr(0), "XYZ"},
		{"错误码与等号间有空格", "wx_errcode = 405", intPtr(405), ""},
		{"code 与等号间有空格", "wx_code = 'SPACED'", nil, "SPACED"},
		{"两者都没有", "<html><body>waiting</body></html>", nil, ""},
		{"空响应", "", nil, ""},
		{"code 为空字符串", "wx_code=''", nil, ""},
	}
	for _, c := range cases {
		errCode, wxCode := parsePoll(c.text)
		if c.wantCode == nil {
			if errCode != nil {
				t.Fatalf("%s: 不应解析出错误码，实际 %d", c.name, *errCode)
			}
		} else {
			if errCode == nil {
				t.Fatalf("%s: 期望错误码 %d，实际为 nil", c.name, *c.wantCode)
			} else if *errCode != *c.wantCode {
				t.Fatalf("%s: 期望错误码 %d，实际 %d", c.name, *c.wantCode, *errCode)
			}
		}
		if wxCode != c.wantWx {
			t.Fatalf("%s: 期望 code %q，实际 %q", c.name, c.wantWx, wxCode)
		}
	}
}

// -----------------------------------------------------------------------------
// 二维码图片校验
// -----------------------------------------------------------------------------

// encodeTestPNG 生成一张最小的合法 PNG，用于验证图片校验的正向路径。
func encodeTestPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("生成测试 PNG 失败: %v", err)
	}
	return buf.Bytes()
}

// TestValidateQRCodeImage 确认代理或网络拦截返回的非图片内容会被拦下。
// 这条防线的作用是把「代理返回了 HTML 错误页」变成一条可读的错误，
// 而不是让前端渲染一张坏图。
func TestValidateQRCodeImage(t *testing.T) {
	if err := validateQRCodeImage(encodeTestPNG(t)); err != nil {
		t.Fatalf("合法 PNG 应当通过校验: %v", err)
	}

	cases := []struct {
		name string
		data []byte
	}{
		{"空响应", nil},
		{"HTML 错误页", []byte("<html><body>502 Bad Gateway</body></html>")},
		{"纯文本", []byte("proxy error: connection refused")},
		{"随机字节", []byte{0x00, 0x11, 0x22, 0x33, 0x44}},
		{"被截断的 PNG", encodeTestPNG(t)[:8]},
	}
	for _, c := range cases {
		if err := validateQRCodeImage(c.data); err == nil {
			t.Fatalf("%s 应当被拦截", c.name)
		}
	}
}

// TestDataURIImage 覆盖 MIME 探测的两个分支：正常图片与被拦截的文本。
func TestDataURIImage(t *testing.T) {
	pngURI := DataURIImage(encodeTestPNG(t))
	if !strings.HasPrefix(pngURI, "data:image/png;base64,") {
		t.Fatalf("PNG 应生成 image/png 的 data URI，实际前缀: %.40q", pngURI)
	}
	encoded := strings.TrimPrefix(pngURI, "data:image/png;base64,")
	if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
		t.Fatalf("data URI 的 base64 部分无法解码: %v", err)
	}

	// 探测失败时应退化成 jpeg，保持与历史行为一致，避免前端直接显示不出图
	textURI := DataURIImage([]byte("plain text from proxy"))
	if !strings.HasPrefix(textURI, "data:image/jpeg;base64,") {
		t.Fatalf("探测失败时应退化为 image/jpeg，实际前缀: %.40q", textURI)
	}
}

// -----------------------------------------------------------------------------
// 凭据提取
// -----------------------------------------------------------------------------

// newCookieJarWith 往应用宝域下写入一组 Cookie，模拟 OAuth 回调设置的凭据。
func newCookieJarWith(t *testing.T, pairs map[string]string) *cookiejar.Jar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 CookieJar 失败: %v", err)
	}
	u, err := url.Parse("https://yybadaccess.3g.qq.com/")
	if err != nil {
		t.Fatalf("解析测试域名失败: %v", err)
	}
	cookies := make([]*http.Cookie, 0, len(pairs))
	for k, v := range pairs {
		cookies = append(cookies, &http.Cookie{Name: k, Value: v})
	}
	jar.SetCookies(u, cookies)
	return jar
}

func TestCredentialsFromJar(t *testing.T) {
	jar := newCookieJarWith(t, map[string]string{
		"openid":       "oXXXX-abc123",
		"accesstoken":  "access-token-value",
		"refreshtoken": "refresh-token-value",
		"expires_in":   "3600",
		"nickname":     "张三",
		"logintype":    "WX",
	})

	before := time.Now().Unix()
	creds, err := credentialsFromJar(jar, "https://yybadaccess.3g.qq.com/pc_yyb_auth/callback")
	if err != nil {
		t.Fatalf("从 Cookie 提取凭据失败: %v", err)
	}
	if creds.OpenID != "oXXXX-abc123" {
		t.Fatalf("openid 解析错误: %q", creds.OpenID)
	}
	if creds.AccessToken != "access-token-value" {
		t.Fatalf("access token 解析错误: %q", creds.AccessToken)
	}
	if creds.RefreshToken != "refresh-token-value" {
		t.Fatalf("refresh token 解析错误: %q", creds.RefreshToken)
	}
	if creds.Nickname != "张三" {
		t.Fatalf("昵称解析错误: %q", creds.Nickname)
	}
	if creds.ExpiresIn != 3600 {
		t.Fatalf("有效期应取 Cookie 中的 3600，实际 %d", creds.ExpiresIn)
	}
	if creds.ExpiresAt < before+3600 || creds.ExpiresAt > time.Now().Unix()+3600 {
		t.Fatalf("过期时间戳应约为 now+3600，实际 %d", creds.ExpiresAt)
	}
}

// TestCredentialsFromJarDefaults 确认缺失可选字段时回退到默认值，
// 而不是留下零值导致后续保活判断出错。
func TestCredentialsFromJarDefaults(t *testing.T) {
	jar := newCookieJarWith(t, map[string]string{
		"openid":      "oOnly-required",
		"accesstoken": "at",
	})
	creds, err := credentialsFromJar(jar, "https://yybadaccess.3g.qq.com/callback")
	if err != nil {
		t.Fatalf("只提供必需字段时不应报错: %v", err)
	}
	if creds.LoginType != "WX" {
		t.Fatalf("未指定 logintype 时应回退为 WX，实际 %q", creds.LoginType)
	}
	if creds.ExpiresIn != 7200 {
		t.Fatalf("未指定 expires_in 时应回退为 7200，实际 %d", creds.ExpiresIn)
	}
}

// TestCredentialsFromJarIgnoresBadExpiresIn 确认非法有效期不会污染凭据——
// 否则一个错误的值会让保活任务做出完全错误的续期判断。
func TestCredentialsFromJarIgnoresBadExpiresIn(t *testing.T) {
	for _, bad := range []string{"not-a-number", "0", "-100", ""} {
		jar := newCookieJarWith(t, map[string]string{
			"openid":      "oBad",
			"accesstoken": "at",
			"expires_in":  bad,
		})
		creds, err := credentialsFromJar(jar, "https://yybadaccess.3g.qq.com/callback")
		if err != nil {
			t.Fatalf("expires_in=%q 不应导致失败: %v", bad, err)
		}
		if creds.ExpiresIn != 7200 {
			t.Fatalf("expires_in=%q 应回退为 7200，实际 %d", bad, creds.ExpiresIn)
		}
	}
}

// TestCredentialsFromJarRequiresOpenIDAndToken 确认必需字段缺失时明确报错，
// 而不是带着残缺凭据继续往下走。
func TestCredentialsFromJarRequiresOpenIDAndToken(t *testing.T) {
	cases := []struct {
		name  string
		pairs map[string]string
	}{
		{"缺少 openid", map[string]string{"accesstoken": "at"}},
		{"缺少 accesstoken", map[string]string{"openid": "oX"}},
		{"两者都缺", map[string]string{"nickname": "张三"}},
		{"没有 Cookie", map[string]string{}},
	}
	for _, c := range cases {
		jar := newCookieJarWith(t, c.pairs)
		if _, err := credentialsFromJar(jar, "https://yybadaccess.3g.qq.com/callback"); err == nil {
			t.Fatalf("%s 应当返回错误", c.name)
		}
	}
}

// -----------------------------------------------------------------------------
// 小工具
// -----------------------------------------------------------------------------

func TestRandomHex(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		s, err := randomHex(16)
		if err != nil {
			t.Fatalf("生成随机串失败: %v", err)
		}
		// 16 字节编码成 32 个十六进制字符
		if len(s) != 32 {
			t.Fatalf("randomHex(16) 应返回 32 个字符，实际 %d", len(s))
		}
		if strings.TrimLeft(s, "0123456789abcdef") != "" {
			t.Fatalf("randomHex 应只返回小写十六进制: %q", s)
		}
		seen[s] = true
	}
	if len(seen) != 16 {
		t.Fatalf("16 次调用只生成 %d 个不同随机串，随机源可能失效", len(seen))
	}
}

func TestDefaultString(t *testing.T) {
	if got := defaultString("", "fallback"); got != "fallback" {
		t.Fatalf("空字符串应返回默认值，实际 %q", got)
	}
	if got := defaultString("value", "fallback"); got != "value" {
		t.Fatalf("非空字符串应原样返回，实际 %q", got)
	}
}
