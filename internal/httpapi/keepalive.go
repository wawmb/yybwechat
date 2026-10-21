package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"yybwechat/internal/protocol"
	"yybwechat/internal/qr"
	"yybwechat/internal/store"
)

// 本文件实现凭据保活，并统一承担「刷新 → 判定 → 落库 → 通知」这条链路。
//
// 设计要点：
//   - 所有刷新都必须经过 refreshAccount，保证状态变更一定会走到 Webhook 通知；
//     历史版本里 /accounts/refresh 与 /wxapp/* 自行调刷新却不通知，导致账号
//     被判失效后后台 goroutine 跳过该账号，告警永久丢失。
//   - 失效判定基于结构化错误码，而不是对整段错误文本做关键词匹配。
//   - 同一账号的刷新串行执行，避免保活与健康检查并发覆盖彼此的结果。

// 凭据保活相关常量。
const (
	keepAliveWorkers  = 4               // 并发刷新账号的 worker 数量
	keepAliveBackoff  = 5 * time.Minute // 首次刷新失败后的退避时间
	keepAliveMaxDelay = 1 * time.Hour   // 连续失败时退避时间的上限

	// renewalAlertInterval 是「续期失败」告警的重提醒间隔。
	// 账号持续瘫痪时每 6 小时再提醒一次，既不刷屏，也不至于响过一声就沉默。
	renewalAlertInterval = 6 * time.Hour

	// expiredAlertInterval 是「账号已失效（expired）」告警的重提醒间隔。
	// expired 是终态，只要账号没被重新扫码，就按这个间隔一直提醒到恢复为止。
	expiredAlertInterval = 6 * time.Hour
)

// runKeepAlive 是后台保活 goroutine，按固定间隔扫描账号并在凭据到期前提前续期。
//
// 与 runHealthChecker 的分工：
//   - runHealthChecker 依据「距上次检查的时长」兜底扫描，保证账号状态不会长期不更新；
//   - runKeepAlive 依据「凭据剩余有效期」精确续期，在真正过期之前完成刷新。
//
// 两者可以同时启用，重复的刷新会被有效期判断自然过滤掉。
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
	// 补推上一轮未能送达的状态变更。必须放在扫描之前：
	// expired 是终态，本轮不会再被扫描到，错过这次就再没有补推机会。
	a.retryPendingNotifications(context.Background())

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
		if accountStatus(acc) == "expired" {
			// 终态账号不参与刷新，但仍然要持续提醒，直到运营者重新扫码
			a.alertExpired(acc)
			continue
		}
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

	// 顺手清理过期的 MMTLS 会话。
	//
	// sessions 表按「账号 + 代理」维度存一行，动态代理每换一次出口就会新增一行；
	// 之前只有健康检查会清理，而健康检查是可以被 --check-interval 0 关掉的，
	// 关掉之后这张表只增不减。保活默认 60 秒一轮，放这里就不会漏。
	if n, err := a.db.PurgeExpiredSessions(context.Background()); err != nil {
		log.Printf("[keepalive] 清理过期会话失败: %v", err)
	} else if n > 0 {
		log.Printf("[keepalive] 已清理 %d 条过期会话", n)
	}
}

// keepAliveShouldSkip 判断该账号本轮是否无需刷新。
func (a *App) keepAliveShouldSkip(acc *store.WechatAccount) bool {
	// 已判定失效的账号不再自动刷新，需要用户重新扫码
	if accountStatus(acc) == "expired" {
		return true
	}
	if acc.Credentials == nil {
		return true
	}
	ahead := a.cfg.KeepAliveAhead
	if setting, err := a.db.AccountProxySettingOrDefault(context.Background(), acc.ID); err == nil {
		// 动态代理每轮提取都会换出口，用它刷新凭据无法维持登录会话，
		// 只会白耗供应商额度并制造「续期失败」的假告警，因此不参与自动保活。
		if setting.Mode == "api" {
			return true
		}
		if setting.Mode != "direct" && setting.RefreshAheadSeconds > 0 {
			ahead = time.Duration(setting.RefreshAheadSeconds) * time.Second
		}
	}
	// 处于失败退避窗口内，跳过以避免对微信侧造成压力
	a.keepAliveMu.Lock()
	next, ok := a.keepAliveNext[acc.ID]
	a.keepAliveMu.Unlock()
	if ok && time.Now().Before(next) {
		return true
	}
	if !credentialsDueForRefresh(protocol.CredentialsFromMap(acc.Credentials), time.Now(), ahead) {
		return true
	}
	return false
}

