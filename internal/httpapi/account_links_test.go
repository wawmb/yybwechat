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

// createLink 发请求创建一条链接，返回响应体（已解包 data）。
func createLink(t *testing.T, app *App, cookie *http.Cookie, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/account-links", strings.NewReader(body))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	var payload struct {
		Code int            `json:"code"`
		Msg  string         `json:"msg"`
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	return rec.Code, payload.Data
}

// TestAccountLinkRequiresConfirmationOnChannelSwitch 锁住跨渠道的安全边界：
// 换渠道等于换身份，必须先确认；同渠道不需要确认。
//
// 这条边界一旦放松，用户点一次「更新链接」就可能把一个应用宝账号
// 悄悄换成电脑管家账号——凭据体系不同，旧配置全部失效，而且没有任何提示。
func TestAccountLinkRequiresConfirmationOnChannelSwitch(t *testing.T) {
	app, cookie := newAuthenticatedApp(t)
	ctx := context.Background()
	acc, err := app.db.UpsertAccount(ctx, "openid-appstore", "buffer", nil, nil, nil, nil,
		map[string]any{"openid": "openid-appstore", "provider": "appstore"}, nil)
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}

	// 同渠道（不传 product，默认沿用原渠道）：成功且不需要确认
	code, data := createLink(t, app, cookie, `{"kind":"update","ref":"openid-appstore"}`)
	if code != http.StatusOK {
		t.Fatalf("同渠道创建链接应成功，got %d (%v)", code, data)
	}
	if data["product"] != "appstore" {
		t.Fatalf("默认应沿用原渠道 appstore，got %v", data["product"])
	}
	if url, _ := data["url"].(string); !strings.Contains(url, "/account-link/") {
		t.Fatalf("返回的链接格式不对: %v", data["url"])
	}
	// 同一条链接管理 ID 落库
	if _, err := app.db.GetAccountLinkByHash(ctx, accountLinkTokenHash(lastTokenSegment(t, data))); err != nil {
		t.Fatalf("链接未落库: %v", err)
	}

	// 清掉刚建的链接，测跨渠道
	if err := app.db.RevokeAccountLink(ctx, int64(data["id"].(float64)), nil); err != nil {
		t.Fatalf("RevokeAccountLink() error = %v", err)
	}

	// 跨渠道但未确认：必须 422，且不能创建
	code, data = createLink(t, app, cookie, `{"kind":"update","ref":"openid-appstore","product":"pcmgr"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("跨渠道未确认应返回 422，got %d (%v)", code, data)
	}
	links, err := app.db.ListAccountLinks(ctx, nil)
	if err != nil {
		t.Fatalf("ListAccountLinks() error = %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("未确认时不应创建任何链接，got %d 条", len(links))
	}

	// 确认后：成功，且标记为 switched
	code, data = createLink(t, app, cookie, `{"kind":"update","ref":"openid-appstore","product":"pcmgr","confirm_switch":true}`)
	if code != http.StatusOK {
		t.Fatalf("确认切换后应成功，got %d (%v)", code, data)
	}
	if data["product"] != "pcmgr" || data["switched"] != true {
		t.Fatalf("应记录为跨渠道切换，got product=%v switched=%v", data["product"], data["switched"])
	}
	if data["source_product"] != "appstore" {
		t.Fatalf("应保留原渠道信息，got %v", data["source_product"])
	}

	// 目标账号本身不能被创建链接这一步改动
	after, err := app.db.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if after.OpenID != "openid-appstore" || after.LoginBuffer != "buffer" {
		t.Fatal("生成链接不应修改账号本身")
	}
}

// lastTokenSegment 从返回的链接里取出 token。
func lastTokenSegment(t *testing.T, data map[string]any) string {
	t.Helper()
	url, _ := data["url"].(string)
	parts := strings.Split(strings.TrimSuffix(url, "/"), "/")
	if len(parts) == 0 {
		t.Fatalf("无法从链接中取出 token: %q", url)
	}
	return parts[len(parts)-1]
}

// TestAccountLinkRejectsSecondActiveLink 验证「同一账号只允许一条未消费链接」。
//
// 允许并存的话，两条链接哪条先被扫到就替换成哪个身份，
// 事后完全无法判断是哪次操作生效——这类问题排查成本极高。
func TestAccountLinkRejectsSecondActiveLink(t *testing.T) {
	app, cookie := newAuthenticatedApp(t)
	ctx := context.Background()
	if _, err := app.db.UpsertAccount(ctx, "openid-dup", "buffer", nil, nil, nil, nil,
		map[string]any{"openid": "openid-dup", "provider": "appstore"}, nil); err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}
	if code, data := createLink(t, app, cookie, `{"kind":"update","ref":"openid-dup"}`); code != http.StatusOK {
		t.Fatalf("首次创建应成功，got %d (%v)", code, data)
	}
	code, data := createLink(t, app, cookie, `{"kind":"update","ref":"openid-dup"}`)
	if code != http.StatusConflict {
		t.Fatalf("第二次创建应返回 409，got %d (%v)", code, data)
	}
	if data["code"] != "active_link_exists" {
		t.Fatalf("应返回 active_link_exists 标识，got %v", data["code"])
	}
	existing, _ := data["existing"].([]any)
	if len(existing) != 1 {
		t.Fatalf("应带回已有的 1 条链接，got %d", len(existing))
	}
}

// TestAccountLinkPageRejectsInvalidToken 验证公开页面的准入：
// 非法 token 返回 410，合法 token 才返回页面。
func TestAccountLinkPageRejectsInvalidToken(t *testing.T) {
	app, cookie := newAuthenticatedApp(t)
	ctx := context.Background()
	if _, err := app.db.UpsertAccount(ctx, "openid-page", "buffer", nil, nil, nil, nil,
		map[string]any{"openid": "openid-page", "provider": "appstore"}, nil); err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}
	_, data := createLink(t, app, cookie, `{"kind":"update","ref":"openid-page"}`)
	token := lastTokenSegment(t, data)

	// 非法 token
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/account-link/short", nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("非法 token 应返回 410，got %d", rec.Code)
	}

	// 合法 token：不需要登录也能拿到页面
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/account-link/"+token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("合法 token 应返回页面，got %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("应返回 HTML，got %q", ct)
	}

	// meta 不需要登录，且带回渠道与目标账号
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/account-link/"+token+"/meta", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("meta 应可匿名读取，got %d (%s)", rec.Code, rec.Body.String())
	}
	var meta struct {
		Data struct {
			Product       string `json:"product"`
			Kind          string `json:"kind"`
			SourceProduct string `json:"source_product"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &meta)
	if meta.Data.Product != "appstore" || meta.Data.Kind != "update" {
		t.Fatalf("meta 内容不对: %+v", meta.Data)
	}
}

