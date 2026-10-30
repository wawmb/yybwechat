package protocol

import (
	"context"
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

// nativeFallbackAllowed 判断原生 AutoAuth 恢复失败后，还能不能回落到
// 「用现有授权取新票据 + 隔离会话验证」这条路。
//
// 为什么要回落：原生材料来自历史 ManualAuth，可能已经被微信侧作废。
// 真实现场见过原生 AutoAuth 业务层返回 -13（材料还在、但上游不认），
// 此时若把它当终局，账号就会永久卡在「每次窗口开启只换来一次 -13」。
//
// 唯一的例外是 session_only：它的 OAuth 已被明确拒绝，取新票据必然失败，
// 回落只会白跑一趟并放大拒绝，所以这种账号只认原生恢复。
func nativeFallbackAllowed(acc *store.WechatAccount) bool {
	return acc != nil && !acc.SessionOnly()
}

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
// 两条路，按顺序：
//  1. 有独立原生恢复材料（ManualAuth 保存的设备 + AutoKey + EncKey）→ 先试它；
//  2. 原生材料不存在、或者存在但被上游拒绝（业务 -13 / 应用 -14）→
//     用现有授权取一张新票据，在**隔离的候选会话**里先验证同一目标能否取到码，
//     验证通过才原子替换缓存。session_only 账号没有 OAuth 兜底，只走第 1 条。
//
// 无论哪条都不轮换 OAuth 凭据、不改扫码时间、不重放手机号或数据业务。
func (p *Pool) RecoverCodeSession(ctx context.Context, acc *store.WechatAccount, appID, proxy string, fallback bool, original error, fetch func(context.Context) (string, error)) (map[string]any, error) {
	return p.recoverCodeSession(ctx, acc, proxy, original, func(ctx context.Context) (WmpfSession, map[string]any, error) {
		var nativeErr error
		if saved, err := p.nativeRecoveryState(ctx, acc.ID, proxy); err == nil {
			st, restoreErr := p.restoreNativeSession(ctx, acc, saved)
			if restoreErr != nil {
				nativeErr = restoreErr
			} else if result, codeErr := p.requestLoginCode(ctx, st, appID, acc.ID); codeErr == nil {
				return st, result, nil
			} else {
				nativeErr = codeErr
			}
			// 原生材料在、但上游不认：这个账号仍然有可用的 OAuth 授权，
			// 继续走「取新票据」而不是把它当成终局——否则账号会永久卡在
			// 「每 5 分钟只换来一次 -13」。session_only 没有兜底，直接报错。
			if !nativeFallbackAllowed(acc) {
				return WmpfSession{}, nil, nativeErr
			}
			p.events.Emit("protocol.native_fallback", eventlog.ForAccount(acc))
		}
		if acc.SessionOnly() {
			return WmpfSession{}, nil, ErrCachedSessionUnavailable
		}
		p.events.Emit("protocol.ticket_requested", eventlog.ForAccount(acc))
		buffer, err := fetch(ctx)
		if err != nil {
			return WmpfSession{}, nil, fmt.Errorf("fetch fresh code ticket: %w", err)
		}
		if err := acquire(ctx, p.loginSem); err != nil {
			return WmpfSession{}, nil, err
		}
		defer release(p.loginSem)
		st, err := p.loginAndSession(ctx, buffer, proxy, fallback)
		if err != nil {
			return WmpfSession{}, nil, err
		}
		result, err := p.requestLoginCode(ctx, st, appID, acc.ID)
		return st, result, err
	})
}

func (p *Pool) recoverCodeSession(ctx context.Context, acc *store.WechatAccount, proxy string, original error, probe func(context.Context) (WmpfSession, map[string]any, error)) (map[string]any, error) {
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
	p.mu.Lock()
	previous, found := p.codeRepairs[lockKey]
	allowed := !found || !previous.Account.SameAuthorization(current) || time.Since(previous.At) >= codeRepairCooldown
	if allowed {
		p.codeRepairs[lockKey] = codeRepairAttempt{Account: current, At: time.Now()}
	}
	p.mu.Unlock()
	if !allowed {
		p.events.Emit("protocol.recovery_deferred", eventlog.ForAccount(current))
		return nil, original
	}
	p.events.Emit("protocol.recovery_started", eventlog.ForAccount(current))
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
	p.events.Emit("protocol.recovered", eventlog.ForAccount(current))
	// 恢复成功就把退避清零：冷却的本意是「别把一次故障放大成连续重建」，
	// 而不是「成功后还要压着 5 分钟不让再用」。如果新会话下一次又被拒，
	// 立刻再修一次才是对的——否则退化成「每 5 分钟只能取一次码」。
	p.clearCodeRepair(lockKey)
	return result, nil
}

// clearCodeRepair 解除某个「账号 + 出口」的恢复退避。
func (p *Pool) clearCodeRepair(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.codeRepairs, key)
}
