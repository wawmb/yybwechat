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

// newAuthenticatedApp 创建一个已完成管理员初始化的 App，并返回可用的会话 Cookie。
// 管理类路由在启用认证后需要登录才能访问，测试需先完成登录。
func newAuthenticatedApp(t *testing.T) (*App, *http.Cookie) {
	t.Helper()
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

	if _, err := app.auth.CreateAdmin(context.Background(), "tester", "password123"); err != nil {
		t.Fatalf("CreateAdmin() error = %v", err)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(
		http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"username":"tester","password":"password123"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == sessionCookie {
			return app, ck
		}
	}
	t.Fatalf("login response did not set session cookie")
	return nil, nil
}

// TestHandlerServesGinRoutesAndSwaggerDocs 验证所有核心路由是否正常工作。
func TestHandlerServesGinRoutesAndSwaggerDocs(t *testing.T) {
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
	defer app.Close()

	handler := app.Handler()

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d", health.Code)
	}
	var healthBody struct {
		Code int            `json:"code"`
		Msg  string         `json:"msg"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(health.Body.Bytes(), &healthBody); err != nil {
		t.Fatalf("decode health JSON: %v", err)
	}
	if healthBody.Code != 0 || healthBody.Msg != "success" || healthBody.Data["ok"] != true {
		t.Fatalf("GET /health body = %#v", healthBody)
	}

	openapi := httptest.NewRecorder()
	handler.ServeHTTP(openapi, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if openapi.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d", openapi.Code)
	}
	var spec map[string]any
	if err := json.Unmarshal(openapi.Body.Bytes(), &spec); err != nil {
		t.Fatalf("decode OpenAPI JSON: %v", err)
	}
	if spec["openapi"] != "3.0.3" {
		t.Fatalf("openapi version = %v", spec["openapi"])
	}
	if _, ok := spec["code"]; ok {
		t.Fatalf("OpenAPI JSON should not be wrapped in API envelope")
	}
	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatalf("OpenAPI paths missing or invalid")
	}
	for _, path := range []string{"/wxapp/getCode", "/wxapp/getPhoneNumber", "/wxapp/operateWxData", "/accounts/avatar"} {
		if _, ok := paths[path]; !ok {
			t.Fatalf("OpenAPI path %s missing", path)
		}
	}
	for _, path := range []string{"/wxapp/getCode", "/wxapp/getPhoneNumber", "/wxapp/operateWxData"} {
		pathItem := paths[path].(map[string]any)
		post := pathItem["post"].(map[string]any)
		tags := post["tags"].([]any)
		if len(tags) != 1 || tags[0] != "wxapp" {
			t.Fatalf("OpenAPI path %s tags = %#v, want [wxapp]", path, tags)
		}
	}
	for _, path := range []string{"/accounts/{ref}", "/accounts/{ref}/getCode", "/accounts/{ref}/getPhoneNumber", "/accounts/{ref}/operateWxData", "/accounts/getCode", "/accounts/getPhoneNumber", "/accounts/operateWxData"} {
		if _, ok := paths[path]; ok {
			t.Fatalf("OpenAPI still exposes old account feature route %s", path)
		}
	}
	if _, ok := paths["/features"]; ok {
		t.Fatalf("OpenAPI still exposes /features")
	}

	// 未登录访问管理类页面时应重定向到登录页
	docsAnon := httptest.NewRecorder()
	handler.ServeHTTP(docsAnon, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if docsAnon.Code != http.StatusSeeOther {
		t.Fatalf("GET /docs (anonymous) status = %d", docsAnon.Code)
	}
	// 该测试用的 App 尚未创建管理员，因此应引导到初始化页而非登录页
	if got := docsAnon.Header().Get("Location"); got != "/setup" {
		t.Fatalf("GET /docs (anonymous) Location = %q, want /setup", got)
	}

	// 登录后应正常跳转到 Swagger 页面
	authedApp, cookie := newAuthenticatedApp(t)
	docs := httptest.NewRecorder()
	docsReq := httptest.NewRequest(http.MethodGet, "/docs", nil)
	docsReq.AddCookie(cookie)
	authedApp.Handler().ServeHTTP(docs, docsReq)
	if docs.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /docs status = %d", docs.Code)
	}
	if got := docs.Header().Get("Location"); got != "/docs/index.html" {
		t.Fatalf("GET /docs Location = %q", got)
	}

	features := httptest.NewRecorder()
	handler.ServeHTTP(features, httptest.NewRequest(http.MethodGet, "/features", nil))
	if features.Code != http.StatusNotFound {
		t.Fatalf("GET /features status = %d", features.Code)
	}
	var notFoundBody struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data any    `json:"data"`
	}
	if err := json.Unmarshal(features.Body.Bytes(), &notFoundBody); err != nil {
		t.Fatalf("decode /features error JSON: %v", err)
	}
	if notFoundBody.Code == 0 || notFoundBody.Msg == "" || notFoundBody.Data != nil {
		t.Fatalf("GET /features body = %#v", notFoundBody)
	}

	oldPath := httptest.NewRecorder()
	handler.ServeHTTP(oldPath, httptest.NewRequest(http.MethodPost, "/accounts/getCode", nil))
	if oldPath.Code != http.StatusNotFound {
		t.Fatalf("POST old account feature route status = %d", oldPath.Code)
	}
}

// TestAuthProtectsManagementRoutes 验证管理类接口在启用认证后必须登录才能访问。
func TestAuthProtectsManagementRoutes(t *testing.T) {
	app, cookie := newAuthenticatedApp(t)
	handler := app.Handler()

	// 未登录：机器调用类接口返回 401，而不是重定向
	anon := httptest.NewRecorder()
	handler.ServeHTTP(anon, httptest.NewRequest(http.MethodGet, "/accounts/refresh", nil))
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("GET /accounts/refresh (anonymous) status = %d, want 401", anon.Code)
	}

	// 匿名 DELETE /accounts 必须被拒绝：写操作不能因为「列表要免认证」而一起放开
	anonDelete := httptest.NewRecorder()
	handler.ServeHTTP(anonDelete, httptest.NewRequest(http.MethodDelete, "/accounts?ref=1", nil))
	if anonDelete.Code != http.StatusUnauthorized {
		t.Fatalf("DELETE /accounts (anonymous) status = %d, want 401", anonDelete.Code)
	}

	// 已登录：正常返回账号列表
	authed := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	req.AddCookie(cookie)
	handler.ServeHTTP(authed, req)
	if authed.Code != http.StatusOK {
		t.Fatalf("GET /accounts status = %d, body = %s", authed.Code, authed.Body.String())
	}

	// 口令错误时不下发会话
	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, httptest.NewRequest(
		http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"username":"tester","password":"wrong-password"}`)))
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("login with wrong password status = %d, want 401, body = %s", bad.Code, bad.Body.String())
	}
	if len(bad.Result().Cookies()) > 0 {
		t.Fatalf("failed login must not set session cookie")
	}

	// 已登录用户访问登录页应直接回到首页
	login := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodGet, "/login", nil)
	loginReq.AddCookie(cookie)
	handler.ServeHTTP(login, loginReq)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("GET /login (authenticated) status = %d, want 303", login.Code)
	}
}

