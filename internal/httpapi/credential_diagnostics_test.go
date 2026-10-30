package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yybwechat/internal/protocol"
)

// TestCredentialDiagnosticsWalksChainAndExtractsWeChatCode 覆盖 v0.2.40。
//
// 409 里要能看出「哪一层拒绝、上游码是多少、微信自己的码是多少」，
// 否则调用方只拿到一个 "re-scan required"，无从判断是令牌真失效还是我们请求有问题。
func TestCredentialDiagnosticsWalksChainAndExtractsWeChatCode(t *testing.T) {
	refresh := &protocol.RefreshRejectedError{Code: -109, Message: "RC_PARAMS_INVALID", Operation: "refresh_token"}
	checkLogin := &protocol.RefreshRejectedError{Code: -109, Message: "error 42007 | 42007", Operation: "check_login"}
	got := credentialDiagnostics(fmt.Errorf("%w; check login: %w", refresh, checkLogin))

	if len(got) != 2 {
		t.Fatalf("应展开 2 段诊断，实际 %d: %+v", len(got), got)
	}
	if got[0].Stage != "refresh_token" || got[0].UpstreamCode != -109 {
		t.Fatalf("第一段应为 refresh_token/-109，实际 %+v", got[0])
	}
	if got[1].Stage != "check_login" || got[1].UpstreamCode != -109 || got[1].WeChatCode != 42007 {
		t.Fatalf("第二段应为 check_login/-109/42007，实际 %+v", got[1])
	}
}

// TestCredentialDiagnosticsNamesShortlinkLayer 覆盖 v0.2.45 的分层。
func TestCredentialDiagnosticsNamesShortlinkLayer(t *testing.T) {
	transport := &protocol.ShortlinkRejectedError{Code: -14, Transport: true}
	application := &protocol.ShortlinkRejectedError{Code: -20014}
	got := credentialDiagnostics(errors.Join(transport, application))

	if len(got) != 2 || got[0].Stage != "shortlink_transport" || got[1].Stage != "shortlink_application" {
		t.Fatalf("短连接拒绝应区分传输/应用两层，实际 %+v", got)
	}
}

// TestCredentialDiagnosticsNamesManualAuthLayer 覆盖 0.2.49 新增的 manual_auth 阶段。
//
// 票据在 ManualAuth 阶段被拒时，具体 API 根本没发出去。如果不单独标出这一层，
// 诊断里只剩「refresh_token 被拒」，会被误读成账号凭据被撤销；
// 实际可能只是这张票不能用了，换一张票就能继续。
func TestCredentialDiagnosticsNamesManualAuthLayer(t *testing.T) {
	err := errors.Join(
		&protocol.ManualAuthRejectedError{Code: -10},
		&protocol.RefreshRejectedError{Code: 42007, Message: "error 42007 | 42007", Operation: "refresh_token"},
	)
	got := credentialDiagnostics(err)
	if len(got) != 2 {
		t.Fatalf("应得到 2 条诊断，实际 %+v", got)
	}
	if got[0].Stage != "manual_auth" || got[0].UpstreamCode != -10 {
		t.Fatalf("第一条应为 manual_auth/-10，实际 %+v", got[0])
	}
	if got[1].Stage != "refresh_token" || got[1].WeChatCode != 42007 {
		t.Fatalf("第二条应为 refresh_token/42007，实际 %+v", got[1])
	}
}

// TestAccountExpiredErrorUnwrapsCause 验证「账号失效」这层包装不会截断错误链。
//
// 缺 Unwrap 时 errors.As 只能拿到 accountExpiredError 本身，
// 上层看不到内部的 RefreshRejectedError，也就没法区分
// 「微信说令牌失效」和「我们请求构造有问题」——这正是 409 诊断要回答的问题。
func TestAccountExpiredErrorUnwrapsCause(t *testing.T) {
	inner := &protocol.RefreshRejectedError{Code: 42007, Message: "error 42007 | 42007", Operation: "refresh_token"}
	err := error(accountExpiredError{openid: "openid-x", accountID: 1, cause: inner})

	var expired accountExpiredError
	if !errors.As(err, &expired) {
		t.Fatal("应能识别出 accountExpiredError 本身")
	}
	var rejected *protocol.RefreshRejectedError
	if !errors.As(err, &rejected) || rejected.Code != 42007 {
		t.Fatalf("应能沿 Unwrap 走到内部拒绝码，got %v", err)
	}
}

// TestCredentialDiagnosticsIgnoresUnstructuredErrors 确认不会把原始错误串抄进响应。
//
// 传输错误里可能带代理认证、openid 甚至 token，原样回显等于把凭据撒给调用方。
func TestCredentialDiagnosticsIgnoresUnstructuredErrors(t *testing.T) {
	if got := credentialDiagnostics(errors.New("dial tcp: token=secret-openid-xxx")); len(got) != 0 {
		t.Fatalf("非结构化错误不应产生诊断，实际 %+v", got)
	}
}

// TestAccountExpiredResponseCarriesDiagnostics 验证 409 的响应结构。
func TestAccountExpiredResponseCarriesDiagnostics(t *testing.T) {
	app := &App{}
	checkLogin := &protocol.RefreshRejectedError{Code: -109, Message: "error 40030 invalid refresh_token", Operation: "check_login"}
	err := accountExpiredError{openid: "o_diag", accountID: 7, cause: checkLogin}

	rec := httptest.NewRecorder()
	app.writeAccountExpired(rec, err)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var body struct {
		Data struct {
			RescanRequired bool                 `json:"rescan_required"`
			UpstreamErrors []upstreamDiagnostic `json:"upstream_errors"`
		} `json:"data"`
	}
	if jsonErr := json.Unmarshal(rec.Body.Bytes(), &body); jsonErr != nil {
		t.Fatalf("响应不是合法 JSON: %v / %s", jsonErr, rec.Body.String())
	}
	if !body.Data.RescanRequired {
		t.Fatalf("应标记 rescan_required")
	}
	if len(body.Data.UpstreamErrors) != 1 || body.Data.UpstreamErrors[0].WeChatCode != 40030 {
		t.Fatalf("应带出 check_login/40030，实际 %+v", body.Data.UpstreamErrors)
	}

	// 没有原因的失效（例如库里状态就是 expired）不应伪造历史原因
	rec = httptest.NewRecorder()
	app.writeAccountExpired(rec, accountExpiredError{openid: "o_diag", accountID: 7})
	if body := rec.Body.String(); !strings.Contains(body, `"rescan_required":true`) || !strings.Contains(body, `"upstream_errors":[]`) {
		t.Fatalf("无原因时 upstream_errors 应为空数组，实际 %s", body)
	}
}