// keepAliveRefresh 刷新单个账号的凭据，并按结果更新状态与重试退避。
// 状态变更通知由 refreshAccount 内部统一处理，这里只负责调度与日志。
func (a *App) keepAliveRefresh(acc *store.WechatAccount) {
	ctx, cancel := context.WithTimeout(context.Background(), keepAliveAccountTimeout(a.cfg.RequestTimeout))
	defer cancel()

	status, refreshed, err := a.refreshAccount(ctx, acc, false)
	switch {
	case err == nil && status == "alive":
		a.clearKeepAliveRetry(acc.ID)
		if refreshed {
			log.Printf("[keepalive] 账号 %s (%s) 凭据已续期",
				deref(acc.Nickname), acc.OpenID)
		}
	case status == "expired":
		// 终态：清掉退避记录，后续由 expired 判定直接跳过
		a.clearKeepAliveRetry(acc.ID)
		log.Printf("[keepalive] 账号 %s (%s) 凭据已彻底失效，需重新扫码: %v",
			deref(acc.Nickname), acc.OpenID, err)
	default:
		a.markKeepAliveRetry(acc.ID)
		log.Printf("[keepalive] 账号 %s (%s) 刷新失败，将退避重试: %v",
			deref(acc.Nickname), acc.OpenID, err)
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

// invalidateAccountSessions 使指定账号在所有代理下的 MMTLS 会话失效。
// 凭据刷新后旧会话不再可用，必须清理，否则后续请求会一直使用过期会话。
func (a *App) invalidateAccountSessions(ctx context.Context, accountID int64) {
	_ = a.db.InvalidateAccountSessions(ctx, accountID)
}

// refreshLiveness 刷新单个账号的存活状态，并返回刷新后的 login buffer。
//
// 这是给控制台与协议接口使用的入口，内部复用 refreshAccount，
// 因此状态变更同样会触发 Webhook 通知。
func (a *App) refreshLiveness(ctx context.Context, acc *store.WechatAccount) (status string, newLoginBuffer string, err error) {
	status, _, err = a.refreshAccount(ctx, acc, true)
	if status != "alive" {
		return status, "", err
	}
	// 手动刷新时顺带同步头像，保活路径不做，避免每轮都产生一次下载
	if avatar := a.resolveAvatar(ctx, acc.OpenID, acc.UserInfo); avatar != "" {
		_ = a.db.SetAccountProfile(ctx, acc.ID, acc.Nickname, &avatar, acc.UserInfo)
	}
	// refreshAccount 已把新凭据写库，这里回读以拿到新的 login buffer
	if latest, getErr := a.db.GetAccount(ctx, acc.ID); getErr == nil {
		newLoginBuffer = latest.LoginBuffer
	}
	return status, newLoginBuffer, err
}

// refreshAccount 是唯一的凭据刷新实现，返回状态、是否真的发生了续期，以及错误。
//
// force 为真表示跳过有效期判断强制刷新（控制台手动刷新、协议调用失败后重试）。
func (a *App) refreshAccount(ctx context.Context, acc *store.WechatAccount, force bool) (string, bool, error) {
	lock := a.refreshLockFor(acc.ID)
	select {
	case lock <- struct{}{}:
	case <-ctx.Done():
		return accountStatus(acc), false, ctx.Err()
	}
	defer func() { <-lock }()

	// 加锁后重新读取，避免用调用方手里的过期快照做判定
	latest, err := a.db.GetAccount(ctx, acc.ID)
	if err != nil {
		return "unknown", false, err
	}
	if accountStatus(latest) == "expired" {
		// 已经是终态：先确保「它还是失效的」这件事送到了运营者手上。
		// 放在刷新尝试之前，是为了让手动刷新也一定发声——
		// 手动刷新的 force 路径会真的去撞一次网络，而那次尝试的结果
		// 可能把它判成 unknown（网络失败），跃迁式通知同样不会触发，
		// 于是「我明明点了刷新，却什么都没收到」。
		a.alertExpired(latest)
		if !force {
			return "expired", false, nil
		}
	}
	if latest.Credentials == nil {
		err = errors.New("account has no credentials")
		_ = a.db.SetAccountStatus(ctx, latest.ID, "unknown")
		return "unknown", false, err
	}

	creds := protocol.CredentialsFromMap(latest.Credentials)
	if !force {
		ahead := a.cfg.KeepAliveAhead
		if resolved, aheadErr := a.accountRefreshAhead(ctx, latest.ID); aheadErr == nil {
			ahead = resolved
		}
		if !credentialsDueForRefresh(creds, time.Now(), ahead) {
			return accountStatus(latest), false, nil
		}
	}

	proxyValue, fallbackDirect, err := a.resolveAccountProxy(ctx, latest.ID)
	if err != nil {
		// 代理解析失败不影响账号本身的有效性判定，按瞬时错误处理
		_ = a.setAccountStatus(ctx, latest.ID, "unknown")
		return "unknown", false, fmt.Errorf("resolve account proxy: %w", err)
	}

	result, refreshErr := a.refreshLoginBufferWithProxy(ctx, creds, proxyValue, fallbackDirect)
	if refreshErr != nil {
		oldStatus := accountStatus(latest)
		status := refreshFailureStatus(oldStatus, creds, refreshErr, time.Now())
		if setErr := a.setAccountStatus(ctx, latest.ID, status); setErr != nil {
			refreshErr = fmt.Errorf("%v; update status: %w", refreshErr, setErr)
		}
		a.notifyIfStatusChanged(latest, oldStatus, status)
		switch {
		case status == "expired":
			// 本轮刚判死：notifyIfStatusChanged 会在「首次发生」时推一条跃迁通知，
			// 之后再交给 alertExpired 按重提醒窗口持续提醒（二者共用同一记账列，不会重复发）。
			a.alertExpired(latest)
		case oldStatus == "expired":
			// 手动刷新把 expired 刷成了 unknown（网络失败不算证据）：账号仍然不可用，
			// 失效告警照样要发，不能因为状态被降级就当成问题消失了。
			a.alertExpired(latest)
		default:
			// 凭据已经过期却没能续上：账号此刻确实不可用。
			// 即便状态为了继续自愈而留在 unknown，这条事实也必须送到运营者手上，
			// 否则一个配错的代理就能让账号静默瘫掉而无人知晓。
			a.notifyRenewalFailure(latest, status, creds, refreshErr)
		}
		return status, false, refreshErr
	}

	if err := a.db.SetAccountCredentialStatus(ctx, latest.ID, result.LoginBuffer, result.Credentials.ToMap(), "alive"); err != nil {
		// 续期本身成功了，只是落库失败。这不是「凭据失效」，
		// 不能返回 expired——否则调用方会清掉退避记录并按终态处理，
		// 一个数据库抖动就会让账号在内存里被当成需重扫。
		return "unknown", false, err
	}
	// 续期成功，当前故障周期结束，下次再过期要重新告警
	if err := a.db.ClearExpiryAlerted(ctx, latest.ID); err != nil {
		log.Printf("[keepalive] 清除续期告警标记失败 (账号 %d): %v", latest.ID, err)
	}
	// 凭据已更新，旧 MMTLS 会话不再有效，必须清理以触发重新登录
	a.invalidateAccountSessions(ctx, latest.ID)
	a.invalidateProxyLease(latest.ID)

	oldStatus := accountStatus(latest)
	if oldStatus != "alive" {
		a.notifyIfStatusChanged(latest, oldStatus, "alive")
	}
	return "alive", true, nil
}

// notifyIfStatusChanged 是状态变更通知的唯一出口。
//
// 之前只有后台 goroutine 推送，而 /accounts/refresh 和 /wxapp/* 也会改写状态：
// 一旦它们先把账号置为 expired，后台就会跳过该账号，告警便永久丢失。
// 把通知收敛到这里后，无论哪条路径触发刷新，跃迁都能被推送出去。
//
// 推送成功才把该状态记为「已通知」；失败则保持旧标记不动，
// 由 retryPendingNotifications 在下一轮扫描时补推。
func (a *App) notifyIfStatusChanged(acc *store.WechatAccount, oldStatus, newStatus string) {
	if !shouldNotifyStatusChange(oldStatus, newStatus) {
		return
	}
	log.Printf("[notify] 账号 %s (%s) 状态变更: %s → %s",
		deref(acc.Nickname), acc.OpenID, oldStatus, newStatus)
	if !a.notifyWebhook(acc, oldStatus, newStatus) {
		log.Printf("[notify] 账号 %s (%s) 的状态变更未送达，已加入待补推",
			deref(acc.Nickname), acc.OpenID)
		return
	}
	if err := a.db.MarkStatusNotified(context.Background(), acc.ID, newStatus); err != nil {
		log.Printf("[notify] 记录推送状态失败 (账号 %d): %v", acc.ID, err)
	}
	// 首次「失效」已经送达到人，重复提醒从这里起算，避免紧接着又被
	// alertExpired 以重提醒的名义再推一条重复内容。
	if newStatus == "expired" {
		if err := a.db.MarkExpiryAlerted(context.Background(), acc.ID, time.Now().Unix()); err != nil {
			log.Printf("[notify] 记录失效告警时间失败 (账号 %d): %v", acc.ID, err)
		}
	}
}

// alertExpired 保证「账号处于 expired」这件事一定被推送出去，并周期性重提醒。
//
// 为什么需要它：账号一旦落到 expired 就是终态，保活与健康检查都会跳过它，
// 手动刷新也不会产生新的状态跃迁（expired → expired 不算变化）。只依赖
// 「状态跃迁才推送」的规则，任何一次跃迁没被捕捉到（升级前就失效、推送失败、
// 服务当时没运行）都会让这个账号永久沉默。
//
// 因此这里不看跃迁，只看当前状态：只要还是 expired 就会推，成功后记账，
// 超过重提醒窗口仍未重新扫码就再推一次，直到账号恢复为止。
func (a *App) alertExpired(acc *store.WechatAccount) {
	if a.getWebhookURL() == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	last, err := a.db.ExpiryAlertedAt(ctx, acc.ID)
	if err != nil {
		log.Printf("[notify] 读取失效告警标记失败 (账号 %d): %v", acc.ID, err)
		return
	}
	now := time.Now()
	if last > 0 && now.Sub(time.Unix(last, 0)) < expiredAlertInterval {
		return
	}
	log.Printf("[notify] 账号 %s (%s) 仍处于失效状态，推送告警",
		deref(acc.Nickname), acc.OpenID)
	if !a.notifyWebhookExpired(acc) {
		log.Printf("[notify] 失效告警未送达 (账号 %d)，下一轮重试", acc.ID)
		return
	}
	if err := a.db.MarkExpiryAlerted(ctx, acc.ID, now.Unix()); err != nil {
		log.Printf("[notify] 记录失效告警时间失败 (账号 %d): %v", acc.ID, err)
	}
	// 同步记账状态，避免补推逻辑再发一条内容重复的变更通知
	if err := a.db.MarkStatusNotified(ctx, acc.ID, "expired"); err != nil {
		log.Printf("[notify] 记录推送状态失败 (账号 %d): %v", acc.ID, err)
	}
}

// notifyRenewalFailure 在「凭据已过期且续不上」时推送告警，并按故障周期去重。
//
// 去重靠 expiry_alerted_at：只有推送成功才写入，所以
//   - 送达失败 → 标记不动 → 下一轮扫描自然重试（等价于补推）；
//   - 已送达且未超过重提醒间隔 → 跳过，避免每 60 秒一条；
//   - 续期成功或重新扫码 → 标记清零，下个故障周期重新告警。
func (a *App) notifyRenewalFailure(acc *store.WechatAccount, status string, creds protocol.LoginBufferCredentials, cause error) {
	// 凭据还没到期的失败只是暂时抖动，保活还有时间窗口去救，不打扰运营者
	if creds.ExpiresAt <= 0 || creds.ExpiresAt > time.Now().Unix() {
		return
	}
	if a.getWebhookURL() == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	last, err := a.db.ExpiryAlertedAt(ctx, acc.ID)
	if err != nil {
		log.Printf("[notify] 读取续期告警标记失败 (账号 %d): %v", acc.ID, err)
		return
	}
	now := time.Now()
	if last > 0 && now.Sub(time.Unix(last, 0)) < renewalAlertInterval {
		return
	}

	reason := renewalFailureReason(cause)
	log.Printf("[notify] 账号 %s (%s) 凭据已过期且无法续期，推送告警: %s",
		deref(acc.Nickname), acc.OpenID, reason)
	if !a.notifyWebhookRenewal(acc, status, reason) {
		log.Printf("[notify] 续期告警未送达 (账号 %d)，下一轮重试", acc.ID)
		return
	}
	if err := a.db.MarkExpiryAlerted(ctx, acc.ID, now.Unix()); err != nil {
		log.Printf("[notify] 记录续期告警时间失败 (账号 %d): %v", acc.ID, err)
	}
}

// renewalFailureReason 把刷新错误折成一句运营者能直接看懂的说明。
func renewalFailureReason(err error) string {
	reason := ""
	if err != nil {
		reason = strings.TrimSpace(err.Error())
	}
	switch {
	case reason == "":
		reason = "续期失败，原因未知"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "刷新请求超时"
	}
	// 错误文本可能很长（含完整 URL 或链路信息），截断避免刷屏
	if runes := []rune(reason); len(runes) > 160 {
		reason = string(runes[:160]) + "…"
	}
	return reason
}

// setAccountStatus 应用状态变更的生命周期副作用。
// 判定失效时同时清理会话、代理租约与退避记录，避免泄漏与无效重试。
func (a *App) setAccountStatus(ctx context.Context, accountID int64, status string) error {
	if err := a.db.SetAccountStatus(ctx, accountID, status); err != nil {
		return err
	}
	if status == "expired" {
		_ = a.db.InvalidateAccountSessions(ctx, accountID)
		a.invalidateProxyLease(accountID)
		a.clearKeepAliveRetry(accountID)
	}
	return nil
}

// refreshLockFor 返回账号专属的刷新锁，保证同一账号的刷新串行执行。
func (a *App) refreshLockFor(accountID int64) chan struct{} {
	a.refreshLocksMu.Lock()
	defer a.refreshLocksMu.Unlock()
	if lock := a.refreshLocks[accountID]; lock != nil {
		return lock
	}
	lock := make(chan struct{}, 1)
	a.refreshLocks[accountID] = lock
	return lock
}

// refreshFailureStatus 根据失败原因判定账号的新状态。
//
// 明确被微信拒绝 → expired（只能重扫）；
// 缓存凭据尚未到期 → 保持原状态（大概率是网络或代理抖动）；
// 其余情况 → unknown，交给下一轮重试。
// refreshFailureStatus 决定刷新失败后账号应该落到哪个状态。
//
// 判定顺序很重要：
//  1. 命中已知失效码（42007 等）→ expired，直接告警；
//  2. 微信侧明确拒绝（业务码非 0）且本地凭据已经过期 → expired。
//     这种情况已经没有可续期的凭据，继续留在 unknown 只会按退避无限重试，
//     运营者永远收不到「该重扫了」的提示——实测 refresh 接口对失效令牌
//     返回的是 -109 + RC_PARAMS_INVALID 这类组合，并不总是标准的 42007；
//  3. 凭据尚未过期 → 维持原状态，下轮再试；
//  4. 其余（网络、代理、超时）→ unknown，不告警。
func refreshFailureStatus(current string, creds protocol.LoginBufferCredentials, err error, now time.Time) string {
	if definitiveCredentialFailure(err) {
		return "expired"
	}
	if refreshRejected(err) && creds.ExpiresAt > 0 && creds.ExpiresAt <= now.Unix() {
		return "expired"
	}
	if creds.ExpiresAt > now.Unix() {
		return current
	}
	return "unknown"
}

// refreshRejected 判断错误是否来自「刷新令牌接口明确返回了业务拒绝码」。
// 只认 RefreshRejectedError：login_buffer 阶段的失败发生在令牌已经换新之后，
// 不能拿它反推旧凭据的状态。
func refreshRejected(err error) bool {
	var rejected *protocol.RefreshRejectedError
	return errors.As(err, &rejected)
}

// definitiveCredentialFailure 判断错误是否表示「凭据已被微信侧彻底拒绝」。
//
// 采用结构化判定：只有拿到接口返回的业务码时才认为凭据可能失效。
// 传输层错误（超时、代理握手失败、JSON 解析失败）一律不在此列——
// 它们可能夹带代理供应商返回的 "token expired"，与微信会话无关。
func definitiveCredentialFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, protocol.ErrMissingRefreshToken) {
		return true
	}
	var refreshRejected *protocol.RefreshRejectedError
	if errors.As(err, &refreshRejected) {
		return definitiveRefreshMessage(refreshRejected.Code, refreshRejected.Message)
	}
	var bufferRejected *protocol.LoginBufferRejectedError
	if errors.As(err, &bufferRejected) {
		return definitiveRefreshMessage(bufferRejected.Code, bufferRejected.Message)
	}
	return false
}

