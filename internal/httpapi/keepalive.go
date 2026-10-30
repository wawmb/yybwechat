package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"yybwechat/internal/eventlog"
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

	// keepAliveUrgentBackoff 是「凭据已进入续期窗口」时的失败重试间隔。
	//
	// 默认提前量只有 4 分钟：用 5 分钟起步的退避，一次失败就把整个可续期窗口
	// 跳过去了，只能等凭据真正过期后才再试——那时已经错过最佳时机。
	// 窗口内改用几十秒级的短退避，才有机会在过期前救回来。
	keepAliveUrgentBackoff = 30 * time.Second

	// keepAliveUrgentWindow 是判定「是否已进入续期窗口」的提前量下限。
	// 取默认提前量加一点余量：即便账号配了更长的提前量，也不会被当成紧急。
	keepAliveUrgentWindow = 6 * time.Minute

	// renewalAlertInterval 是「续期失败」告警的重提醒间隔。
	// 账号持续瘫痪时每 2 小时再提醒一次：掉线这类事越早再提醒一次越好，
	// 2 小时既不会刷屏，也不至于响过一声就沉默。
	renewalAlertInterval = 2 * time.Hour

	// expiredAlertInterval 是「账号已失效（expired）」告警的重提醒间隔。
	// expired 是终态，只要账号没被重新扫码，就按这个间隔一直提醒到恢复为止。
	expiredAlertInterval = 2 * time.Hour

	// paramsInvalidCode 是应用宝 refresh 接口表示「参数无效」的业务码。
	// 拿失效或被轮换掉的令牌去刷新，实测返回的就是 -109 + RC_PARAMS_INVALID，
	// 而不是文档里的标准失效码 42007。
	paramsInvalidCode = -109

	// credentialRejectLimit 是「连续被微信以参数无效码拒绝」的收敛阈值。
	//
	// 单次 -109 不足以判死：它也可能来自我们自己构造请求的问题，
	// 把还活着的账号打成终态不值得。但连续命中同一个账号、同一个形态，
	// 就足以说明服务端确实不认这套凭据——继续留在 unknown 按退避重试，
	// 最多拖到 expires_at 才收敛，期间每次协议调用都会撞墙吐 502。
	credentialRejectLimit = 3
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
			// 跳过刷新不代表账号没问题：未到期是正常跳过，但「凭据已过期却在
			// 失败退避里」意味着账号此刻不可用。这条路径以前完全静默，
			// 所以第一次告警没送达就再没有第二次机会，这里补上。
			a.alertUnusableCredentials(acc, accountStatus(acc))
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
	// 「仅原会话可用」同样不再刷：授权已经明确被拒，重刷只会重复撞墙。
	// 缓存会话由协议层按需使用，这里不参与。
	if acc.SessionOnly() {
		return true
	}
	if acc.Credentials == nil {
		return true
	}
	ahead := a.cfg.KeepAliveAhead
	if setting, err := a.db.AccountProxySettingOrDefault(context.Background(), acc.ID); err == nil {
		// 动态代理模式曾经在这里整条跳过，理由是"每次提取都换出口，用它刷新
		// 维持不了会话、还白耗额度"。那时跳过的代价更大——这类账号永远得不到
		// 保活，撑到 expires_at 就死掉，等于放弃了一批账号。
		//
		// 现在照常参与保活。本函数负责的是「凭据快到点才刷」（下面第 158 行
		// 的到期判断），能省掉大部分空转轮次；但真正的刷新频率还受健康检查影响：
		// scanAndCheck 对超过 --check-stale（默认 1 小时）未检查的账号会强制刷新，
		// 所以默认配置下每个账号大约每小时真正刷新一次。要省额度就调大 check-stale
		// 或关掉 --check-interval。租约（45 秒）只作用于单次刷新内的多次尝试。
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

// pushStartupAlerts 在服务启动后立刻补推一轮，不等第一个定时器触发。
//
// 为什么必须有：time.NewTicker 的第一次触发要等满一个间隔（保活默认 60 秒，
// 健康检查默认 30 分钟），而两个定时器都可以被 --keepalive-interval 0 /
// --check-interval 0 关掉。于是「重启期间已经失效的账号」和「停机前那次没送达
// 的告警」最长要等 30 分钟、或者永远等不到推送——这正好是掉线告警最不该丢的时刻。
//
// 这里只读数据库并推送，不做任何网络刷新：避免重启瞬间对所有账号发起连接风暴。
func (a *App) pushStartupAlerts() {
	if a.getWebhookURL() == "" {
		return
	}
	ctx := context.Background()
	// 先把停机前没送达的状态变更补掉
	a.retryPendingNotifications(ctx)

	accounts, err := a.db.ListAccounts(ctx)
	if err != nil {
		log.Printf("[notify] 启动补推读取账号列表失败: %v", err)
		return
	}
	for _, acc := range accounts {
		switch {
		case accountStatus(acc) == "expired":
			// 终态账号必须持续提醒，直到重新扫码
			a.alertExpired(acc)
		default:
			// 凭据已过期 / 凭据缺失：账号此刻不可用，同样要发声。
			// 凭据仍在有效期内的账号会被静默跳过。
			a.alertUnusableCredentials(acc, accountStatus(acc))
		}
	}
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
			a.events.Emit("keepalive.renewed", eventlog.ForAccount(acc))
			log.Printf("[keepalive] 账号 %s (%s) 凭据已续期",
				deref(acc.Nickname), acc.OpenID)
		}
	case status == "expired":
		// 终态：清掉退避记录，后续由 expired 判定直接跳过
		a.clearKeepAliveRetry(acc.ID)
		fields := eventlog.ForAccount(acc)
		fields.Rejections = eventRejections(err)
		a.events.Emit("keepalive.failed", fields)
		a.events.Emit("account.expired", fields)
		log.Printf("[keepalive] 账号 %s (%s) 凭据已彻底失效，需重新扫码: %v",
			deref(acc.Nickname), acc.OpenID, err)
	case status == store.StatusSessionOnly:
		// 既不是终态也不是健康：清掉退避（后续扫描会直接跳过这个账号），
		// 但保留缓存会话，等运营者重新扫码。
		a.clearKeepAliveRetry(acc.ID)
		fields := eventlog.ForAccount(acc)
		fields.Rejections = eventRejections(err)
		a.events.Emit("keepalive.failed", fields)
		log.Printf("[keepalive] 账号 %s (%s) 授权已被拒绝，仅保留已有协议会话，需重新扫码: %v",
			deref(acc.Nickname), acc.OpenID, err)
	default:
		urgent := credentialsNearExpiry(acc, time.Now()) && !isPCMgrAccount(acc)
		fields := eventlog.ForAccount(acc)
		fields.Rejections = eventRejections(err)
		a.events.Emit("keepalive.failed", fields)
		a.markKeepAliveRetry(acc.ID, urgent)
		log.Printf("[keepalive] 账号 %s (%s) 刷新失败，将退避重试（紧急=%v）: %v",
			deref(acc.Nickname), acc.OpenID, urgent, err)
	}
}

