package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// decodeEnvelope 解开统一响应信封，返回 data 部分。
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		Code int            `json:"code"`
		Msg  string         `json:"msg"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode envelope: %v, body=%s", err, rec.Body.String())
	}
	return body.Data
}

// TestPasswordChangeRequiresSession 锁定一个曾经真实存在的缺陷：
// /api/auth/password 一度注册在公开分组里，处理器从 context 取当前管理员
// 永远取不到值，导致改密码接口恒定返回 401，前端设置项完全不可用。
func TestPasswordChangeRequiresSession(t *testing.T) {
	app, cookie := newAuthenticatedApp(t)

	// 未登录：必须是 401，而不是 200 或 500
	anon := httptest.NewRecorder()
	app.Handler().ServeHTTP(anon, httptest.NewRequest(http.MethodPut, "/api/auth/password",
		strings.NewReader(`{"old_password":"password123","new_password":"newpassword123"}`)))
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous PUT /api/auth/password status = %d, want 401", anon.Code)
	}

	// 已登录：必须真的改成功
	req := httptest.NewRequest(http.MethodPut, "/api/auth/password",
		strings.NewReader(`{"old_password":"password123","new_password":"newpassword123"}`))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated PUT /api/auth/password status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 新口令应当能登录，旧口令应当失效
	if _, err := app.auth.Authenticate(context.Background(), "tester", "newpassword123"); err != nil {
		t.Fatalf("login with new password failed: %v", err)
	}
	if _, err := app.auth.Authenticate(context.Background(), "tester", "password123"); err == nil {
		t.Fatalf("login with old password should fail after change")
	}
}

// TestWXOAuthAliasMatchesWXAppOAuth 覆盖 /wx/oauth 兼容别名。
//
// 早期自动化脚本调用的是 /wx/oauth，移植时一度只保留了 /wxapp/oauth，
// 老脚本会直接 404。两个路径必须产出同样的结构化提示字段。
func TestWXOAuthAliasMatchesWXAppOAuth(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	app, err := NewApp(Config{
		ResourceRoot:   t.TempDir(),
		RequestTimeout: time.Second,
		AvatarTimeout:  time.Second,
		SessionTTL:     time.Minute,
		QRSessionTTL:   time.Minute,
	})
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	// appid 是旧字段名，应与 app_id 等价
	payload := `{"appid":"wx1234567890abcdef","redirect_uri":"https://example.com/cb","scope":"snsapi_base"}`

	for _, path := range []string{"/wxapp/oauth", "/wx/oauth"} {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload)))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s status = %d, body = %s", path, rec.Code, rec.Body.String())
		}
		data := decodeEnvelope(t, rec)
		if data["url"] == "" || data["full_url"] == "" {
			t.Fatalf("POST %s returned no authorization url: %v", path, data)
		}
		if data["authorization_required"] != true {
			t.Fatalf("POST %s missing authorization_required=true: %v", path, data)
		}
		if data["callback_code_source"] != "redirect_uri" {
			t.Fatalf("POST %s callback_code_source = %v", path, data["callback_code_source"])
		}
		if params, ok := data["callback_params"].([]any); !ok || len(params) != 2 {
			t.Fatalf("POST %s callback_params = %v", path, data["callback_params"])
		}
		if data["appid"] != "wx1234567890abcdef" {
			t.Fatalf("POST %s appid = %v, want legacy field to be honored", path, data["appid"])
		}
	}
}

// TestKeepAliveCoversDynamicProxyAccounts 覆盖动态代理账号的保活行为。
//
// 动态代理账号曾经被整条排除在自动保活之外，理由是"每次提取都换出口，
// 用它刷新维持不了会话、还白耗额度"。代价是这类账号永远得不到保活，
// 撑到 expires_at 就死掉——等于放弃了一批账号。现在照常参与保活，
// 额度问题由代理租约解决（租约有效期内复用同一出口）。
//
// 刷新通道本身仍沿用账号代理，不另开直连：刷新出口与扫码/协议出口
// 必须一致，否则微信侧看到的来源身份就变了。
func TestKeepAliveCoversDynamicProxyAccounts(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	app, err := NewApp(Config{
		ResourceRoot:      t.TempDir(),
		RequestTimeout:    time.Second,
		AvatarTimeout:     time.Second,
		SessionTTL:        time.Minute,
		QRSessionTTL:      time.Minute,
		KeepAliveInterval: time.Minute,
		KeepAliveAhead:    time.Hour,
	})
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	ctx := context.Background()
	now := time.Now().Unix()
	// 凭据已过期 —— 若是直连账号就应该被扫描到
	expiredCreds := map[string]any{
		"openid":        "openid-dynamic",
		"access_token":  "token",
		"expires_at":    now - 60,
		"refresh_token": "refresh",
	}
	acc, err := app.db.UpsertAccount(ctx, "openid-dynamic", "buffer", nil, nil, nil, nil, expiredCreds, stringPtrMaybe("alive"))
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}

	// 直连（默认）应当需要刷新
	if app.keepAliveShouldSkip(acc) {
		t.Fatalf("direct-proxy account with expired credentials should not be skipped")
	}

	// 切到动态代理后仍然要参与自动保活：否则这类账号撑到 expires_at 就死
	if _, err := app.db.UpsertAccountProxySetting(ctx, acc.ID, "api", "http", "",
		"https://api.example.com/get?num=1", nil, "", "", "", 600, nil); err != nil {
		t.Fatalf("UpsertAccountProxySetting() error = %v", err)
	}
	refreshed, err := app.db.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if app.keepAliveShouldSkip(refreshed) {
		t.Fatalf("dynamic-proxy account should be kept alive, otherwise it dies at expires_at")
	}
}

// TestProtocolTokenGateAllowsConsoleSession 锁定一个容易踩的坑：
//
// /accounts 与 /wxapp/* 都挂在协议令牌门禁下，但控制台页面本身就要调它们，
// 而浏览器只会带会话 Cookie、不会带协议令牌。门禁若只认令牌，
// 配了 --protocol-token 之后控制台会全线 401，前端又把 401 当「会话过期」
// 直接跳回登录页——表现就是「一开令牌就登不进控制台」。
func TestProtocolTokenGateAllowsConsoleSession(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	const protoToken = "smoke-protocol-token"
	app, err := NewApp(Config{
		ResourceRoot:   t.TempDir(),
		RequestTimeout: time.Second,
		AvatarTimeout:  time.Second,
		SessionTTL:     time.Minute,
		QRSessionTTL:   time.Minute,
		ProtocolToken:  protoToken,
	})
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	// 建管理员并登录，拿到浏览器会话 Cookie
	if _, err := app.auth.CreateAdmin(context.Background(), "tester", "password123"); err != nil {
		t.Fatalf("CreateAdmin() error = %v", err)
	}
	login := httptest.NewRecorder()
	app.Handler().ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"username":"tester","password":"password123"}`)))
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", login.Code, login.Body.String())
	}
	var cookie *http.Cookie
	for _, ck := range login.Result().Cookies() {
		if ck.Name == sessionCookie {
			cookie = ck
		}
	}
	if cookie == nil {
		t.Fatalf("login did not set session cookie")
	}

	cases := []struct {
		name    string
		headers map[string]string
		cookie  *http.Cookie
		want    int
	}{
		{name: "无凭据", want: http.StatusUnauthorized},
		{name: "错误令牌", headers: map[string]string{"Authorization": "Bearer wrong"}, want: http.StatusUnauthorized},
		{name: "正确令牌", headers: map[string]string{"Authorization": "Bearer " + protoToken}, want: http.StatusOK},
		{name: "管理员会话 Cookie", cookie: cookie, want: http.StatusOK},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
		for k, v := range tc.headers {
			req.Header.Set(k, v)
		}
		if tc.cookie != nil {
			req.AddCookie(tc.cookie)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s: GET /accounts status = %d, want %d, body = %s",
				tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}

	// 控制台的调用面板同样要能用：不能返回 401
	panelReq := httptest.NewRequest(http.MethodPost, "/wxapp/getCode",
		strings.NewReader(`{"ref":"missing","app_id":"wx141bfb9b73c970a9"}`))
	panelReq.AddCookie(cookie)
	panelRec := httptest.NewRecorder()
	app.Handler().ServeHTTP(panelRec, panelReq)
	if panelRec.Code == http.StatusUnauthorized {
		t.Fatalf("console session should pass the protocol gate for /wxapp/*, got 401")
	}
}
