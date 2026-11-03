package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"yybwechat/internal/eventlog"
	"yybwechat/internal/store"
)

// CodeSessionRecoveryError 表示「取码会话恢复」这一层已经试过并失败。
//
// 它必须**阻止**后续进入 OAuth 凭据恢复：最初的 -14 只说明这一次请求没被受理，
// 并不构成「账号凭据已过期」的证据。如果任由它继续往下走，就会把一次
// 应用层瞬时拒绝升级成整轮重新登录，反而更容易触发上游撤销授权。
type CodeSessionRecoveryError struct{ Cause error }

func (e *CodeSessionRecoveryError) Error() string { return e.Cause.Error() }
func (e *CodeSessionRecoveryError) Unwrap() error { return e.Cause }

// CodeCredentialRecoveryError 表示「取新票据这一步被明确拒绝」，而不是
// 从应用层或原生错误里推断出来的凭据过期。
//
// 有它才能放行一次凭据刷新：票据接口自己说 access 被拒（-101 / 40014 / 42001）
// 是上游的明确表态，与「这次请求没被受理」的 -14 完全不同。
type CodeCredentialRecoveryError struct{ Cause error }

func (e *CodeCredentialRecoveryError) Error() string { return e.Cause.Error() }
func (e *CodeCredentialRecoveryError) Unwrap() error { return e.Cause }

// freshSessionWindow 是「刚在这条会话上成功过」的判定窗口。
//
// 会话不可能在几秒内失效，所以窗口内的 -14 只可能是这个 AppID 自身被拒。
// 此时再做一次 ManualAuth 帮不上忙，只会多出几次登录让上游记账——
// 实测一次整点批量里，同一账号曾在 4 秒内连续恢复 3 次。
const freshSessionWindow = 2 * time.Minute

// repairRegistrationHook 在登记一次恢复尝试之前触发，测试用它在那个精确时刻取消请求。
var repairRegistrationHook func()

// verifiedSession 记录某个账号在某条出口上的会话最后一次成功取到 code 的时刻。
//
// Fingerprint 标识「就是这条会话」：缓存被替换或作废之后，旧的成功记录不再算数。
// 只按账号+路径记的话，一次重建出来的新会话会白白继承旧会话的「已验证」身份。
type verifiedSession struct {
	Account     *store.WechatAccount
	At          time.Time
	Fingerprint string
}

// sessionFingerprint 用 SHA-256 摘要标识一条会话（会话密钥 + PSK 票据）。
//
// 每段先写长度再写内容，避免不同切分拼出同一个字节流而误判为同一条会话。
// 只保留摘要，不保留密钥本身。
func sessionFingerprint(st WmpfSession) string {
	h := sha256.New()
	for _, part := range [][]byte{st.Session.SendKey, st.Session.RecvKey, []byte(st.PSK.PreSharedKey), []byte(st.PSK.TicketEntry)} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sessionKey(accountID int64, proxy string) string {
	return fmt.Sprintf("%d|%s", accountID, proxy)
}

func firstAccount(expected []*store.WechatAccount) *store.WechatAccount {
	if len(expected) == 0 {
		return nil
	}
	return expected[0]
}

// markSessionVerified 在某条会话上取到非空 code 之后记录「它就是刚被验证过的会话」。
//
// acc 可能为 nil（调用方没传授权快照）；路由取会话自己携带的缓存路径，
// 代理失败回退直连之后它与配置里的路径并不相同。
func (p *Pool) markSessionVerified(acc *store.WechatAccount, accountID int64, st WmpfSession) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range p.verified {
		if now.Sub(v.At) >= freshSessionWindow {
			delete(p.verified, k)
		}
	}
	p.verified[sessionKey(accountID, st.TCPProxy)] = verifiedSession{
		Account:     acc,
		At:          now,
		Fingerprint: sessionFingerprint(st),
	}
}

// sessionRecentlyVerified 判断「当前缓存里的这条会话」在窗口内是否刚取到过 code。
//
// 三个条件缺一不可：记录未过期、授权没换过、指纹与此刻缓存里的会话一致。
// 缓存被替换或作废之后指纹就对不上，新会话不会继承旧成功记录。
func (p *Pool) sessionRecentlyVerified(ctx context.Context, current *store.WechatAccount, proxy string) bool {
	p.mu.Lock()
	v, ok := p.verified[sessionKey(current.ID, proxy)]
	p.mu.Unlock()
	if !ok || time.Since(v.At) >= freshSessionWindow {
		return false
	}
	if v.Account != nil && !v.Account.SameAuthorization(current) {
		return false
	}
	cached, err := p.cachedSession(ctx, current.ID, proxy)
	return err == nil && sessionFingerprint(cached) == v.Fingerprint
}