// markKeepAliveRetry 记录账号的下次可重试时间（失败退避）。
//
// 连续失败时退避按 2 倍递增（5min → 10min → 20min …）并封顶在 keepAliveMaxDelay，
// 避免长期失效的账号被高频重试而给微信侧造成压力。
func (a *App) markKeepAliveRetry(accountID int64, urgent bool) {
	a.keepAliveMu.Lock()
	defer a.keepAliveMu.Unlock()

	fails := a.keepAliveFails[accountID] + 1
	a.keepAliveFails[accountID] = fails

	if urgent {
		// 窗口内不累积指数退避：每次失败都只等一小会儿就再试，
		// 否则凭据会在等待期间直接过期，连一次补救机会都没有。
		a.keepAliveNext[accountID] = time.Now().Add(keepAliveUrgentBackoff)
		return
	}

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
	// 「仅原会话可用」的账号不再尝试任何 OAuth 刷新。
	//
	// 这里的取舍是刻意的：授权已经被明确拒绝过一次，再刷只会重复撞墙，
	// 而每一次失败都在给上游增加「这套凭据可疑」的证据。
	// 缓存会话该用还能用（由协议层判断），但不再声称账号健康，
	// 也不启动新的 30 天倒计时——等运营者重新扫码即可。
	if latest.SessionOnly() {
		if !force {
			return store.StatusSessionOnly, false, nil
		}
		return store.StatusSessionOnly, false, errors.New("授权已被拒绝，仅可使用已有协议会话；需重新扫码")
	}
	if latest.Credentials == nil {
		err = errors.New("account has no credentials")
		// 没有凭据是**终态**，不是「无法判定」：刷新连输入都没有，任何重试都不可能成功。
		//
		// 以前记成 unknown（界面「待确认」），后果是健康检查每两分钟重试一次、
		// 永远失败在同一行，而 Public() 里「建议重扫」要靠凭据里的到期时间推导，
		// 没有凭据就永远不点亮——账号于是安静地烂在列表里，运营者只能靠猜。
		a.persistStatus(latest.ID, "expired")
		a.noteRefreshError(latest.ID, err)
		// 没有凭据就永远刷不回来，也没有任何网络动作会暴露它，
		// 不主动提醒的话它只会安静地留在列表里。
		a.alertUnusableCredentials(latest, "expired")
		return "expired", false, accountExpiredError{
			accountID: latest.ID,
			openid:    latest.OpenID,
			cause:     err,
		}
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

	// 凭据刷新沿用账号自己的出口，不另开一条"直连"通道。
	//
	// 曾经改成动态代理账号一律走直连，理由是"刷新只是普通 HTTPS，不需要换出口"。
	// 但换出口本身就是风险来源：刷新出口与扫码/协议出口不一致，微信侧看到的
	// 来源身份就变了——实测中被拒时返回 -109 RC_PARAMS_INVALID，而我们又把它
	// 当成凭据失效处理，等于自己制造拒绝、再自己把账号判死。
	// 参考实现（v0.2.29 #84）也明确写：凭据刷新仍使用原账号代理。
	//
	// 额度消耗：保活按「到期才刷新」省掉空转轮次，但健康检查会对超过
	// --check-stale（默认 1 小时）未检查的账号强制刷新，两者叠加后默认约每小时
	// 一次。单次刷新内的多次尝试由租约（45 秒）复用同一出口。
	proxyValue, fallbackDirect, err := a.resolveAccountProxy(ctx, latest.ID)
	if err != nil {
		// 代理解析失败不影响账号本身的有效性判定，按瞬时错误处理。
		//
		// 但「不判死」不等于「不告警」：动态代理提取失败（额度用完、IP 白名单变了、
		// 供应商接口挂掉）会让这个账号从此刷不动，凭据到期后就是彻底不可用。
		// 这条路径以前只落库、不发声，运营者只会看到账号安静地变成「待确认」——
		// 必须把原因送出去。凭据还没到期时 notifyRenewalFailure 会自己返回，
		// 所以不会因为一次代理抖动就打扰。
		a.persistStatus(latest.ID, "unknown")
		a.noteRefreshError(latest.ID, fmt.Errorf("resolve account proxy: %w", err))
		a.notifyRenewalFailure(latest, "unknown", creds, err)
		return "unknown", false, fmt.Errorf("resolve account proxy: %w", err)
	}

	// 第一步只做「续期」：换 token，不取票据。
	//
	// 拆开的意义在于省请求：后台每两小时给每个账号刷一次凭据，
	// 如果每次顺带取一张票据，而票据只在真正发起协议调用时才用得上，
	// 那么绝大多数取票请求都是纯浪费——多一次上游交互就多一次被风控看见的机会。
	refreshed, refreshErr := a.refreshCredentialsWithProxy(ctx, creds, proxyValue, fallbackDirect)
	if refreshErr != nil {
		oldStatus := accountStatus(latest)
		rejects := a.noteCredentialRejection(latest.ID, refreshErr)
		status := refreshFailureStatus(oldStatus, creds, refreshErr, time.Now(), rejects)
		// 已判死的账号不因「这次刷新失败」而被推翻。
		//
		// 失败不构成任何新证据：既没有证明它复活，也没有证明之前的判定错了。
		// 以前这里会把它降级成 unknown，直接后果是——
		//   · 用户每点一次「刷新存活」，卡片就在「需重扫」和「待确认」之间来回跳；
		//   · unknown 会重新进入重试队列，还得再撞三次 -109 才能回到终态，
		//     期间白白消耗刷新次数，账号却一直是不可用的。
		// 想把它从 expired 救回来只有一条路：刷新真的成功（走下面的 alive 分支）。
		if oldStatus == "expired" {
			status = "expired"
		} else if status == "expired" {
			// OAuth 被拒不能推出「ILink 会话也失效了」。只要这条出口上
			// 还留着未到期的 PSK 或独立原生恢复材料，就转入 session_only：
			// 缓存请求继续可用，但不再声称账号健康，也不启动新的倒计时。
			protocolProxy, directFallback, routeErr := a.resolveWXAppProxy(ctx, latest.ID)
			if routeErr == nil && (a.pool.HasReusableSession(ctx, latest.ID, protocolProxy) ||
				protocolProxy != "" && directFallback && a.pool.HasReusableSession(ctx, latest.ID, "")) {
				status = store.StatusSessionOnly
			}
		}
		a.noteRefreshError(latest.ID, refreshErr)
		applied, setErr := a.applyRefreshResult(latest, latest.LoginBuffer, latest.Credentials, status)
		if setErr != nil {
			refreshErr = fmt.Errorf("%v; update status: %w", refreshErr, setErr)
		} else if !applied {
			// 账号已被替换：这次失败是针对旧身份的，既不落库也不告警
			log.Printf("[keepalive] 账号 %d 在刷新期间已被替换，失败结果已丢弃", latest.ID)
			return "unknown", false, fmt.Errorf("account changed during refresh; result discarded: %w", refreshErr)
		}
		a.notifyIfStatusChanged(latest, oldStatus, status)
		switch {
		case status == "expired":
			// 终态：确保「它现在仍是失效的」这件事送到运营者手上。
			// 跃迁那次由 notifyIfStatusChanged 负责，这里是周期性重提醒，
			// 两者共用同一记账列，不会连发两份。
			a.alertExpired(latest)
		default:
			// 凭据已经过期却没能续上：账号此刻确实不可用。
			// 即便状态为了继续自愈而留在 unknown，这条事实也必须送到运营者手上，
			// 否则一个配错的代理就能让账号静默瘫掉而无人知晓。
			a.notifyRenewalFailure(latest, status, creds, refreshErr)
		}
		return status, false, refreshErr
	}

	// 轮换结果必须**先落库**再做任何后续请求。
	//
	// refresh token 一旦被服务端轮换，旧值即刻作废。如果先取票据、取票失败就返回，
	// 库里留下的还是旧 token，下一轮必然被拒——等于我们自己把账号弄死。
	saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(ctx), statusWriteTimeout)
	applied, saveErr := a.db.ApplyCredentialRefresh(saveCtx, latest, latest.LoginBuffer, refreshed.ToMap(), "alive")
	cancelSave()
	if saveErr != nil {
		// 续期本身成功了，只是落库失败。这不是「凭据失效」，
		// 不能返回 expired——否则调用方会清掉退避记录并按终态处理，
		// 一个数据库抖动就会让账号在内存里被当成需重扫。
		return "unknown", false, saveErr
	}
	if !applied {
		log.Printf("[keepalive] 账号 %d 在刷新期间已被替换，本次续期结果已丢弃", latest.ID)
		return "unknown", false, errors.New("account changed during refresh; result discarded")
	}
	saved, err := a.db.GetAccount(context.WithoutCancel(ctx), latest.ID)
	if err != nil {
		return "alive", true, fmt.Errorf("credentials renewed; reload account: %w", err)
	}
	if !force {
		// 后台保活到此为止：只续期，不取票。票据在真正需要发起协议调用时再取
		// （见 invokeWXApp 的 -10 恢复分支）。
		a.clearCredentialRejections(latest.ID)
		a.noteRefreshError(latest.ID, nil)
		if err := a.db.ClearExpiryAlerted(ctx, latest.ID); err != nil {
			log.Printf("[keepalive] 清除续期告警标记失败 (账号 %d): %v", latest.ID, err)
		}
		oldStatus := accountStatus(latest)
		if oldStatus != "alive" {
			a.notifyIfStatusChanged(latest, oldStatus, "alive")
		}
		return "alive", true, nil
	}

	// 第二步（仅强制刷新 / 协议调用恢复时）：取一张新票据。
	buffer, bufErr := a.fetchLoginBufferWithProxy(ctx, refreshed, proxyValue, fallbackDirect)
	if bufErr != nil {
		// 微信明确说这套授权没有该 scope（-101 + 40188 invalid scope）时，结论已经出来了：
		// 续期能成功只说明 refresh token 还能换 token，不代表这个号还能取票据。
		//
		// 以前这里一律保持 alive 等下次再试，后果是「界面永远显示可用、每次真实调用
		// 都 502」——账号明明死了，却要运营者自己去猜。这类拒绝是结构化的、
		// 可复现的，重试一万次也不会变成功，只能重新扫码。
		// 复用刷新阶段那套「终态证据」判定：带 40188 invalid scope 的
		// LoginBufferRejectedError 在 definitiveCredentialFailure 里已经是
		// 终态，只是这条分支以前压根没去问它，才让账号一直顶着 alive。
		if definitiveCredentialFailure(bufErr) {
			oldStatus := accountStatus(latest)
			a.persistStatus(latest.ID, "expired")
			a.noteRefreshError(latest.ID, bufErr)
			a.notifyIfStatusChanged(latest, oldStatus, "expired")
			a.alertExpired(latest)
			return "expired", true, accountExpiredError{
				accountID: latest.ID,
				openid:    latest.OpenID,
				cause:     bufErr,
			}
		}
		// 其余情况：凭据已经换成新的了，只是票据没拿到。状态保持 alive，
		// 下一次协议调用会再试一次取票，不要因为这一步失败就把账号判死。
		return "alive", true, fmt.Errorf("credentials renewed; fetch login buffer: %w", bufErr)
	}
	saveBufCtx, cancelBufSave := context.WithTimeout(context.WithoutCancel(ctx), statusWriteTimeout)
	appliedBuf, bufSaveErr := a.db.ApplyCredentialRefresh(saveBufCtx, saved, buffer, saved.Credentials, "alive")
	cancelBufSave()
	if bufSaveErr != nil || !appliedBuf {
		if bufSaveErr == nil {
			bufSaveErr = errors.New("account changed during refresh; result discarded")
		}
		return "unknown", false, bufSaveErr
	}
	// 续期成功，本轮凭据被认可，「连续拒绝」的账要清掉，下次重新计数
	a.clearCredentialRejections(latest.ID)
	a.noteRefreshError(latest.ID, nil)
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

// noteRefreshError 记住某账号最近一次刷新失败的原因，供 /accounts 展示。
//
// 之前只有日志里有原因，用户看到「需重扫」却不知道是微信拒绝、代理故障还是超时。
// 空字符串表示清除（刷新成功了）。
func (a *App) noteRefreshError(accountID int64, err error) {
	a.keepAliveMu.Lock()
	defer a.keepAliveMu.Unlock()
	if a.lastRefreshErrs == nil {
		a.lastRefreshErrs = map[int64]string{}
	}
	if err == nil {
		delete(a.lastRefreshErrs, accountID)
		return
	}
	msg := strings.TrimSpace(err.Error())
	if runes := []rune(msg); len(runes) > 200 {
		msg = string(runes[:200]) + "…"
	}
	a.lastRefreshErrs[accountID] = msg
}

// lastRefreshError 返回某账号最近一次刷新失败的原因（没有则空串）。
func (a *App) lastRefreshError(accountID int64) string {
	a.keepAliveMu.Lock()
	defer a.keepAliveMu.Unlock()
	return a.lastRefreshErrs[accountID]
}

// noteAlertWindowSkip 在「因去重窗口而跳过告警」时打一行日志。
//
// 为什么需要：窗口内跳过是静默的，日志里「没推送」和「推送被去重」长得一模一样，
// 排查时只能靠猜（真实发生过：用户问「为什么没推送」，日志里一条 [notify] 都没有）。
// 每个账号每小时最多一行，既不刷屏，也让这件事在日志里自己说清楚。
func (a *App) noteAlertWindowSkip(acc *store.WechatAccount, kind string, lastUnix int64, window time.Duration) {
	a.keepAliveMu.Lock()
	if a.alertSkipLogged == nil {
		a.alertSkipLogged = map[int64]time.Time{}
	}
	if t, ok := a.alertSkipLogged[acc.ID]; ok && time.Since(t) < time.Hour {
		a.keepAliveMu.Unlock()
		return
	}
	a.alertSkipLogged[acc.ID] = time.Now()
	a.keepAliveMu.Unlock()

	log.Printf("[notify] 账号 %s (%s) 的%s告警在去重窗口内（上次推送 %s，窗口 %s），本轮跳过",
		deref(acc.Nickname), acc.OpenID, kind,
		time.Unix(lastUnix, 0).Format("01-02 15:04"), window)
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
		a.noteAlertWindowSkip(acc, "失效", last, expiredAlertInterval)
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
	a.pushRenewalAlert(acc, status, renewalFailureReason(cause))
}

// alertUnusableCredentials 在「没有本次刷新错误」的情况下补发告警。
//
// 存在的理由：账号一旦进入失败退避（5 分钟 → 1 小时递增），后续扫描会直接
// keepAliveShouldSkip 跳过，根本不会再走到 refreshAccount，也就不会再有
// notifyRenewalFailure 的机会。如果第一次告警恰好没送达，就只能干等退避到期；
// 更糟的是退避最长 1 小时，而凭据早就过期了——这段时间账号不可用却完全静默。
//
// 因此跳过刷新的路径也要检查「账号现在还能不能用」：
//   - 完全没有凭据：刷新根本发不出去，永远自愈不了，必须提醒重扫；
//   - 凭据已过期：账号当前不可用。
//
// 两种情况都走同一个 2 小时去重窗口，送达才记账，因此推送失败会一直重试。
func (a *App) alertUnusableCredentials(acc *store.WechatAccount, status string) {
	if acc.Credentials == nil {
		a.pushRenewalAlert(acc, status,
			"账号缺少登录凭据（credentials 为空），无法发起刷新，需要重新扫码")
		return
	}
	creds := protocol.CredentialsFromMap(acc.Credentials)
	if creds.ExpiresAt <= 0 || creds.ExpiresAt > time.Now().Unix() {
		// 凭据仍在有效期内：账号此刻可用，跳过刷新属于正常节奏
		return
	}
	a.pushRenewalAlert(acc, status,
		"凭据已过期，自动刷新仍在失败退避中（上次刷新失败后暂缓重试），账号当前不可用")
}

// pushRenewalAlert 是续期类告警的唯一出口：判定该不该发、发、送达才记账。
//
// 是否「值得报警」由调用方判断（凭据过期 / 凭据缺失）；这里只管去重与记账：
//   - 2 小时窗口内已送达过 → 不发，避免每 60 秒一条；
//   - 送达失败 → 不记账，下一轮扫描自然重试，等价于补推。
func (a *App) pushRenewalAlert(acc *store.WechatAccount, status, reason string) {
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
		a.noteAlertWindowSkip(acc, "续期失败", last, renewalAlertInterval)
		return
	}

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
		// 超时可能发生在「提取代理」而不是「刷新凭据」这两个完全不同的阶段。
		// 一律改写成「刷新请求超时」会把排查方向带偏——运营者会去查微信侧，
		// 实际问题却在他自己配的代理供应商那边。已经写明阶段的就保留原文。
		if !strings.Contains(reason, "代理") {
			reason = "刷新请求超时"
		}
	}
	// 错误文本可能很长（含完整 URL 或链路信息），截断避免刷屏
	if runes := []rune(reason); len(runes) > 160 {
		reason = string(runes[:160]) + "…"
	}
	// 提示放在截断之后，保证这条最关键的行动指引不会被前面的长文本挤掉
	if suspectCredentialRejection(err) {
		reason += "（微信侧不认可该凭据，通常需要重新扫码）"
	}
	return reason
}

