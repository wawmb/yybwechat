package oauth

import (
	"net/url"
	"strings"
	"testing"
)

// TestBuildGeneratesAuthorizeURL 校验参数被正确拼进授权链接，
// 且 FullURL 带上了微信要求的 #wechat_redirect 片段。
func TestBuildGeneratesAuthorizeURL(t *testing.T) {
	res, err := Build(Request{
		AppID:       "wx141bfb9b73c970a9",
		RedirectURI: "https://example.com/callback",
		Scope:       "snsapi_userinfo",
		State:       "abc123",
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if res.Scope != "snsapi_userinfo" || res.State != "abc123" {
		t.Fatalf("scope/state 未回传: %q / %q", res.Scope, res.State)
	}
	if !strings.HasSuffix(res.FullURL, "#wechat_redirect") {
		t.Fatalf("FullURL 缺少 #wechat_redirect: %s", res.FullURL)
	}
	if !strings.HasPrefix(res.FullURL, res.URL) {
		t.Fatalf("FullURL 应包含 URL: %s", res.FullURL)
	}

	u, err := url.Parse(res.URL)
	if err != nil {
		t.Fatalf("解析生成的 URL 失败: %v", err)
	}
	q := u.Query()
	want := map[string]string{
		"appid":         "wx141bfb9b73c970a9",
		"redirect_uri":  "https://example.com/callback",
		"response_type": "code",
		"scope":         "snsapi_userinfo",
		"state":         "abc123",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("参数 %s = %q，期望 %q", k, got, v)
		}
	}
	if q.Has("component_appid") {
		t.Error("未传 component_appid 时不应出现该参数")
	}
}

// TestBuildDefaults 校验 scope 缺省为 snsapi_base，state 缺省为随机生成。
func TestBuildDefaults(t *testing.T) {
	a, err := Build(Request{AppID: "wx141bfb9b73c970a9", RedirectURI: "https://e.com/cb"})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if a.Scope != "snsapi_base" {
		t.Errorf("缺省 scope = %q，期望 snsapi_base", a.Scope)
	}
	if len(a.State) != 32 {
		t.Errorf("随机 state 长度 = %d，期望 32（16 字节 hex）", len(a.State))
	}

	b, err := Build(Request{AppID: "wx141bfb9b73c970a9", RedirectURI: "https://e.com/cb"})
	if err != nil {
		t.Fatalf("第二次 Build() error = %v", err)
	}
	if a.State == b.State {
		t.Error("两次缺省调用应生成不同的随机 state（否则失去防 CSRF 意义）")
	}
}

// TestBuildRejectsInvalidInput 覆盖所有参数校验分支——
// 这些分支是防止构造出钓鱼链接或畸形请求的关键。
func TestBuildRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		req  Request
	}{
		{"AppID 为空", Request{RedirectURI: "https://e.com/cb"}},
		{"AppID 位数不足", Request{AppID: "wx141bfb9b73c970", RedirectURI: "https://e.com/cb"}},
		{"AppID 不以 wx 开头", Request{AppID: "gh141bfb9b73c970a9", RedirectURI: "https://e.com/cb"}},
		{"回调地址为空", Request{AppID: "wx141bfb9b73c970a9"}},
		{"回调地址相对路径", Request{AppID: "wx141bfb9b73c970a9", RedirectURI: "/callback"}},
		{"回调地址非 http(s)", Request{AppID: "wx141bfb9b73c970a9", RedirectURI: "ftp://e.com/cb"}},
		{"回调地址带 userinfo（钓鱼风险）", Request{AppID: "wx141bfb9b73c970a9", RedirectURI: "https://user:pwd@e.com/cb"}},
		{"回调地址带 fragment", Request{AppID: "wx141bfb9b73c970a9", RedirectURI: "https://e.com/cb#frag"}},
		{"scope 非法", Request{AppID: "wx141bfb9b73c970a9", RedirectURI: "https://e.com/cb", Scope: "snsapi_admin"}},
		{"state 超长", Request{AppID: "wx141bfb9b73c970a9", RedirectURI: "https://e.com/cb", State: strings.Repeat("x", 129)}},
		{"state 含换行", Request{AppID: "wx141bfb9b73c970a9", RedirectURI: "https://e.com/cb", State: "a\nb"}},
		{"component_appid 非法", Request{AppID: "wx141bfb9b73c970a9", RedirectURI: "https://e.com/cb", ComponentAppID: "bad"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Build(tc.req); err == nil {
				t.Fatalf("Build(%+v) 应被拒绝，却返回了成功", tc.req)
			}
		})
	}
}

// TestBuildKeepsComponentAppID 校验第三方平台场景下 component_appid 会被带上。
func TestBuildKeepsComponentAppID(t *testing.T) {
	res, err := Build(Request{
		AppID:          "wx141bfb9b73c970a9",
		RedirectURI:    "https://e.com/cb",
		ComponentAppID: "wxaaaaaaaaaaaaaaaa",
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	u, _ := url.Parse(res.URL)
	if got := u.Query().Get("component_appid"); got != "wxaaaaaaaaaaaaaaaa" {
		t.Fatalf("component_appid = %q，期望原样保留", got)
	}
}

// TestValidateRedirectURI 单独覆盖回调地址的边界条件。
func TestValidateRedirectURI(t *testing.T) {
	if _, err := validateRedirectURI(strings.Repeat("https://e.com/", 200) + "x"); err == nil {
		t.Error("超过 2048 字符的回调地址应被拒绝")
	}
	got, err := validateRedirectURI("  https://e.com/cb  ")
	if err != nil {
		t.Fatalf("首尾空白应被裁剪: %v", err)
	}
	if got != "https://e.com/cb" {
		t.Fatalf("裁剪结果 = %q，期望 %q", got, "https://e.com/cb")
	}
}
