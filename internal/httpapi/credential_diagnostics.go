package httpapi

import (
	"log"
	"net/http"
	"regexp"
	"strconv"

	"yybwechat/internal/pcmgr"
	"yybwechat/internal/protocol"
)

// upstreamDiagnostic 是「上游到底在哪一层、用什么码拒绝了我们」的结构化描述。
//
// 只从**已解析的业务错误类型**里取信息，绝不复制原始错误字符串：那些字符串
// 可能带着 access token、openid 或代理认证信息，一旦随 409 返回给调用方、
// 进了脚本日志，凭据就撒出去了。
type upstreamDiagnostic struct {
	Stage        string `json:"stage"`
	UpstreamCode int64  `json:"upstream_code"`
	WeChatCode   int    `json:"wechat_code,omitempty"`
}

// diagnosticWeChatCode 从上游 msg 里捞出微信自己的业务码。
// 上游常见写法是 "error 42007 | 42007" 或 "code: 40030"。
var diagnosticWeChatCode = regexp.MustCompile(`(?i)(?:^|(?:\berror\b|\bcode\b)\s*[:=]?\s*|\[)(40030|42002|42007|40188)(?:$|[\s\]|,;])`)

// credentialDiagnostics 遍历整条错误链，把每一层的拒绝整理成结构化条目。
//
// 为什么要遍历：refresh 与 check-login 是两段独立拒绝，只看最外层会丢掉
// 「微信到底说了什么」——而这正是判断「该重扫还是该重试」的唯一依据。
func credentialDiagnostics(err error) []upstreamDiagnostic {
	out := make([]upstreamDiagnostic, 0)
	seen := make(map[upstreamDiagnostic]bool)
	var visit func(error)
	visit = func(err error) {
		if err == nil {
			return
		}
		var item upstreamDiagnostic
		var message string
		switch e := err.(type) {
		case *pcmgr.BufferRejectedError:
			// 电脑管家取票据（20138）在业务层就被拒。
			item.Stage, item.UpstreamCode = "pcmgr_login_buffer", int64(e.Code)
		case *pcmgr.RPCError:
			// 按命令号分层：20138 是取票据，20042 是主 key 续期。
			// 两者失败含义完全不同，混成一条会让诊断只看到外层 -14。
			switch e.Command {
			case 20138:
				item.Stage, item.UpstreamCode = "pcmgr_login_buffer", int64(e.Code)
			case 20042:
				item.Stage, item.UpstreamCode = "pcmgr_key_renewal", int64(e.Code)
			}
		case *protocol.AutoAuthRejectedError:
			// 独立原生恢复有三层：传输、应用、业务，分别留事件。
			switch e.Stage {
			case "transport", "application", "business":
				item.Stage, item.UpstreamCode = "native_autoauth_"+e.Stage, e.Code
			}
		case *protocol.RefreshRejectedError:
			item.Stage = "refresh_token"
			if e.Operation == "check_login" {
				item.Stage = "check_login"
			}
			item.UpstreamCode, message = int64(e.Code), e.Message
		case *protocol.LoginBufferRejectedError:
			item.Stage, item.UpstreamCode, message = "login_buffer", int64(e.Code), e.Message
		case *protocol.ManualAuthRejectedError:
			// 票据在握手阶段就被拒：此时具体 API 还没发出去，
			// 说明问题在票据本身，而不是目标小程序或账号权限。
			item.Stage, item.UpstreamCode = "manual_auth", e.Code
		case *protocol.ShortlinkRejectedError:
			item.Stage, item.UpstreamCode = "shortlink_application", e.Code
			if e.Transport {
				item.Stage = "shortlink_transport"
			}
		}
		if item.Stage != "" {
			if match := diagnosticWeChatCode.FindStringSubmatch(message); len(match) > 1 {
				item.WeChatCode, _ = strconv.Atoi(match[1])
			}
			if !seen[item] {
				seen[item] = true
				out = append(out, item)
			}
		}
		switch e := err.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range e.Unwrap() {
				visit(child)
			}
		case interface{ Unwrap() error }:
			visit(e.Unwrap())
		}
	}
	visit(err)
	return out
}

// writeAccountExpired 统一输出 409：既告诉调用方要重扫，也带上可排查的结构化原因。
func (a *App) writeAccountExpired(w http.ResponseWriter, err accountExpiredError) {
	diagnostics := credentialDiagnostics(err.cause)
	if len(diagnostics) > 0 {
		log.Printf("[protocol] 账号 id=%d 凭据恢复被拒: %+v", err.accountID, diagnostics)
	}
	writeRawJSON(w, http.StatusConflict, apiEnvelope{
		Code: http.StatusConflict,
		Msg:  "account login_buffer expired (refresh failed); re-scan required",
		Data: map[string]any{"rescan_required": true, "upstream_errors": diagnostics},
	})
}

// writeProtocolCallError 输出协议调用失败。
//
// 与 writeError 的差别在于：只要能从错误链里解析出结构化拒绝，就带上
// upstream_errors 与 request_rejected，让调用方知道「上游在哪一层、用什么码拒绝」，
// 而不是只看到一个笼统的 502。原始错误文本仍不复制（可能含凭据）。
func (a *App) writeProtocolCallError(w http.ResponseWriter, err error) {
	diagnostics := credentialDiagnostics(err)
	if len(diagnostics) == 0 {
		writeError(w, http.StatusBadGateway, "call failed: "+err.Error())
		return
	}
	writeRawJSON(w, http.StatusBadGateway, apiEnvelope{
		Code: http.StatusBadGateway,
		Msg:  "call failed: " + err.Error(),
		Data: map[string]any{"upstream_errors": diagnostics, "request_rejected": protocol.IsApplicationRequestRejection(err)},
	})
}