// statusWriteTimeout 是状态/凭据落库的独立超时。
const statusWriteTimeout = 5 * time.Second

// persistStatus 用独立上下文写账号状态，刻意不复用刷新调用的 ctx。
//
// 原因很实际：刷新调用的 ctx 往往就是刚刚超时的那一条（保活给整次刷新设了
// 总预算，keepAliveAccountTimeout）。拿它去写库会一并失败，结果是
// 「判定出 expired 了、但状态没落库」——库里账号继续显示可用，保活下一轮
// 照旧刷新、照旧失败，而依赖 expired 终态的失效告警永远不会触发。
//
// 写库是毫秒级操作，用独立的短超时既安全又不会因为上游取消而丢状态。
func (a *App) persistStatus(accountID int64, status string) error {
	ctx, cancel := context.WithTimeout(context.Background(), statusWriteTimeout)
	defer cancel()
	return a.setAccountStatus(ctx, accountID, status)
}

// persistCredentials 续期成功后写入新凭据与状态，同样使用独立上下文。
//
// 这条尤其不能失败：refresh token 有可能已经被微信轮换，旧凭据已经作废。
// 若因为调用方 ctx 超时而没写进去，账号就带着废掉的凭据继续留着，
// 下一轮刷新会直接被判 -109，等于自己把账号弄死。
// applyRefreshResult 以条件更新写回「凭据 + 状态」，返回 false 表示库里已被别人改过。
//
// 用 CAS 而不是按 ID 直写：刷新持续数秒，期间账号可能被更新链接、合并或跨渠道重扫
// 整体替换。旧结果写进去会把新身份覆盖回去，而调用方看不出任何异常。
func (a *App) applyRefreshResult(expected *store.WechatAccount, loginBuffer string, credentials map[string]any, status string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statusWriteTimeout)
	defer cancel()
	return a.db.ApplyCredentialRefresh(ctx, expected, loginBuffer, credentials, status)
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
		// 已判死就没有再统计「连续拒绝」的意义了，重新扫码后从头计
		a.clearCredentialRejections(accountID)
	}
	return nil
}

