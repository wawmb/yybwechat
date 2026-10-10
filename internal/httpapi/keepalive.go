package httpapi

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"yybwechat/internal/protocol"
	"yybwechat/internal/store"
)

// 凭据保活相关常量。
const (
	keepAliveWorkers  = 4               // 并发刷新账号的 worker 数量
	keepAliveBackoff  = 5 * time.Minute // 首次刷新失败后的退避时间
	keepAliveMaxDelay = 1 * time.Hour   // 连续失败时退避时间的上限
)

// runKeepAlive 是后台保活 goroutine，按固定间隔扫描账号并在凭据到期前提前续期。
//
// 与 runHealthChecker 的分工：
//   - runHealthChecker 依据「距上次检查的时长」兜底扫描，保证账号状态不会长期不更新；
//   - runKeepAlive 依据「凭据剩余有效期」精确续期，在真正过期之前完成刷新。
//
// 两者可以同时启用，重复的刷新会被凭据有效期判断自然过滤掉。
func (a *App) runKeepAlive() {
	ticker := time.NewTicker(a.cfg.KeepAliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.stopKeep:
			log.Println("[keepalive] 凭据保活已停止")
			return
		case <-ticker.C:
			a.refreshDueAccounts()
		}
	}
}

// refreshDueAccounts 扫描所有账号，并发刷新凭据即将到期或已过期的账号。
// 跳过已标记失效、无凭据、未到期以及处于失败退避窗口内的账号。
func (a *App) refreshDueAccounts() {
	accounts, err := a.db.ListAccounts(context.Background())
	if err != nil {
		log.Printf("[keepalive] 获取账号列表失败: %v", err)
		return
	}
	if len(accounts) == 0 {
		return
	}

	sem := make(chan struct{}, keepAliveWorkers)
	var wg sync.WaitGroup
	for _, acc := range accounts {
		if a.keepAliveShouldSkip(acc) {
			continue
		}
		select {
		case <-a.stopKeep:
			return
		default:
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(acc *store.WechatAccount) {
			defer wg.Done()
			defer func() { <-sem }()
			a.keepAliveRefresh(acc)
		}(acc)
	}
	wg.Wait()
}

// keepAliveShouldSkip 判断该账号本轮是否无需刷新。
func (a *App) keepAliveShouldSkip(acc *store.WechatAccount) bool {
	// 已判定失效的账号不再自动刷新，需要用户重新扫码
	if deref(acc.Status) == "expired" {
		return true
	}
	if acc.Credentials == nil {
		return true
	}
	// 处于失败退避窗口内，跳过以避免对微信侧造成压力
	a.keepAliveMu.Lock()
	next, ok := a.keepAliveNext[acc.ID]
	a.keepAliveMu.Unlock()
	if ok && time.Now().Before(next) {
		return true
	}
	// 凭据仍有充足有效期，无需续期。
	// credentialExpiresIn 在无法判断剩余有效期时返回 0，此时按「需要刷新」处理。
	if credentialExpiresIn(acc) > int64(a.cfg.KeepAliveAhead/time.Second) {
		return true
	}
	return false
}

// keepAliveRefresh 刷新单个账号的凭据，并按结果更新账号状态与重试退避。
func (a *App) keepAliveRefresh(acc *store.WechatAccount) {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.RequestTimeout+20*time.Second)
	defer cancel()

	oldStatus := deref(acc.Status)
	status, _, refreshErr := a.refreshLiveness(ctx, acc)

	switch status {
	case "alive":
		// 刷新成功，清除该账号的退避记录
		a.clearKeepAliveRetry(acc.ID)
		if shouldNotifyStatusChange(oldStatus, status) {
			log.Printf("[keepalive] 账号 %s (%s) 已恢复: %s → %s",
				deref(acc.Nickname), acc.OpenID, oldStatus, status)
			a.notifyWebhook(acc, oldStatus, status)
		}
	default:
		a.markKeepAliveRetry(acc.ID)
		if definitiveCredentialFailure(refreshErr) {
			// 凭据彻底失效：状态已置为 expired，后续保活会跳过该账号直到重新扫码
			log.Printf("[keepalive] 账号 %s (%s) 凭据已彻底失效，需重新扫码: %v",
				deref(acc.Nickname), acc.OpenID, refreshErr)
		} else {
			// 瞬时错误（网络抖动、超时）：状态为 unknown，按递增退避等待重试
			log.Printf("[keepalive] 账号 %s (%s) 刷新失败，将退避重试: %v",
				deref(acc.Nickname), acc.OpenID, refreshErr)
		}
		if shouldNotifyStatusChange(oldStatus, status) {
			log.Printf("[keepalive] 账号 %s (%s) 状态变更: %s → %s",
				deref(acc.Nickname), acc.OpenID, oldStatus, status)
			a.notifyWebhook(acc, oldStatus, status)
		}
	}
}

