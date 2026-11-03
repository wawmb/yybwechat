package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"yybwechat/internal/qr"
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
	for _, path := range []string{"/wxapp/getCode", "/wxapp/getPhoneNumber", "/wxapp/operateWxData", "/accounts/avatar",
		"/wx/code", "/wx/encryptkey", "/wx/getlatestuserkey", "/wx/getuserinfo", "/wx/getphonenumber"} {
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

// TestWXAliasRoutes 验证 /wx/* 兼容入口确实注册且受同一套校验约束。
//
// 这些路径等价于 /wxapp/*，但用的是客户端侧的命名习惯。
// 三个关键约束：
//   - 路由已注册（不是 404）；
//   - 缺 payload 时明确指出缺什么，而不是把空 payload 发给微信侧；
//   - 配置了协议令牌后必须被拦下（401），不能因为换了路径就绕过门禁。
func TestWXAliasRoutes(t *testing.T) {
	t.Setenv("GIN_MODE", "test")

	newApp := func(token string) *App {
		t.Helper()
		app, err := NewApp(Config{
			ResourceRoot:   t.TempDir(),
			RequestTimeout: time.Second,
			AvatarTimeout:  time.Second,
			SessionTTL:     time.Minute,
			QRSessionTTL:   time.Minute,
			ProtocolToken:  token,
		})
		if err != nil {
			t.Fatalf("NewApp() error = %v", err)
		}
		t.Cleanup(func() { _ = app.Close() })
		return app
	}

	handler := newApp("").Handler()

	// 只接受 POST 的别名：GET 应返回 405（说明路由存在且方法受限）
	for _, path := range []string{
		"/wx/code", "/wx/getphonenumber", "/wx/encryptkey", "/wx/getlatestuserkey",
		"/wx/cloud", "/wx/mpgeta8key", "/wx/appmsgext", "/wx/appmsglike",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s status = %d, want %d", path, rec.Code, http.StatusMethodNotAllowed)
		}
	}

	// getUserInfo 别名允许 GET，但缺 ref 时应报参数错误而不是 404
	userInfo := httptest.NewRecorder()
	handler.ServeHTTP(userInfo, httptest.NewRequest(http.MethodGet, "/wx/getuserinfo", nil))
	if userInfo.Code != http.StatusBadRequest {
		t.Fatalf("GET /wx/getuserinfo without ref status = %d, want %d", userInfo.Code, http.StatusBadRequest)
	}

	// 缺 payload 必须报清楚，避免把空 payload 当成合法请求发出去
	for _, path := range []string{"/wx/encryptkey", "/wx/getlatestuserkey", "/wx/cloud"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(`{"ref":"1","app_id":"wx0000000000000000"}`))
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "payload is required") {
			t.Fatalf("POST %s without payload = %d %s", path, rec.Code, rec.Body.String())
		}
	}

	// 配置令牌后，别名入口同样必须被门禁拦下
	gated := newApp("secret-token").Handler()
	for _, path := range []string{"/wx/encryptkey", "/wx/getlatestuserkey", "/wx/cloud", "/wx/code"} {
		rec := httptest.NewRecorder()
		gated.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("POST %s without token status = %d, want %d", path, rec.Code, http.StatusUnauthorized)
		}
	}
}

// TestNormalizeEncryptKeyPayload 验证客户端拼写会被改写成服务端操作名，
// 且其余业务字段原样保留。
func TestNormalizeEncryptKeyPayload(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]any
		want string
	}{
		{name: "顶层客户端拼写", in: map[string]any{"api_name": "getLatestUserKey", "data": map[string]any{"appid": "wx-test"}}, want: encryptKeyOperation},
		{name: "嵌套客户端拼写", in: map[string]any{"data": map[string]any{"api_name": "getLatestUserKey", "version": 2}}, want: encryptKeyOperation},
		{name: "旧版服务端拼写", in: map[string]any{"api_name": "getUserEncryptKey"}, want: encryptKeyOperation},
		{name: "服务端拼写保持不变", in: map[string]any{"api_name": encryptKeyOperation}, want: encryptKeyOperation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeEncryptKeyPayload(tt.in)
			if got["api_name"] == tt.want {
				return
			}
			if nested, ok := got["data"].(map[string]any); ok && nested["api_name"] == tt.want {
				return
			}
			t.Fatalf("normalized payload = %#v, want api_name %q", got, tt.want)
		})
	}

	// 归一化不能把其他操作名改掉
	other := normalizeEncryptKeyPayload(map[string]any{"api_name": "appmsgext"})
	if other["api_name"] != "appmsgext" {
		t.Fatalf("unrelated api_name changed to %v", other["api_name"])
	}
	// 不能修改调用方传入的 map（并发调用时可能被多处共享）
	src := map[string]any{"api_name": "getLatestUserKey"}
	_ = normalizeEncryptKeyPayload(src)
	if src["api_name"] != "getLatestUserKey" {
		t.Fatalf("normalizeEncryptKeyPayload mutated input: %#v", src)
	}
	if normalizeEncryptKeyPayload(nil) != nil {
		t.Fatalf("nil payload should stay nil")
	}
}