// TestAccountsListIsPublicForScripts 锁死自动化脚本的调用契约：
// 匿名 GET /accounts 必须 200，且不能泄露 login_buffer / credentials。
//
// 这是脚本调用链的第一步（探测服务 + 拉取在线账号），历史上曾因为把它挪进
// 需登录的管理组而返回 401，直接打断青龙面板等无人值守任务。此测试防止回归。
func TestAccountsListIsPublicForScripts(t *testing.T) {
	app, _ := newAuthenticatedApp(t)
	handler := app.Handler()

	nick := "脚本测试号"
	status := "alive"
	acc, err := app.db.UpsertAccount(context.Background(),
		"openid-script-test",
		"SUPER-SECRET-LOGIN-BUFFER",
		nil, &nick, nil,
		map[string]any{"nickname": nick},
		map[string]any{"accesstoken": "SUPER-SECRET-TOKEN", "refreshtoken": "SUPER-SECRET-REFRESH"},
		&status)
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}

	// 完全匿名、无任何 Cookie 或 Authorization 头，模拟脚本的裸请求
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /accounts (anonymous) status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}

	var envelope struct {
		Code int                          `json:"code"`
		Msg  string                       `json:"msg"`
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode /accounts JSON: %v, body = %s", err, rec.Body.String())
	}
	if envelope.Code != 0 {
		t.Fatalf("/accounts code = %d, want 0, body = %s", envelope.Code, rec.Body.String())
	}
	if len(envelope.Data) != 1 {
		t.Fatalf("/accounts data length = %d, want 1", len(envelope.Data))
	}

	// 脚本依赖的字段必须存在
	for _, field := range []string{"id", "openid", "nickname", "status"} {
		if _, ok := envelope.Data[0][field]; !ok {
			t.Fatalf("/accounts data[0] missing field %q, got keys %v", field, keysOf(envelope.Data[0]))
		}
	}
	// 敏感字段一律不得出现在公开视图里
	if raw := rec.Body.String(); strings.Contains(raw, "SUPER-SECRET-LOGIN-BUFFER") ||
		strings.Contains(raw, "SUPER-SECRET-TOKEN") || strings.Contains(raw, "SUPER-SECRET-REFRESH") {
		t.Fatalf("GET /accounts leaked secret material: %s", raw)
	}
	if acc == nil {
		t.Fatalf("UpsertAccount returned nil account")
	}
}

