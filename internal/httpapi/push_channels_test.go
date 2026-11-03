package httpapi

import (
	"context"
	"strings"
	"testing"

	"yybwechat/internal/protocol"
	"yybwechat/internal/store"
)

// 续期入口的边界：只改应用宝/PC YYB 凭据的刷新业务号，
// 不能把电脑管家或其它来源也拖进 legacy 路径。
func TestLegacyRefreshPolicyKeepsProviderAndOrigin(t *testing.T) {
	for _, provider := range []string{"", "appstore", "pcmgr", "unsupported"} {
		for _, source := range []string{"", "pc_yyb", "pc_yyb_auth", "unsupported"} {
			t.Run(provider+"/"+source, func(t *testing.T) {
				app := &App{cfg: Config{ASRefreshMode: ASRefreshLegacy}}
				input := protocol.LoginBufferCredentials{Provider: provider, RefreshBusinessID: source, RefreshTokenObservedAt: 123}
				want := source
				if (provider == "" || provider == "appstore") && source != "unsupported" {
					want = "pc_yyb_auth"
				}
				calls := 0
				app.refreshCredentials = func(_ context.Context, used protocol.LoginBufferCredentials) (protocol.LoginBufferCredentials, error) {
					calls++
					if used.Provider != provider || used.RefreshBusinessID != want {
						t.Fatal("刷新入口越过了渠道 / 来源边界")
					}
					used.AccessToken = "rotated-access"
					return used, nil
				}
				fresh, err := app.refreshCredentialsWithProxy(context.Background(), input, "", false)
				if err != nil || calls != 1 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				// 刷新成功后必须把来源标记还原：调用方据此决定后续走哪条分支。
				if fresh.RefreshBusinessID != source || fresh.Provider != provider ||
					fresh.RefreshTokenObservedAt != 123 || fresh.AccessToken != "rotated-access" {
					t.Fatal("刷新结果丢了轮换材料或改了凭据来源")
				}
			})
		}
	}
}

// legacy 入口把「扫码授权已被拒」包成含糊的 -109，必须补一次 check-login
// 才能得到明确结论；其余失败一律不许去探别的入口。
func TestLegacyRefreshOnlyProbesCheckLoginOnParamsInvalid(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		failure    error
		wantChecks int
	}{
		{"网络错误不补校验", "pc_yyb", context.DeadlineExceeded, 0},
		{"其它拒绝不补校验", "pc_yyb", &protocol.RefreshRejectedError{Code: -1, Message: "busy"}, 0},
		{"扫码凭据的 -109 补一次", "pc_yyb", &protocol.RefreshRejectedError{Code: -109, Message: "RC_PARAMS_INVALID"}, 1},
		{"未标记来源的 -109 也补一次", "", &protocol.RefreshRejectedError{Code: -109, Message: "RC_PARAMS_INVALID"}, 1},
		{"_auth 凭据自身不补", "pc_yyb_auth", &protocol.RefreshRejectedError{Code: -109, Message: "RC_PARAMS_INVALID"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &App{cfg: Config{ASRefreshMode: ASRefreshLegacy}}
			checks := 0
			app.refreshCredentials = func(_ context.Context, creds protocol.LoginBufferCredentials) (protocol.LoginBufferCredentials, error) {
				if creds.RefreshBusinessID != "pc_yyb_auth" {
					t.Fatal("没有选到 legacy 入口")
				}
				return protocol.LoginBufferCredentials{}, tc.failure
			}
			app.checkLogin = func(_ context.Context, creds protocol.LoginBufferCredentials) (protocol.LoginBufferCredentials, error) {
				checks++
				// 补校验用的是同一份凭据（同一个出口、同一个授权），不改任何字段。
				return creds, nil
			}
			_, _ = app.refreshCredentialsWithProxy(context.Background(), protocol.LoginBufferCredentials{RefreshBusinessID: tc.source}, "", false)
			if checks != tc.wantChecks {
				t.Fatalf("check-login 调用 %d 次, want %d", checks, tc.wantChecks)
			}
		})
	}
}

// source 模式下不碰 legacy 入口，行为与升级前一致。
func TestSourceRefreshModeBypassesLegacyPolicy(t *testing.T) {
	app := &App{cfg: Config{ASRefreshMode: ASRefreshSource}}
	app.checkLogin = func(context.Context, protocol.LoginBufferCredentials) (protocol.LoginBufferCredentials, error) {
		t.Fatal("source 模式不该触发 check-login 补校验")
		return protocol.LoginBufferCredentials{}, nil
	}
	app.refreshCredentials = func(_ context.Context, creds protocol.LoginBufferCredentials) (protocol.LoginBufferCredentials, error) {
		if creds.RefreshBusinessID == "pc_yyb_auth" {
			t.Fatal("source 模式被改写成了 legacy 入口")
		}
		return creds, nil
	}
	if _, err := app.refreshCredentialsWithProxy(context.Background(), protocol.LoginBufferCredentials{RefreshBusinessID: "pc_yyb"}, "", false); err != nil {
		t.Fatal(err)
	}
}