// codeRepairAttempt 记录一次恢复尝试，用于同账号+出口的冷却。
type codeRepairAttempt struct {
	Account *store.WechatAccount
	At      time.Time
}

// codeRepairCooldown 是同一「账号 + 出口」两次取码恢复之间的最小间隔。
// 卡住的旧会话会让每次调用都失败，不设冷却就会把一次故障放大成持续重登录。
//
// 注意作用域：它限制的是「重建会话」这个动作，不是取码本身。会话健康时
// 连打多少次取码都不会碰到它；只有已经失败、需要重建时才会撞上。
const codeRepairCooldown = 5 * time.Minute

func (p *Pool) requestLoginCode(ctx context.Context, st WmpfSession, appID string, accountID int64) (map[string]any, error) {
	host := st.Session.HostAppID
	if len(host) == 0 {
		host = hostAppIDDefault
	}
	// 沿用本项目的稳定设备标识：同一账号历次请求必须复用同一个 device，
	// 否则微信侧会把每次调用当成新设备，会话更容易被判为异常。
	// 这是本项目自有的优化，与上游「复用同一设备」的目标一致，保留。
	sessDevice, err := p.ensureSessDevice(ctx, accountID)
	if err != nil {
		return nil, err
	}
	plain := buildJSAPIPlaintext(st.Session.UIN, appID, jsLoginURL, jsLoginCmdID, nil, host, sessDevice)
	envelope, err := buildTransferPacket(st.Session, plain)
	if err != nil {
		return nil, err
	}
	code, _, err := p.sendEnvelope(ctx, st, envelope)
	if err != nil {
		return nil, err
	}
	if len(code) == 0 {
		return nil, ErrLoginCodeUnavailable
	}
	return map[string]any{"code": string(code), "errMsg": "login:ok"}, nil
}

// RecoverCodeSession 在取码被应用层 -14 拒绝时恢复会话。
//
// 两条路，按账号状态分流：
//  1. 授权仍然有效的账号 → **保存票据优先**：先拿现有的 login_buffer 直接登录，
//     只有 ManualAuth 明确回 -10（票确实不能用了）才去取一张新票据。
//     早期是「先试原生 AutoAuth」，而原生材料往往是历史 ManualAuth 留下的、
//     微信侧早已不认，一次 -13 就把本来能用的保存票据路径也堵掉了。
//  2. session_only 账号（授权已被明确拒绝）→ 只认独立的原生恢复材料，
//     因为它不需要 OAuth，而取新票据必然失败。
//
// 两条路都要先在**隔离的候选会话**里验证同一目标能取到非空 code，才原子替换缓存。
// 都不轮换 OAuth 凭据、不改扫码时间、不重放手机号或数据业务。
func (p *Pool) RecoverCodeSession(ctx context.Context, acc *store.WechatAccount, appID, proxy string, fallback bool, original error, fetch func(context.Context) (string, error)) (map[string]any, error) {
	// 恢复也会真正走一次 ManualAuth，设备标识必须与正常登录一致，
	// 否则「修一次会话就换一台设备」，等于把稳定标识的努力抵消掉。
	device := p.deviceFor(ctx, acc)
	return p.recoverCodeSession(ctx, acc, appID, proxy, original, func(ctx context.Context) (WmpfSession, map[string]any, error) {
		if acc.SessionOnly() {
			saved, err := p.nativeRecoveryState(ctx, acc.ID, proxy)
			if err != nil {
				return WmpfSession{}, nil, ErrCachedSessionUnavailable
			}
			st, err := p.restoreNativeSession(ctx, acc, saved)
			if err != nil {
				return WmpfSession{}, nil, err
			}
			result, err := p.requestLoginCode(ctx, st, appID, acc.ID)
			return st, result, err
		}
		return recoverCodeWithTickets(ctx, acc.LoginBuffer, func(ctx context.Context, buffer string) (WmpfSession, error) {
			if err := acquire(ctx, p.loginSem); err != nil {
				return WmpfSession{}, err
			}
			defer release(p.loginSem)
			return p.loginAndSession(ctx, buffer, proxy, fallback, device)
		}, func(ctx context.Context) (string, error) {
			p.events.Emit("protocol.ticket_requested", eventlog.ForAccount(acc).WithTarget(appID, eventlog.OpGetCode, ""))
			return fetch(ctx)
		}, func(ctx context.Context, st WmpfSession) (map[string]any, error) {
			return p.requestLoginCode(ctx, st, appID, acc.ID)
		})
	})
}