// keysOf 取出 map 的键集合，仅用于测试失败时打印可读的诊断信息。
func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAuthStatusRedirectsToSetup 验证未初始化管理员时页面访问会跳转到初始化页。
func TestAuthStatusRedirectsToSetup(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	app, err := NewApp(Config{ResourceRoot: t.TempDir(), RequestTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	defer app.Close()
	handler := app.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET / (no admin) status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/setup" {
		t.Fatalf("GET / (no admin) Location = %q, want /setup", got)
	}

	status := httptest.NewRecorder()
	handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/auth/status", nil))
	if status.Code != http.StatusOK {
		t.Fatalf("GET /api/auth/status status = %d", status.Code)
	}
	var body struct {
		Data struct {
			Initialized   bool `json:"initialized"`
			Authenticated bool `json:"authenticated"`
		} `json:"data"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status JSON: %v", err)
	}
	if body.Data.Initialized {
		t.Fatalf("initialized = true, want false before admin creation")
	}
}

// TestProtocolTokenGate 验证配置协议令牌后 /wxapp/* 必须携带 Bearer 令牌。
func TestProtocolTokenGate(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	app, err := NewApp(Config{
		ResourceRoot:   t.TempDir(),
		RequestTimeout: time.Second,
		ProtocolToken:  "secret-token",
	})
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	defer app.Close()
	handler := app.Handler()

	// 缺省令牌：拒绝
	noToken := httptest.NewRecorder()
	handler.ServeHTTP(noToken, httptest.NewRequest(
		http.MethodPost, "/wxapp/getCode", strings.NewReader(`{}`)))
	if noToken.Code != http.StatusUnauthorized {
		t.Fatalf("POST /wxapp/getCode (no token) status = %d, want 401", noToken.Code)
	}

	// 错误令牌：拒绝
	badToken := httptest.NewRecorder()
	badReq := httptest.NewRequest(http.MethodPost, "/wxapp/getCode", strings.NewReader(`{}`))
	badReq.Header.Set("Authorization", "Bearer wrong")
	handler.ServeHTTP(badToken, badReq)
	if badToken.Code != http.StatusUnauthorized {
		t.Fatalf("POST /wxapp/getCode (bad token) status = %d, want 401", badToken.Code)
	}

	// 正确令牌：通过门禁进入业务处理（此处因缺少 ref 返回 400，说明已放行）
	okReq := httptest.NewRequest(http.MethodPost, "/wxapp/getCode", strings.NewReader(`{}`))
	okReq.Header.Set("Authorization", "Bearer secret-token")
	okRec := httptest.NewRecorder()
	handler.ServeHTTP(okRec, okReq)
	if okRec.Code == http.StatusUnauthorized {
		t.Fatalf("POST /wxapp/getCode (valid token) should pass the token gate")
	}
}