// markKeepAliveRetry 记录账号的下次可重试时间（失败退避）。
//
// 连续失败时退避按 2 倍递增（5min → 10min → 20min …）并封顶在 keepAliveMaxDelay，
// 避免长期失效的账号被高频重试而给微信侧造成压力。
func (a *App) markKeepAliveRetry(accountID int64) {
	a.keepAliveMu.Lock()
	defer a.keepAliveMu.Unlock()

	fails := a.keepAliveFails[accountID] + 1
	a.keepAliveFails[accountID] = fails

	delay := keepAliveBackoff
	for i := 1; i < fails; i++ {
		delay *= 2
		if delay >= keepAliveMaxDelay {
			delay = keepAliveMaxDelay
			break
		}
	}
	a.keepAliveNext[accountID] = time.Now().Add(delay)
}

// clearKeepAliveRetry 清除账号的失败退避记录（刷新成功后调用）。
func (a *App) clearKeepAliveRetry(accountID int64) {
	a.keepAliveMu.Lock()
	defer a.keepAliveMu.Unlock()
	delete(a.keepAliveNext, accountID)
	delete(a.keepAliveFails, accountID)
}

// invalidateAccountSessions 使指定账号的 MMTLS 会话失效。
// 凭据刷新后旧会话不再可用，必须清理，否则后续请求会一直使用过期会话。
func (a *App) invalidateAccountSessions(ctx context.Context, accountID int64) {
	_ = a.db.InvalidateSession(ctx, accountID, a.cfg.TCPProxy)
}

// definitiveCredentialFailure 判断错误信息是否表示「凭据已彻底失效」。
//
// 采用保守的启发式匹配：只有明确命中微信返回的拒绝码或失效描述时才认定为彻底失效，
// 网络抖动、超时等瞬时错误一律不在此列，避免误把正常账号标记为需要重新扫码。
func definitiveCredentialFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "42007") && strings.Contains(msg, "refresh_token"):
		return true
	case strings.Contains(msg, "40188") && strings.Contains(msg, "invalid scope"):
		return true
	case strings.Contains(msg, "重新登录"), strings.Contains(msg, "re-scan"), strings.Contains(msg, "rescan"):
		return true
	}
	// 同时出现「失效类词汇」与「凭据类词汇」才判定，降低误伤概率
	expireWords := []string{"过期", "失效", "无效", "expired", "invalid", "revoked"}
	credWords := []string{"token", "凭证", "凭据", "credential", "login_buffer", "loginbuffer"}
	hasExpire, hasCred := false, false
	for _, w := range expireWords {
		if strings.Contains(msg, w) {
			hasExpire = true
			break
		}
	}
	for _, w := range credWords {
		if strings.Contains(msg, w) {
			hasCred = true
			break
		}
	}
	return hasExpire && hasCred
}

// credentialExpiresIn 返回账号凭据的剩余有效秒数，无法判断时返回 0。
func credentialExpiresIn(acc *store.WechatAccount) int64 {
	if acc == nil || acc.Credentials == nil {
		return 0
	}
	creds := protocol.CredentialsFromMap(acc.Credentials)
	if creds.ExpiresAt <= 0 {
		return 0
	}
	return creds.ExpiresAt - time.Now().Unix()
}
