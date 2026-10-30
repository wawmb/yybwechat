package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mergeRequest 发一次合并请求，返回状态码与 data。
func mergeRequest(t *testing.T, app *App, cookie *http.Cookie, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/accounts/merge", strings.NewReader(body))
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

// TestAccountMergePreviewThenConfirm 覆盖合并的两步流程：
// 预览不动数据，确认才落库，且预览指纹不可伪造。
//
// 为什么值得单独测：预览如果顺手改了数据，运营者点一次「预览」就丢掉一个账号；
// 而指纹如果形同虚设，预览之后被保活刷新的凭据就会被旧信息覆盖掉。
func TestAccountMergePreviewThenConfirm(t *testing.T) {
	app, cookie := newAuthenticatedApp(t)
	ctx := context.Background()
	target, err := app.db.UpsertAccount(ctx, "openid-keep", "buffer-keep", nil, nil, nil, nil,
		map[string]any{"openid": "openid-keep", "provider": "appstore"}, nil)
	if err != nil {
		t.Fatalf("UpsertAccount(target) error = %v", err)
	}
	if _, err := app.db.SetAccountRemark(ctx, target.ID, "原账号"); err != nil {
		t.Fatalf("SetAccountRemark() error = %v", err)
	}
	source, err := app.db.UpsertAccount(ctx, "openid-extra", "buffer-extra", nil, nil, nil, nil,
		map[string]any{"openid": "openid-extra", "provider": "appstore"}, nil)
	if err != nil {
		t.Fatalf("UpsertAccount(source) error = %v", err)
	}

	// 1. 预览：不改任何数据
	code, data := mergeRequest(t, app, cookie, `{"source_ref":"openid-extra","target_ref":"openid-keep"}`)
	if code != http.StatusOK {
		t.Fatalf("预览应成功，got %d (%v)", code, data)
	}
	if data["mode"] != "preview" {
		t.Fatalf("应返回 preview 模式，got %v", data["mode"])
	}
	key, _ := data["preview_key"].(string)
	if key == "" {
		t.Fatal("预览必须返回 preview_key")
	}
	if _, err := app.db.GetAccount(ctx, source.ID); err != nil {
		t.Fatalf("预览不应删除来源账号: %v", err)
	}

	// 2. 指纹错误：拒绝
	code, _ = mergeRequest(t, app, cookie,
		`{"source_ref":"openid-extra","target_ref":"openid-keep","confirm":true,"preview_key":"deadbeef"}`)
	if code != http.StatusConflict {
		t.Fatalf("错误指纹应返回 409，got %d", code)
	}
	if _, err := app.db.GetAccount(ctx, source.ID); err != nil {
		t.Fatalf("指纹不匹配时不应删除来源账号: %v", err)
	}

	// 3. 正确指纹：执行合并
	body := `{"source_ref":"openid-extra","target_ref":"openid-keep","confirm":true,"preview_key":"` + key + `"}`
	code, data = mergeRequest(t, app, cookie, body)
	if code != http.StatusOK {
		t.Fatalf("确认合并应成功，got %d (%v)", code, data)
	}
	if data["mode"] != "merged" {
		t.Fatalf("应返回 merged，got %v", data["mode"])
	}
	if removed, _ := data["removed_id"].(float64); int64(removed) != source.ID {
		t.Fatalf("removed_id 应为来源 ID %d，got %v", source.ID, data["removed_id"])
	}
	kept, err := app.db.GetAccount(ctx, target.ID)
	if err != nil {
		t.Fatalf("目标账号应仍存在: %v", err)
	}
	if kept.OpenID != "openid-extra" || kept.LoginBuffer != "buffer-extra" {
		t.Fatalf("目标应采用来源授权，got openid=%q", kept.OpenID)
	}
	if kept.Remark == nil || *kept.Remark != "原账号" {
		t.Fatalf("目标备注必须保留，got %v", kept.Remark)
	}
	if _, err := app.db.GetAccount(ctx, source.ID); err == nil {
		t.Fatal("来源账号必须已删除")
	}
}

// TestAccountMergeRejectsGuards 覆盖合并的几个前置拒绝条件。
func TestAccountMergeRejectsGuards(t *testing.T) {
	app, cookie := newAuthenticatedApp(t)
	ctx := context.Background()
	if _, err := app.db.UpsertAccount(ctx, "openid-a", "buffer-a", nil, nil, nil, nil,
		map[string]any{"openid": "openid-a"}, nil); err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}
	expired := "expired"
	if _, err := app.db.UpsertAccount(ctx, "openid-b", "buffer-b", nil, nil, nil, nil,
		map[string]any{"openid": "openid-b"}, &expired); err != nil {
		t.Fatalf("UpsertAccount() error = %v", err)
	}

	// 同一账号
	if code, _ := mergeRequest(t, app, cookie, `{"source_ref":"openid-a","target_ref":"openid-a"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("同一账号应返回 422，got %d", code)
	}
	// 来源已失效
	if code, _ := mergeRequest(t, app, cookie, `{"source_ref":"openid-b","target_ref":"openid-a"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("来源已失效应返回 422，got %d", code)
	}
	// 账号不存在
	if code, _ := mergeRequest(t, app, cookie, `{"source_ref":"openid-missing","target_ref":"openid-a"}`); code != http.StatusNotFound {
		t.Fatalf("账号不存在应返回 404，got %d", code)
	}
	// 未登录
	req := httptest.NewRequest(http.MethodPost, "/accounts/merge", strings.NewReader(`{"source_ref":"openid-a","target_ref":"openid-b"}`))
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应返回 401，got %d", rec.Code)
	}
}