// noteCredentialRejection 记录一次「疑似凭据被服务端作废」并返回连续次数。
//
// 只有与上一次同类（同码同形态）才累加；失败原因一变——无论是网络抖动、
// 换了别的业务码，还是刷新成功——计数立即归零。这样「连续」才是真的连续，
// 不会出现「三年前失败过两次，今天再失败一次就被判死」的错账。
func (a *App) noteCredentialRejection(accountID int64, err error) int {
	a.keepAliveMu.Lock()
	defer a.keepAliveMu.Unlock()
	if !suspectCredentialRejection(err) {
		delete(a.credentialRejects, accountID)
		return 0
	}
	if a.credentialRejects == nil {
		a.credentialRejects = map[int64]int{}
	}
	n := a.credentialRejects[accountID] + 1
	a.credentialRejects[accountID] = n
	return n
}

// resetAccountRuntimeState 清空所有「以账号 ID 为键」的内存状态：
// 代理租约、失败退避、连续拒绝计数。
//
// /accounts/compact 会重排并复用账号 ID。重排之后这些缓存全部失去意义：
// 留着的话，新账号可能直接继承上一个账号的代理出口（登录 IP 与保活 IP 不一致），
// 或者继承一份退避时间/拒绝计数，被无端跳过刷新、甚至下一次 -109 就被判死。
func (a *App) resetAccountRuntimeState() {
	a.proxyMu.Lock()
	a.proxyLeases = map[int64]accountProxyLease{}
	a.proxyLeaseLocks = map[int64]chan struct{}{}
	a.proxyMu.Unlock()

	a.keepAliveMu.Lock()
	a.keepAliveNext = map[int64]time.Time{}
	a.keepAliveFails = map[int64]int{}
	a.credentialRejects = map[int64]int{}
	a.alertSkipLogged = map[int64]time.Time{}
	a.lastRefreshErrs = map[int64]string{}
	a.keepAliveMu.Unlock()
}