// definitiveRefreshMessage 依据微信返回的业务码与消息文本判定是否为终态失效。
//
// 关键错误码：
//   - 42007：access_token 与 refresh_token 同时失效，必须重新扫码；
//   - 40188：授权 scope 失效，同样只能重扫。
//
// 其余情况要求消息中同时出现「refresh token 类」词汇与「失效类」词汇才判终态，
// 避免把「access_token 过期」（可自动续期）误判成需要重扫。
func definitiveRefreshMessage(code int, raw string) bool {
	// 42007 是「refresh token 过期/失效」的标准业务码，40188 是「scope 无效」。
	// 这两个码本身就是结论，不再依赖 errmsg 的措辞——errmsg 可能是空的、
	// 本地化的，或换了一版文案，靠它做二次判断会把真实失效漏成 unknown。
	if code == 42007 || code == 40188 {
		return true
	}

	message := strings.ToLower(strings.TrimSpace(raw))
	// 把 refreshtoken / refresh_token / refresh-token 统一成 "refresh token"
	message = strings.NewReplacer("_", " ", "-", " ", "refreshtoken", "refresh token").Replace(message)
	refreshToken := strings.Contains(message, "refresh token") || strings.Contains(message, "刷新令牌")
	if strings.Contains(message, "42007") && refreshToken {
		return true
	}
	if strings.Contains(message, "40188") && strings.Contains(message, "invalid scope") {
		return true
	}
	invalid := strings.Contains(message, "invalid") || strings.Contains(message, "expired") ||
		strings.Contains(message, "expire") || strings.Contains(message, "无效") ||
		strings.Contains(message, "过期") || strings.Contains(message, "失效")
	relogin := strings.Contains(message, "relogin") || strings.Contains(message, "re login") ||
		strings.Contains(message, "重新登录") || strings.Contains(message, "重新授权")
	return relogin || (invalid && refreshToken)
}