// TestAccountLinkExpiredIsRejected 验证过期链接在页面上立即失效，
// 不依赖后台清理任务的执行时机。
func TestAccountLinkExpiredIsRejected(t *testing.T) {
	app, _ := newAuthenticatedApp(t)
	ctx := context.Background()
	acc, err := app.db.UpsertAccount(ctx, "openid-exp", "buffer", nil, nil, nil, nil,
		map[string]any{"openid": "openid-exp", "provider": "appstore"}, nil)
	if err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}
	raw, hash, err := newAccountLinkToken()
	if err != nil {
		t.Fatalf("newAccountLinkToken() error = %v", err)
	}
	if _, err := app.db.CreateAccountLinkForProduct(ctx, hash, aencryptForTest(t, app, raw), "update", acc.ID, nil,
		acc.OpenID, time.Now().Add(-time.Second).Unix(), "appstore", false); err != nil {
		t.Fatalf("CreateAccountLinkForProduct() error = %v", err)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/account-link/"+raw, nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("过期链接应返回 410，got %d", rec.Code)
	}
}

// aencryptForTest 用 App 自己的密钥加密 token，验证管理页能取回明文链接。
func aencryptForTest(t *testing.T, app *App, token string) string {
	t.Helper()
	sealed := app.encryptAccountLinkToken(context.Background(), token)
	if sealed == "" {
		t.Fatal("加密 token 失败")
	}
	plain, err := app.decryptAccountLinkToken(context.Background(), sealed)
	if err != nil || plain != token {
		t.Fatalf("加解密往返失败: %v", err)
	}
	return sealed
}

// TestAccountLinkTokenFormat 验证 token 格式约束（长度与字符集）在进库前就生效。
func TestAccountLinkTokenFormat(t *testing.T) {
	raw, hash, err := newAccountLinkToken()
	if err != nil {
		t.Fatalf("newAccountLinkToken() error = %v", err)
	}
	if !validAccountLinkToken(raw) {
		t.Fatalf("生成的 token 应通过校验: %q", raw)
	}
	if accountLinkTokenHash(raw) != hash {
		t.Fatal("token 哈希应可重复计算")
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("a", 31) + "/", strings.Repeat("a", 31) + "."} {
		if validAccountLinkToken(bad) {
			t.Fatalf("应拒绝非法 token: %q", bad)
		}
	}
}