// recoverCodeWithTickets 先试保存票据，只在 ManualAuth -10 时才取新票据。
//
// 关键在于**只有 -10 才换票**：通用票据拒绝、网络失败、空 code 以及
// 「仅目标应用被拒」都不能据此认为票据作废。把那些也当成换票理由，
// 会把一次目标级拒绝放大成整轮重新登录。
func recoverCodeWithTickets(ctx context.Context, saved string, login func(context.Context, string) (WmpfSession, error), fetch func(context.Context) (string, error), request func(context.Context, WmpfSession) (map[string]any, error)) (WmpfSession, map[string]any, error) {
	st, err := login(ctx, saved)
	if err == nil {
		result, err := request(ctx, st)
		return st, result, err
	}
	var rejected *ManualAuthRejectedError
	if ctx.Err() != nil || !errors.As(err, &rejected) || rejected.Code != -10 {
		return WmpfSession{}, nil, err
	}
	savedErr := err
	buffer, err := fetch(ctx)
	if err != nil {
		// 票据接口自己说 access 被拒，才算「凭据层面」的明确表态，
		// 才允许上层放行一次凭据刷新。
		var access *LoginBufferRejectedError
		if errors.As(err, &access) && (access.Code == -101 || access.Code == 40014 || access.Code == 42001) {
			err = &CodeCredentialRecoveryError{Cause: err}
		}
		return WmpfSession{}, nil, errors.Join(savedErr, fmt.Errorf("fetch fresh code ticket: %w", err))
	}
	st, err = login(ctx, buffer)
	if err != nil {
		return WmpfSession{}, nil, errors.Join(savedErr, fmt.Errorf("fresh ticket login: %w", err))
	}
	result, err := request(ctx, st)
	if err != nil {
		return st, nil, errors.Join(savedErr, fmt.Errorf("fresh ticket code: %w", err))
	}
	return st, result, nil
}