// accountRefreshAhead 返回该账号应使用的提前续期窗口。
// 动态代理的租约很短，使用账号自己的 refresh_ahead_seconds；直连沿用全局配置。
func (a *App) accountRefreshAhead(ctx context.Context, accountID int64) (time.Duration, error) {
	setting, err := a.db.AccountProxySettingOrDefault(ctx, accountID)
	if err != nil {
		return 0, fmt.Errorf("read account proxy setting: %w", err)
	}
	if setting.Mode == "direct" {
		return a.cfg.KeepAliveAhead, nil
	}
	return time.Duration(setting.RefreshAheadSeconds) * time.Second, nil
}

// refreshLoginBufferWithProxy 按代理配置刷新凭据。proxyValue 为空时走直连客户端。
func (a *App) refreshLoginBufferWithProxy(ctx context.Context, creds protocol.LoginBufferCredentials, proxyValue string, fallbackDirect bool) (protocol.LoginBufferResult, error) {
	if proxyValue == "" {
		return a.qr.RefreshLoginBuffer(ctx, creds)
	}
	client, err := qr.NewClientWithProxy(a.cfg.RequestTimeout, proxyValue, fallbackDirect)
	if err != nil {
		return protocol.LoginBufferResult{}, err
	}
	return client.RefreshLoginBuffer(ctx, creds)
}