// 通道配置：qywx 被有意排除（企业微信统一走 Webhook），拼错必须拒绝启动。
func TestNotifyChannelValidation(t *testing.T) {
	cases := []struct {
		name    string
		channel string
		token   string
		wantErr bool
	}{
		{"留空可用", "", "", false},
		{"none 视作关闭", "none", "", false},
		{"serverchan 需要令牌", "serverchan", "", true},
		{"pushplus 可用", "pushplus", "tok", false},
		{"qywx 引导用 Webhook", "qywx", "tok", true},
		{"拼错拒绝", "telegram", "tok", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, err := NewApp(Config{
				ResourceRoot:   t.TempDir(),
				RequestTimeout: 0,
				NotifyChannel:  tc.channel,
				NotifyToken:    tc.token,
			})
			if app != nil {
				// 必须关掉数据库连接，否则 TempDir 清理时文件还被占用。
				t.Cleanup(func() { app.Close() })
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// 推送请求要打到正确的地址，且令牌只出现在 URL / 请求体里。
func TestPushRequestTargets(t *testing.T) {
	ctx := context.Background()

	req, err := newPushRequest(ctx, pushTarget{Channel: "serverchan", Token: "SCT123abc"}, "标题", "第一行\n第二行")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.URL.String(), "sctapi.ftqq.com/SCT123abc.send") {
		t.Fatalf("unexpected serverchan endpoint: %s", req.URL)
	}

	// Server酱³ 的 SendKey 自带推送域名编号。
	req, err = newPushRequest(ctx, pushTarget{Channel: "serverchan", Token: "sctp88tAbCdEf"}, "标题", "正文")
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Host != "88.push.ft07.com" {
		t.Fatalf("Server酱³ endpoint = %s, want 88.push.ft07.com", req.URL.Host)
	}

	req, err = newPushRequest(ctx, pushTarget{Channel: "pushplus", Token: "tok", Topic: "team"}, "标题", "正文")
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.String() != "https://www.pushplus.plus/send" {
		t.Fatalf("unexpected pushplus endpoint: %s", req.URL)
	}

	if _, err := newPushRequest(ctx, pushTarget{Channel: "qywx", Token: "k"}, "t", "c"); err == nil {
		t.Fatal("qywx 不该被额外通道接受")
	}
}

// 各通道对「受理」的判定不同，HTTP 200 不等于成功。
func TestPushAcceptedPerChannel(t *testing.T) {
	zero, two, ok200 := 0, 2, 200
	if !pushAccepted("serverchan", pushResponse{Code: &zero}) {
		t.Fatal("serverchan code=0 应视为受理")
	}
	if !pushAccepted("serverchan", pushResponse{Errno: &zero}) {
		t.Fatal("serverchan errno=0 应视为受理")
	}
	if pushAccepted("serverchan", pushResponse{Code: &two}) {
		t.Fatal("serverchan code=2 不该视为受理")
	}
	if !pushAccepted("pushplus", pushResponse{Code: &ok200}) {
		t.Fatal("pushplus code=200 应视为受理")
	}
	if pushAccepted("pushplus", pushResponse{Code: &zero}) {
		t.Fatal("pushplus code=0 不该视为受理")
	}
}

// 告警正文只带 ID / 昵称 / 渠道 / 状态，不把 OpenID 送到第三方通道。
func TestPushMessagesOmitOpenID(t *testing.T) {
	nick := "测试号"
	acc := &store.WechatAccount{ID: 7, OpenID: "owNAX6nYDz83cktEDQxIoaAvAmk0", Nickname: &nick}
	for _, build := range []func() (string, string){
		func() (string, string) { return statusChangeMessage(acc, "alive", "expired") },
		func() (string, string) { return renewalFailureMessage(acc, "unknown", "续期被拒") },
		func() (string, string) { return expiredMessage(acc) },
	} {
		title, content := build()
		if title == "" || content == "" {
			t.Fatal("推送标题与正文都不能为空")
		}
		if strings.Contains(content, acc.OpenID) || strings.Contains(title, acc.OpenID) {
			t.Fatal("推送内容不该包含 OpenID")
		}
		if !strings.Contains(content, "#7") || !strings.Contains(content, nick) {
			t.Fatalf("推送内容缺少账号标识: %s", content)
		}
	}
}