// clearCredentialRejections 清除账号的「连续凭据拒绝」计数。
func (a *App) clearCredentialRejections(accountID int64) {
	a.keepAliveMu.Lock()
	defer a.keepAliveMu.Unlock()
	delete(a.credentialRejects, accountID)
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

// refreshFailureStatus 决定刷新失败后账号应该落到哪个状态。
//
// 判定顺序：
//  1. 命中终态失效证据（42007 / 40188 / 明确要求重登 / 缺少 refresh token）→ expired；
//  2. 连续被微信侧以「参数无效」码拒绝达到阈值 → expired。
//     实测失效/被轮换的令牌在 refresh 接口上返回 -109 + RC_PARAMS_INVALID 而不是
//     标准 42007，消息里既没有 refresh token 字样也没有失效字样，第 1 条抓不住。
//     但它同样是「服务端在说不认这套凭据」的结构化业务码，连续命中就该收敛；
//     单次不判死——一次参数异常不值得把一个还能用的账号钉成终态；
//  3. 凭据尚未过期 → 维持原状态，下轮再试；
//  4. 其余（凭据已过期但结论不明、网络、代理、超时）→ unknown，不告警。
//
// 第 4 条里「凭据已过期但仍判 unknown」是刻意的：本地到期只是推断，不是证据。
// 参考实现（v0.2.33 #96）把「业务码 + 到期」从终态降级回 unknown 也是同一个道理——
// 只有服务端明确表态才算数，否则代理抖动一次就能把账号钉死。
//
// rejects 是「连续」被同类业务码拒绝的次数，由 noteCredentialRejection 统计。
func refreshFailureStatus(current string, creds protocol.LoginBufferCredentials, err error, now time.Time, rejects int) string {
	if definitiveCredentialFailure(err) {
		return "expired"
	}
	// -109 累计到阈值也只在本条凭据真的过期后才判死。
	//
	// -109 的字面语义是「参数不合法」，我们自己的请求出问题同样会命中；
	// 而进入续期窗口（默认只剩 4 分钟）后重试间隔被压到 30 秒，
	// 三次 -109 九十秒就能凑齐——一次上游抖动就足以把还能用的账号判死。
	// 由于续期只发生在最后 4 分钟，"要求凭据已过期"最多把判定推迟 4 分钟，
	// 却把误杀窗口压到零。
	if rejects >= credentialRejectLimit && suspectCredentialRejection(err) && creds.ExpiresAt <= now.Unix() {
		return "expired"
	}
	if creds.ExpiresAt > now.Unix() {
		return current
	}
	return "unknown"
}

// suspectCredentialRejection 判断错误是否为「应用宝 refresh 接口的参数无效拒绝」。
//
// 它是「疑似」而不是「确定」：-109 RC_PARAMS_INVALID 的字面语义是参数不合法，
// 我们自己构造请求出错（空 access token、openid 不匹配、代理返回被截断的响应）
// 同样会命中。因此单次出现不足以判死，交给 noteCredentialRejection 连续计数，
// 达到 credentialRejectLimit 才收敛到终态。
//
// 同时它也用于 renewalFailureReason 给告警文案加一句可行动的提示——
// 「不认这套凭据」对运营者的含义就是「去重新扫码」，光看 -109 看不出来。
func suspectCredentialRejection(err error) bool {
	var rejected *protocol.RefreshRejectedError
	if !errors.As(err, &rejected) {
		return false
	}
	return isParamsInvalidRejection(rejected.Code, rejected.Message)
}

// isParamsInvalidRejection 判断业务码与消息是否命中「参数无效」形态。
//
// 由 suspectCredentialRejection 调用，后者只认 RefreshRejectedError，
// 因此天然只覆盖 refresh 阶段——login_buffer 阶段的失败发生在令牌已经换新
// 之后，那时凭据刚被证明有效，拿它反推「旧凭据失效」不成立。
// 消息里必须带 params invalid，避免把其它 -109（系统繁忙、限流等）也算进来。
func isParamsInvalidRejection(code int, raw string) bool {
	if code != paramsInvalidCode {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(raw))
	message = strings.NewReplacer("_", " ", "-", " ").Replace(message)
	return strings.Contains(message, "params invalid")
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
	// 先看这一层自身的类型，再递归展开整条错误链。
	//
	// 不能用 errors.As：它只返回链上第一个匹配项。refresh 与 check-login
	// 是两段独立原因（现在两段都用 %w 包裹），第一段含糊的 -109 会把
	// 后一段明确的 42007 挡住，于是本该判死的账号被当成"可重试"。
	switch rejected := err.(type) {
	case *protocol.RefreshRejectedError:
		if definitiveRefreshMessage(rejected.Code, rejected.Message) {
			return true
		}
	case *protocol.LoginBufferRejectedError:
		if definitiveRefreshMessage(rejected.Code, rejected.Message) {
			return true
		}
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range wrapped.Unwrap() {
			if definitiveCredentialFailure(cause) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return definitiveCredentialFailure(wrapped.Unwrap())
	}
	return false
}

// definitiveRefreshMessage 依据微信返回的业务码与消息文本判定是否为终态失效。
//
// 关键错误码：
//   - 42007：access_token 与 refresh_token 同时失效，必须重新扫码；
//   - 40188：授权 scope 失效，同样只能重扫；
//
// 其余情况要求消息中同时出现「refresh token 类」词汇与「失效类」词汇才判终态，
// 避免把「access_token 过期」（可自动续期）误判成需要重扫。
//
// 注意：-109 + RC_PARAMS_INVALID 虽然语义上等同于失效码，但它是 refresh 阶段
// 独有的形态，因此由 definitiveCredentialFailure 单独处理，不进这里——
// 本函数两个阶段的错误共用，而 login_buffer 阶段的同码拒绝不能反推旧凭据状态。
var rejectedRefreshCode = regexp.MustCompile(`(?i)(?:^|(?:\berror\b|\bcode\b)\s*[:=]?\s*|\[)(?:40030|42002|42007)(?:$|[\s\]|,;])`)

func definitiveRefreshMessage(code int, raw string) bool {
	// 已知微信业务码本身就是结论：40030 refresh_token 无效、42002 token 失效、
	// 42007 refresh token 过期/失效。官方 check-login 有时只回
	// "error 42007 | 42007" 而不写 refresh_token 字样，靠措辞匹配会漏判成正常。
	// 但这只对「已解析的凭据业务拒绝」成立——HTTP / 代理 / JSON 错误里的
	// 同名字样不算证据，调用方（definitiveCredentialFailure）已经做了类型区分。
	if code == 40030 || code == 42002 || code == 42007 || rejectedRefreshCode.MatchString(raw) {
		return true
	}

	message := strings.ToLower(strings.TrimSpace(raw))
	// 把 refreshtoken / refresh_token / refresh-token 统一成 "refresh token"
	message = strings.NewReplacer("_", " ", "-", " ", "refreshtoken", "refresh token").Replace(message)
	refreshToken := strings.Contains(message, "refresh token") || strings.Contains(message, "刷新令牌")
	if (code == 40188 || strings.Contains(message, "40188")) && strings.Contains(message, "invalid scope") {
		return true
	}
	invalid := strings.Contains(message, "invalid") || strings.Contains(message, "expired") ||
		strings.Contains(message, "expire") || strings.Contains(message, "无效") ||
		strings.Contains(message, "过期") || strings.Contains(message, "失效")
	relogin := strings.Contains(message, "relogin") || strings.Contains(message, "re login") ||
		strings.Contains(message, "重新登录") || strings.Contains(message, "重新授权")
	// 单独的 access token 过期是可恢复的；含糊的 token expired 也继续重试，
	// 直到出现明确的刷新拒绝。「invalid login type」不是失效结论。
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

// refreshCredentialsWithProxy 按代理配置「只续期」，不取票据。
func (a *App) refreshCredentialsWithProxy(ctx context.Context, creds protocol.LoginBufferCredentials, proxyValue string, fallbackDirect bool) (protocol.LoginBufferCredentials, error) {
	if proxyValue == "" {
		return a.refreshCredentials(ctx, creds)
	}
	client, err := qr.NewClientWithProxy(a.cfg.RequestTimeout, proxyValue, fallbackDirect)
	if err != nil {
		return protocol.LoginBufferCredentials{}, err
	}
	return client.LoginBuffers().RefreshCredentials(ctx, creds)
}

// fetchLoginBufferWithProxy 按代理配置取登录票据。proxyValue 为空时走直连客户端。
func (a *App) fetchLoginBufferWithProxy(ctx context.Context, creds protocol.LoginBufferCredentials, proxyValue string, fallbackDirect bool) (string, error) {
	if proxyValue == "" {
		return a.fetchLoginBuffer(ctx, creds)
	}
	client, err := qr.NewClientWithProxy(a.cfg.RequestTimeout, proxyValue, fallbackDirect)
	if err != nil {
		return "", err
	}
	return client.LoginBuffers().FetchLoginBuffer(ctx, creds)
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
	if creds.Provider == "pcmgr" {
		// 这里是原生 key 的下次续期时间，不是 OAuth 有效期。
		// 套用应用宝的提前量会反复触发轮换，把有效期越切越短。
		return now.Unix() >= creds.PCMgr.NextRenewAt()
	}
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

// credentialsNearExpiry 判断账号是否已进入续期窗口（或已经过期）。
//
// 用于决定失败后是走常规退避还是紧急短退避。凭据缺失 / 时间戳异常时按
// 不紧急处理：那种情况重试也救不回来，密集重试只会给上游压力。
func credentialsNearExpiry(acc *store.WechatAccount, now time.Time) bool {
	if acc == nil || acc.Credentials == nil {
		return false
	}
	creds := protocol.CredentialsFromMap(acc.Credentials)
	if creds.ExpiresAt <= 0 {
		return false
	}
	return creds.ExpiresAt-now.Unix() <= int64(keepAliveUrgentWindow.Seconds())
}

// isPCMgrAccount 判断账号是否走腾讯电脑管家渠道。
//
// 管家没有"到期前 N 分钟"的概念，它的时间轴是「下次续期」，所以不能套用
// 应用宝那套提前量与紧急退避。
func isPCMgrAccount(acc *store.WechatAccount) bool {
	if acc == nil || acc.Credentials == nil {
		return false
	}
	return protocol.CredentialsFromMap(acc.Credentials).Provider == "pcmgr"
}
