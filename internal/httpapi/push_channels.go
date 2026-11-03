package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"yybwechat/internal/store"
)

// 额外的推送通道：Server酱（含 Server酱³）与 PushPlus。
//
// 它们不是「第二套通知系统」：去重窗口、状态跃迁判定、待补推队列都还是原来
// notifyIfStatusChanged / alertExpired 那一套，这里只是把**同一条告警**多发一份。
// 企业微信仍走原来的 Webhook 地址（支持 markdown 卡片），不在这里再开一个口子，
// 否则同一个群里会出现两种排版。
const pushChannelTimeout = 8 * time.Second

// pushTarget 是一个推送目标。Token 是机密：不进日志、不进事件、不进接口响应。
type pushTarget struct {
	Channel string
	Token   string
	Topic   string
}

// validExtraPushChannel 只接受本项目支持的两个额外通道。
// qywx 被有意排除：企业微信统一走 Webhook 地址，避免两处配置各发一份。
func validExtraPushChannel(channel string) bool {
	return channel == "serverchan" || channel == "pushplus"
}

// notifyChannelsConfigured 判断是否配置了任意一个推送出口。
//
// 各处「没配就别推」的门禁必须用它而不是只看 Webhook 地址，
// 否则只配了 Server酱 的部署会一条通知都发不出去。
func (a *App) notifyChannelsConfigured() bool {
	return a.getWebhookURL() != "" || a.cfg.NotifyChannel != ""
}

// pushExtraChannel 把一条告警发到配置的额外通道，返回是否送达。
// 未配置时返回 false 且不做任何事。
func (a *App) pushExtraChannel(title, content string) bool {
	if a.cfg.NotifyChannel == "" || a.cfg.NotifyToken == "" {
		return false
	}
	target := pushTarget{Channel: a.cfg.NotifyChannel, Token: a.cfg.NotifyToken, Topic: a.cfg.NotifyTopic}
	client := &http.Client{Timeout: pushChannelTimeout}
	ctx, cancel := context.WithTimeout(context.Background(), pushChannelTimeout)
	defer cancel()
	if err := postPush(ctx, client, target, title, content); err != nil {
		// 错误里可能带 URL（含 key），postPush 已经把 url.Error 剥掉了。
		log.Printf("[notify] %s 推送失败: %v", target.Channel, err)
		return false
	}
	return true
}

// pushAccountLabel 生成「#ID 昵称」形式的账号标签。
func pushAccountLabel(acc *store.WechatAccount) string {
	label := "#" + strconv.FormatInt(acc.ID, 10)
	if acc == nil {
		return label
	}
	for _, candidate := range []*string{acc.Remark, acc.Nickname} {
		if candidate != nil && strings.TrimSpace(*candidate) != "" {
			return label + " " + strings.TrimSpace(*candidate)
		}
	}
	return label
}

// pushChannelLabel 把渠道标识翻译成人看的名字。
func pushChannelLabel(acc *store.WechatAccount) string {
	if acc != nil && acc.Credentials["provider"] == "pcmgr" {
		return "电脑管家"
	}
	return "应用宝"
}

// pushStatusLabel 是状态取值的中文说法，与面板上的措辞保持一致。
func pushStatusLabel(status string) string {
	switch status {
	case "alive":
		return "可用"
	case "expired":
		return "需重扫"
	case store.StatusSessionOnly:
		return "需补授权（仅剩缓存会话）"
	case "unknown":
		return "待确认"
	}
	return status
}

// statusChangeMessage 是状态跃迁的推送正文。
//
// 只写 ID、昵称、渠道与状态：Server酱 / PushPlus 属于第三方通道，
// OpenID 这类账号标识没有必要带过去。
func statusChangeMessage(acc *store.WechatAccount, oldStatus, newStatus string) (string, string) {
	title := "账号状态变更：" + pushStatusLabel(newStatus)
	lines := []string{
		"账号：" + pushAccountLabel(acc),
		"渠道：" + pushChannelLabel(acc),
		"状态：" + pushStatusLabel(oldStatus) + " → " + pushStatusLabel(newStatus),
		"时间：" + time.Now().Format("2006-01-02 15:04:05"),
	}
	return title, strings.Join(lines, "\n")
}