// fetchUserInfoWithProxy 按代理配置拉取用户信息。
func (a *App) fetchUserInfoWithProxy(ctx context.Context, creds protocol.LoginBufferCredentials, proxyValue string, fallbackDirect bool) (map[string]any, error) {
	if proxyValue == "" {
		return a.qr.LoginBuffers().FetchUserInfo(ctx, creds)
	}
	client, err := qr.NewClientWithProxy(a.cfg.RequestTimeout, proxyValue, fallbackDirect)
	if err != nil {
		return nil, err
	}
	return client.LoginBuffers().FetchUserInfo(ctx, creds)
}

// credentialsDueForRefresh 判断凭据是否已进入续期窗口。
// ExpiresAt 未知（0）时按「需要刷新」处理，避免老数据永不续期。
func credentialsDueForRefresh(creds protocol.LoginBufferCredentials, now time.Time, ahead time.Duration) bool {
	return creds.ExpiresAt <= 0 || now.Add(ahead).Unix() >= creds.ExpiresAt
}

// keepAliveAccountTimeout 给单个账号的刷新设置超时上限。
func keepAliveAccountTimeout(requestTimeout time.Duration) time.Duration {
	if requestTimeout <= 0 {
		return 30 * time.Second
	}
	if timeout := requestTimeout * 4; timeout > 30*time.Second {
		return timeout
	}
	return 30 * time.Second
}

// accountStatus 返回账号状态字符串，空值归一化为 unknown。
func accountStatus(acc *store.WechatAccount) string {
	if acc == nil || acc.Status == nil || *acc.Status == "" {
		return "unknown"
	}
	return *acc.Status
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