// TestOpenAPIScopesProtocolBearer 验证 Bearer 安全定义只作用于协议接口。
//
// 文档范围必须和路由门禁一致：如果管理接口也被标成「需要 Bearer」，
// 使用者会以为令牌能替代网页登录；反过来协议接口漏标，
// Swagger 里就不会带 Authorization 头，调一次 401 一次。
func TestOpenAPIScopesProtocolBearer(t *testing.T) {
	spec := newOpenAPISpec()

	components, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatalf("spec.components missing")
	}
	schemes, ok := components["securitySchemes"].(map[string]any)
	if !ok {
		t.Fatalf("spec.components.securitySchemes missing")
	}
	bearer, ok := schemes["ProtocolBearer"].(map[string]any)
	if !ok {
		t.Fatalf("ProtocolBearer scheme missing")
	}
	if bearer["type"] != "http" || bearer["scheme"] != "bearer" {
		t.Fatalf("ProtocolBearer = %#v, want http/bearer", bearer)
	}

	paths := spec["paths"].(map[string]any)
	hasSecurity := func(path string) (bool, bool) {
		item, ok := paths[path].(map[string]any)
		if !ok {
			t.Fatalf("path %s missing", path)
		}
		found401 := false
		for _, value := range item {
			operation := value.(map[string]any)
			security, _ := operation["security"].([]map[string][]string)
			for _, entry := range security {
				if _, ok := entry["ProtocolBearer"]; ok {
					if responses, ok := operation["responses"].(map[string]any); ok {
						_, found401 = responses["401"]
					}
					return true, found401
				}
			}
		}
		return false, found401
	}

	for _, path := range []string{"/wxapp/getCode", "/wxapp/operateWxData", "/wx/encryptkey", "/wx/getlatestuserkey", "/wx/code"} {
		secured, found401 := hasSecurity(path)
		if !secured {
			t.Fatalf("%s 未声明 ProtocolBearer 安全要求", path)
		}
		if !found401 {
			t.Fatalf("%s 未声明 401 响应", path)
		}
	}

	// 管理类与公开接口不能被标成需要协议令牌
	for _, path := range []string{"/accounts/avatar", "/accounts/status", "/settings/webhook", "/health"} {
		if secured, _ := hasSecurity(path); secured {
			t.Fatalf("%s 不应要求协议令牌", path)
		}
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

// TestQRStatusTerminalDropsSession 验证二维码终态会立刻回收会话。
//
// 不回收的后果有两个：迟到的 confirm 会拿到一个看着还能用的会话白撞一次微信侧；
// 内存与磁盘上的二维码图片要等到「下一次创建二维码」才可能被清掉。
func TestQRStatusTerminalDropsSession(t *testing.T) {
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

	put := func(id string) {
		app.mu.Lock()
		app.qrSessions[id] = &qr.Session{ID: id}
		app.mu.Unlock()
	}

	// 非终态：会话必须保留，轮询要继续
	put("sess-pending")
	app.finishQRPoll("sess-pending", "pending")
	if app.getQRSession("sess-pending") == nil {
		t.Fatalf("pending 会话被误删，轮询会断")
	}
	put("sess-scanned")
	app.finishQRPoll("sess-scanned", "scanned")
	if app.getQRSession("sess-scanned") == nil {
		t.Fatalf("scanned 会话被误删，等待确认的流程会断")
	}

	// 终态：三种都要回收
	for _, status := range []string{"expired", "cancelled", "unknown"} {
		id := "sess-" + status
		put(id)
		app.finishQRPoll(id, status)
		if app.getQRSession(id) != nil {
			t.Fatalf("%s 终态会话未被回收", status)
		}
	}
}

// TestCancelledQRSessionPollsAsCancelled 验证「用户主动取消」与「正常回收」在轮询结果上的区别。
//
// 前端 scan 流程、OpenAPI 的 status 枚举、qrStatusTerminal 都认 "cancelled"，
// 但在这个标记补上之前后端从来没产生过它：用户点「重新生成」后，迟到的 poll
// 只会拿到 404 "qr session not found"，看起来像二维码过期了，而实际上是他自己刷新的。
//
// 反面同样要守住：确认成功、过期清理走的是 dropQRSession，删的也是同一个表项，
// 但绝不能因此被报成「用户取消」，否则刚注册成功的轮询会被当成失败收尾。
func TestCancelledQRSessionPollsAsCancelled(t *testing.T) {
	// 走真实路由而不是直接调 handler：cancel 分支是本轮新加的，
	// 只测 handler 会漏掉「路由没挂上」这种接线问题。
	app, cookie := newAuthenticatedApp(t)

	put := func(id string) {
		app.mu.Lock()
		app.qrSessions[id] = &qr.Session{ID: id}
		app.mu.Unlock()
	}
	// 只对已被回收的会话发起 poll：会话还在的话会真的去打微信侧。
	call := func(method, id, action string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/qr/"+id+"/"+action, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	pollStatus := func(id string) (int, string) {
		rec := call(http.MethodGet, id, "poll")
		if rec.Code != http.StatusOK {
			return rec.Code, ""
		}
		// 统一响应封装：{code,msg,data:{session_id,status}}
		var env struct {
			Data struct {
				Status string `json:"status"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("poll 响应不是合法 JSON: %v (%s)", err, rec.Body.String())
		}
		return rec.Code, env.Data.Status
	}

	// 主动取消：会话立刻退役，迟到的 poll 拿到明确的 cancelled
	put("sess-cancel")
	if rec := call(http.MethodPost, "sess-cancel", "cancel"); rec.Code != http.StatusOK {
		t.Fatalf("cancel 返回 %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if app.getQRSession("sess-cancel") != nil {
		t.Fatalf("取消后会话仍在内存里，迟到的 confirm 会把上一张二维码补完")
	}
	if code, status := pollStatus("sess-cancel"); code != http.StatusOK || status != "cancelled" {
		t.Fatalf("取消后 poll = (%d, %q), want (200, cancelled)", code, status)
	}

	// 从没见过的会话不能被这个标记兜住，仍要报不存在
	if code, _ := pollStatus("sess-never-seen"); code != http.StatusNotFound {
		t.Fatalf("未知会话 poll = %d, want 404", code)
	}

	// 确认成功路径只回收，不产生 cancelled
	put("sess-confirmed")
	app.dropQRSession("sess-confirmed")
	if code, _ := pollStatus("sess-confirmed"); code != http.StatusNotFound {
		t.Fatalf("确认回收后的 poll = %d, want 404（不能被报成用户取消）", code)
	}

	// 标记只为迟到几百毫秒的 poll 服务，超出窗口照样按不存在处理
	put("sess-stale")
	call(http.MethodPost, "sess-stale", "cancel")
	app.mu.Lock()
	app.qrCancelled["sess-stale"] = time.Now().Add(-2 * time.Minute)
	app.mu.Unlock()
	if code, _ := pollStatus("sess-stale"); code != http.StatusNotFound {
		t.Fatalf("过期标记 poll = %d, want 404", code)
	}
}

// TestQRProxyRequested 判断请求体是否要求指定代理。
//
// 关键是「不能静默降级」：写了 mode 就必须走代理分支让后端校验，
// 哪怕值写错也要报错，而不是悄悄用服务器 IP 去登录。
func TestQRProxyRequested(t *testing.T) {
	profileID := int64(7)
	tests := []struct {
		name string
		body accountProxyIn
		want bool
	}{
		{"空请求体沿用默认路径", accountProxyIn{}, false},
		{"只带 product 不算指定代理", accountProxyIn{Product: "appstore"}, false},
		{"直连", accountProxyIn{Mode: "direct"}, true},
		{"静态代理", accountProxyIn{Mode: "static", StaticProxy: "1.2.3.4:8080"}, true},
		{"动态 API", accountProxyIn{Mode: "api", APIURL: "https://api.example.com/get"}, true},
		{"供应商配置", accountProxyIn{ProviderProfileID: &profileID}, true},
		{"写错的 mode 也要走校验", accountProxyIn{Mode: "socks5"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qrProxyRequested(tt.body); got != tt.want {
				t.Fatalf("qrProxyRequested(%+v) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

// TestQRRootRejectsBadProxyBeforeNetwork 验证代理参数有误时在建二维码之前就报错。
//
// 这些分支都在真正访问微信之前返回，所以测试不依赖外网：
// 参数错了必须立刻告诉调用方，而不是先生成一张用错出口的二维码。
func TestQRRootRejectsBadProxyBeforeNetwork(t *testing.T) {
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

	cases := []struct {
		name string
		body string
		want int
		part string
	}{
		{"产品不支持", `{"product":"wechat"}`, http.StatusUnprocessableEntity, "appstore"},
		{"模式非法", `{"mode":"socks5"}`, http.StatusBadRequest, "代理模式"},
		{"静态代理地址非法", `{"mode":"static","static_proxy":"not-a-proxy"}`, http.StatusBadRequest, "静态代理无效"},
		{"动态 API 非法", `{"mode":"api","api_url":"ftp://x"}`, http.StatusBadRequest, "代理 API"},
		{"供应商配置不存在", `{"mode":"api","provider_profile_id":999}`, http.StatusBadRequest, "代理配置不存在"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/qr", strings.NewReader(tc.body))
			app.handleQRRoot(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tc.want, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.part) {
				t.Fatalf("body = %s, want 含 %q", rec.Body.String(), tc.part)
			}
		})
	}
}

// TestTakeQRProxyChoiceIsOneShot 验证代理选择只被消费一次。
//
// 复用同一个 session_id 再确认一次（浏览器重试）时不能把代理配置再写一遍，
// 更不能把一个已经落库的账号重新当成新账号处理。
func TestTakeQRProxyChoiceIsOneShot(t *testing.T) {
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

	choice := qrProxyChoice{In: accountProxyIn{Mode: "static", StaticProxy: "1.2.3.4:8080"}}
	app.mu.Lock()
	app.qrProxies["sess-1"] = choice
	app.mu.Unlock()

	got, ok := app.takeQRProxyChoice("sess-1")
	if !ok || got.In.StaticProxy != "1.2.3.4:8080" {
		t.Fatalf("第一次取值失败: ok=%v got=%+v", ok, got)
	}
	if _, ok := app.takeQRProxyChoice("sess-1"); ok {
		t.Fatalf("代理选择应被消费掉，第二次仍取到值")
	}

	// 会话回收时也要连带清掉，避免内存里留下永远用不到的代理配置
	app.mu.Lock()
	app.qrProxies["sess-2"] = choice
	app.mu.Unlock()
	app.dropQRSession("sess-2")
	if _, ok := app.takeQRProxyChoice("sess-2"); ok {
		t.Fatalf("dropQRSession 未清理代理选择")
	}
}

// seedRuntimeState 给某个账号 ID 埋上代理租约、失败退避与连续拒绝计数。
func seedRuntimeState(app *App, id int64) {
	app.proxyMu.Lock()
	app.proxyLeases[id] = accountProxyLease{Value: "socks5://1.2.3.4:1080", ExpiresAt: time.Now().Add(time.Minute)}
	app.proxyMu.Unlock()

	app.keepAliveMu.Lock()
	app.keepAliveNext[id] = time.Now().Add(time.Hour)
	app.keepAliveFails[id] = 2
	app.credentialRejects[id] = 2
	app.keepAliveMu.Unlock()
}

// runtimeStateCount 统计该账号在内存里还剩几项状态。
func runtimeStateCount(app *App, id int64) int {
	n := 0
	app.proxyMu.Lock()
	if _, ok := app.proxyLeases[id]; ok {
		n++
	}
	app.proxyMu.Unlock()
	app.keepAliveMu.Lock()
	if _, ok := app.keepAliveNext[id]; ok {
		n++
	}
	if _, ok := app.keepAliveFails[id]; ok {
		n++
	}
	if _, ok := app.credentialRejects[id]; ok {
		n++
	}
	app.keepAliveMu.Unlock()
	return n
}

// TestDeleteAccountClearsRuntimeState 验证删除账号会连带清理内存状态。
//
// 这些状态以账号 ID 为键，而 /accounts/compact 会重排并复用 ID：
// 残留的代理租约会让新账号用上一个账号的出口，残留的退避会让它被跳过刷新，
// 残留的拒绝计数会让它下一次 -109 就被判死。
func TestDeleteAccountClearsRuntimeState(t *testing.T) {
	app, cookie := newAuthenticatedApp(t)

	nick := "待删账号"
	acc, err := app.db.UpsertAccount(context.Background(), "o_del_1", "lb",
		nil, &nick, nil, nil, map[string]any{"openid": "o_del_1"}, nil)
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}
	seedRuntimeState(app, acc.ID)
	if got := runtimeStateCount(app, acc.ID); got != 4 {
		t.Fatalf("前置状态埋点不完整，实际 %d 项", got)
	}

	req := httptest.NewRequest(http.MethodDelete, "/accounts?ref="+strconv.FormatInt(acc.ID, 10), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.handleAccountsRoot(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /accounts = %d %s", rec.Code, rec.Body.String())
	}
	if got := runtimeStateCount(app, acc.ID); got != 0 {
		t.Fatalf("删除账号后仍残留 %d 项内存状态", got)
	}
}

// TestCompactClearsRuntimeState 验证 ID 重排后清空所有账号级缓存。
func TestCompactClearsRuntimeState(t *testing.T) {
	app, cookie := newAuthenticatedApp(t)

	ctx := context.Background()
	nickA, nickB := "账号A", "账号B"
	first, err := app.db.UpsertAccount(ctx, "o_first", "lb", nil, &nickA, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("UpsertAccount(first) error = %v", err)
	}
	second, err := app.db.UpsertAccount(ctx, "o_second", "lb", nil, &nickB, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("UpsertAccount(second) error = %v", err)
	}
	// 制造 ID 空洞，让紧凑化真的发生重排
	if err := app.db.DeleteAccount(ctx, first.ID); err != nil {
		t.Fatalf("DeleteAccount() error = %v", err)
	}
	seedRuntimeState(app, second.ID)

	req := httptest.NewRequest(http.MethodPost, "/accounts/compact", strings.NewReader(`{"confirm":true}`))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.handleAccountCompact(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /accounts/compact = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"changed":1`) {
		t.Fatalf("紧凑化结果异常: %s", rec.Body.String())
	}
	if got := runtimeStateCount(app, second.ID); got != 0 {
		t.Fatalf("紧凑化后仍残留 %d 项内存状态（会套到新 ID 的账号身上）", got)
	}
}

// TestAccountsExposeLastRefreshError 验证 /accounts 会带上最近一次刷新失败原因。
//
// 用户看到「需重扫」却不知道是微信拒绝、代理故障还是超时，
// 原因以前只在容器日志里，排查只能靠登服务器翻日志。
func TestAccountsExposeLastRefreshError(t *testing.T) {
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

	nick := "诊断账号"
	acc, err := app.db.UpsertAccount(context.Background(), "o_diag", "lb",
		nil, &nick, nil, nil, map[string]any{"openid": "o_diag"}, nil)
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}

	// 没有失败过：不该出现该字段
	rec := httptest.NewRecorder()
	app.handleAccountsRoot(rec, httptest.NewRequest(http.MethodGet, "/accounts", nil))
	if strings.Contains(rec.Body.String(), "last_error") {
		t.Fatalf("未失败过的账号不应带 last_error: %s", rec.Body.String())
	}

	// 记录一次失败原因
	app.noteRefreshError(acc.ID, errors.New("refresh failed: code=42007 msg=refresh_token expired"))
	rec = httptest.NewRecorder()
	app.handleAccountsRoot(rec, httptest.NewRequest(http.MethodGet, "/accounts", nil))
	if !strings.Contains(rec.Body.String(), "42007") {
		t.Fatalf("应带出失败原因，实际: %s", rec.Body.String())
	}

	// 刷新成功后清除
	app.noteRefreshError(acc.ID, nil)
	if got := app.lastRefreshError(acc.ID); got != "" {
		t.Fatalf("成功后就应清空，仍有 %q", got)
	}
}