// renewalFailureMessage 是「凭据过期且续不上」的推送正文。
func renewalFailureMessage(acc *store.WechatAccount, status, reason string) (string, string) {
	title := "凭据续期失败：" + pushStatusLabel(status)
	lines := []string{
		"账号：" + pushAccountLabel(acc),
		"渠道：" + pushChannelLabel(acc),
		"状态：" + pushStatusLabel(status),
		"时间：" + time.Now().Format("2006-01-02 15:04:05"),
	}
	if strings.TrimSpace(reason) != "" {
		lines = append(lines, "原因："+strings.TrimSpace(reason))
	}
	return title, strings.Join(lines, "\n")
}

// expiredMessage 是「账号已失效、需要重新扫码」的推送正文。
func expiredMessage(acc *store.WechatAccount) (string, string) {
	title := "账号需要重新扫码"
	lines := []string{
		"账号：" + pushAccountLabel(acc),
		"渠道：" + pushChannelLabel(acc),
		"状态：" + pushStatusLabel("expired"),
		"时间：" + time.Now().Format("2006-01-02 15:04:05"),
		"请打开控制台重新扫码。",
	}
	return title, strings.Join(lines, "\n")
}

// Server酱³ 的 SendKey 内嵌了用户自己的推送域名编号。
var serverChanV3Key = regexp.MustCompile(`^sctp(\d+)t`)

func newPushRequest(ctx context.Context, target pushTarget, title, content string) (*http.Request, error) {
	switch target.Channel {
	case "serverchan":
		endpoint := "https://sctapi.ftqq.com/" + url.PathEscape(target.Token) + ".send"
		if m := serverChanV3Key.FindStringSubmatch(target.Token); m != nil {
			endpoint = "https://" + m[1] + ".push.ft07.com/send/" + url.PathEscape(target.Token) + ".send"
		}
		// desp 按 Markdown 渲染，单个换行会被合并，所以要写成两个。
		form := url.Values{"title": {title}, "desp": {strings.ReplaceAll(content, "\n", "\n\n")}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		return req, err
	case "pushplus":
		body := map[string]any{"token": target.Token, "title": title, "content": content, "template": "txt"}
		if target.Topic != "" {
			body["topic"] = target.Topic
		}
		return jsonPushRequest(ctx, "https://www.pushplus.plus/send", body)
	}
	return nil, errors.New("unsupported notify channel")
}

func jsonPushRequest(ctx context.Context, endpoint string, body map[string]any) (*http.Request, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, err
}

type pushResponse struct {
	Code    *int `json:"code"`
	Errno   *int `json:"errno"`
	ErrCode *int `json:"errcode"`
}

// pushAccepted 判断通道是否真的受理了这条推送。
//
// 只看 HTTP 200 不够：这几个通道在参数或额度出问题时同样回 200，
// 结论写在响应体里，所以要按各自约定读 code / errno。
func pushAccepted(channel string, r pushResponse) bool {
	switch channel {
	case "serverchan":
		return r.Code != nil && *r.Code == 0 || r.Errno != nil && *r.Errno == 0
	case "pushplus":
		return r.Code != nil && *r.Code == 200
	}
	return false
}

func postPush(ctx context.Context, client *http.Client, target pushTarget, title, content string) error {
	req, err := newPushRequest(ctx, target, title, content)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		// url.Error 会带上完整请求 URL，而 URL 里就是 Server酱 的 key。
		// 只保留传输层原因，别把密钥写进日志。
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("send: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var parsed pushResponse
	if json.Unmarshal(raw, &parsed) != nil || !pushAccepted(target.Channel, parsed) {
		// 响应体不回声：它可能把请求字段（含 key）重复一遍。
		return errors.New("channel rejected the notice")
	}
	return nil
}
