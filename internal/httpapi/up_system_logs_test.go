package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yybwechat/internal/eventlog"
)

// TestSystemLogsRequireAdminAndValidateFilter 验证运行日志接口的访问控制与参数校验。
//
// 与上游版本的差别：本项目是单管理员模型，没有普通用户/成员概念，
// 所以「普通用户 403」这一段不适用（没有这样的用户可造）。其余断言一一对应。
func TestSystemLogsRequireAdminAndValidateFilter(t *testing.T) {
	app, err := NewApp(Config{ResourceRoot: t.TempDir(), AdminUser: "owner", AdminPassword: "owner-password"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	handler := app.Handler()
	// 匿名访问：接口层给 401，页面层由浏览器会话中间件重定向到登录页。
	for _, path := range []string{"/api/system-logs", "/logs"} {
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, httptest.NewRequest("GET", path, nil))
		want := http.StatusUnauthorized
		if path == "/logs" {
			want = http.StatusSeeOther
		}
		if out.Code != want {
			t.Fatalf("anonymous %s status=%d", path, out.Code)
		}
	}
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"owner","password":"owner-password"}`)))
	if len(login.Result().Cookies()) == 0 {
		t.Fatalf("login failed: %d", login.Code)
	}
	request := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		for _, cookie := range login.Result().Cookies() {
			req.AddCookie(cookie)
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out
	}
	app.events.Emit("protocol.failed", eventlog.Fields{AccountID: 1, Channel: "pcmgr", Rejections: []eventlog.Rejection{{Stage: "pcmgr_login_buffer", Code: 1}}})
	// 日志是异步批量落库的，先关一次把队列刷空，否则可能查不到刚发的事件。
	app.events.Close()
	for _, path := range []string{"/api/system-logs?category=protocol&level=error&account_id=1", "/logs"} {
		out := request(path)
		if out.Code != 200 {
			t.Fatalf("admin %s status=%d", path, out.Code)
		}
	}
	// 每一类参数都必须走白名单/数值校验，不能把任意值直接当列值使用。
	for _, query := range []string{"level=secret", "category=secret", "channel=secret", "before=-1", "since=abc", "account_id=1%20OR%201=1", "limit=101"} {
		out := request("/api/system-logs?" + query)
		if out.Code != 400 {
			t.Fatalf("invalid query accepted: %s", query)
		}
	}
	out := request("/api/system-logs?category=protocol&limit=1")
	var body struct {
		Data struct {
			Entries []struct {
				AccountID int64 `json:"account_id"`
			} `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &body); err != nil || len(body.Data.Entries) != 1 || body.Data.Entries[0].AccountID != 1 {
		t.Fatal("missing diagnostic record", err)
	}
	if out.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("logs may be cached")
	}
}