func (p *Pool) recoverCodeSession(ctx context.Context, acc *store.WechatAccount, appID, proxy string, original error, probe func(context.Context) (WmpfSession, map[string]any, error)) (map[string]any, error) {
	fail := func(err error) (map[string]any, error) {
		return nil, &CodeSessionRecoveryError{Cause: errors.Join(original, err)}
	}
	if !IsApplicationRequestRejection(original) || acc == nil || acc.SessionOnly() && !p.hasNativeRecovery(ctx, acc.ID, proxy) {
		return nil, original
	}
	// 本项目用带引用计数的 acquireScoped 管理锁（表项可回收），
	// 语义与上游的 codeLockFor / lockFor 一致，这里换成对应写法。
	codeUnlock, err := acquireScoped(ctx, &p.mu, p.codeLocks, acc.ID, func() *keyLock {
		return &keyLock{ch: make(chan struct{}, 1)}
	})
	if err != nil {
		return fail(err)
	}
	defer codeUnlock()
	lockKey := fmt.Sprintf("%d\x00%s", acc.ID, proxy)
	stateUnlock, err := acquireScoped(ctx, &p.mu, p.locks, lockKey, func() *keyLock {
		return &keyLock{ch: make(chan struct{}, 1)}
	})
	if err != nil {
		return fail(err)
	}
	defer stateUnlock()
	current, err := p.db.GetAccount(ctx, acc.ID)
	if err != nil {
		return fail(err)
	}
	if !current.SameAuthorization(acc) {
		return nil, store.ErrAccountAuthorizationChanged
	}
	if current.SessionOnly() && !p.hasNativeRecovery(ctx, current.ID, proxy) || current.Status != nil && *current.Status == "expired" {
		return nil, original
	}
	verified := p.sessionRecentlyVerified(ctx, current, proxy)
	if err := ctx.Err(); err != nil {
		// 已取消的请求不该登记一次它永远跑不了的恢复尝试与退避。
		// 这里也覆盖「上面的会话查询因为取消而失败」的情况。
		return fail(err)
	}
	if verified {
		// 这条会话刚刚取到过 code，所以这个 -14 更可能是该 AppID 自己被拒。
		// 再做一次 ManualAuth 和换票都帮不上忙，只会多出几次登录。
		p.events.Emit("protocol.recovery_skipped", eventlog.ForAccount(current).WithTarget(appID, eventlog.OpGetCode, ""))
		return nil, original
	}
	if hook := repairRegistrationHook; hook != nil {
		hook()
	}
	// 冷却按「账号 + 出口 + AppID」计：换个目标取码是另一件事，
	// 不该被上一个目标留下的退避拦住。
	key := fmt.Sprintf("%d\x00%s\x00%s", acc.ID, proxy, appID)
	p.mu.Lock()
	// 临界区里再检查一次取消：等锁期间被取消的请求同样不能留下 5 分钟退避。
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return fail(err)
	}
	// 顺手清理过期条目：目标是调用方给的，脚本遍历任意 AppID 时这张表
	// 只增不删，长期运行会攒出成千上万条再也用不到的记录。
	for k, attempt := range p.codeRepairs {
		if time.Since(attempt.At) >= codeRepairCooldown {
			delete(p.codeRepairs, k)
		}
	}
	previous, found := p.codeRepairs[key]
	allowed := !found || !previous.Account.SameAuthorization(current) || time.Since(previous.At) >= codeRepairCooldown
	if allowed && !p.AllowLogin(current.ID) {
		// 这个账号的登录预算已经用完。保留它原本的错误，也**不**给这个 AppID
		// 记 5 分钟退避：等窗口滑过去，后续请求仍然可以修好它。
		// 拒绝次数由预算自己的汇总事件上报，一次突发不会变成每请求一行日志。
		p.mu.Unlock()
		return nil, original
	}
	if allowed {
		p.codeRepairs[key] = codeRepairAttempt{Account: current, At: time.Now()}
	}
	p.mu.Unlock()
	if !allowed {
		p.events.Emit("protocol.recovery_deferred", eventlog.ForAccount(current).WithTarget(appID, eventlog.OpGetCode, ""))
		return nil, original
	}
	p.events.Emit("protocol.recovery_started", eventlog.ForAccount(current).WithTarget(appID, eventlog.OpGetCode, ""))
	st, result, err := probe(ctx)
	if err != nil {
		return fail(fmt.Errorf("code session recovery failed: %w", err))
	}
	if code, ok := result["code"].(string); !ok || code == "" {
		return fail(ErrLoginCodeUnavailable)
	}
	// 出口必须与原本一致：否则等于把账号的会话挪到了另一条链路上，
	// 后续调用会混用两个出口，微信侧更容易判定异常。
	if st.TCPProxy != proxy {
		return fail(errors.New("fresh code session route mismatch"))
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if !st.nativePersisted {
		if err := p.db.PutSessionForAccount(saveCtx, current, st.Session.UIN, st.toBlob(), sessionExpiresAt(st, p.cfg.SessionTTL), proxy); err != nil {
			return fail(err)
		}
	} else {
		// 原生路径已经自己保存过密钥，这里只需确认期间授权没被换掉。
		latest, err := p.db.GetAccount(saveCtx, current.ID)
		if err != nil {
			return fail(err)
		}
		if !latest.SameAuthorization(current) || latest.Status != nil && *latest.Status == "expired" {
			return fail(store.ErrAccountAuthorizationChanged)
		}
	}
	p.events.Emit("protocol.recovered", eventlog.ForAccount(current).WithTarget(appID, eventlog.OpGetCode, ""))
	// 新会话已经验证可用，直接记为「刚成功」：随后紧跟的 -14 会被判成
	// 目标级拒绝而不是会话坏了，不再触发第二轮重建。
	p.markSessionVerified(current, current.ID, st)
	// 恢复成功就把退避清零：冷却的本意是「别把一次故障放大成连续重建」，
	// 而不是「成功后还要压着 5 分钟不让再用」。如果新会话下一次又被拒，
	// 立刻再修一次才是对的——否则退化成「每 5 分钟只能取一次码」。
	p.clearCodeRepair(key)
	return result, nil
}

// clearCodeRepair 解除某个「账号 + 出口」的恢复退避。
func (p *Pool) clearCodeRepair(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.codeRepairs, key)
}
