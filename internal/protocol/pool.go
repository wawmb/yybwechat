// Package protocol 提供与微信服务器通信的低层协议实现。
// 包括 MMTLS 握手与加密、ShortLink 短连接请求、
// login_buffer 获取与刷新、HTTP-DNS 解析以及 TCP 代理传输。
package protocol

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"yybwechat/internal/eventlog"
	"yybwechat/internal/store"
)

// ErrLoginCodeUnavailable 表示应用返回里没有 code。
//
// 它不等于凭据失效：同一份会话下别的 AppID 可能正常取到 code（上游用隔离
// 实验确认过京东 PT 的 AppID 返回空 code，而控制小程序仍能取到 32 字符 code）。
// 所以遇到它只报错，不重建会话、不清凭据——否则一个应用的空结果会把
// 同账号其他应用正在用的会话一起销毁。
var ErrLoginCodeUnavailable = errors.New("wx.login returned no code; check the application's AppID or authorization")

// ErrCachedSessionUnavailable 表示「授权已被拒绝，且没有可复用的协议会话」。
//
// 用于 session_only 账号：凭据层已经明确说不行了，同时本地也没留下能独立恢复的
// 原生材料，此时唯一的出路就是重新扫码。它与 ErrLoginCodeUnavailable 的差别是
// 后者只说明某个 AppID 取不到 code，会话本身可能还是好的。
var ErrCachedSessionUnavailable = errors.New("authorization rejected and no reusable protocol session; re-scan required")

// Config 是协议连接池的配置参数。
type Config struct {
	ShortlinkTimeout        time.Duration
	LoginTimeout            time.Duration
	SessionTTL              time.Duration
	DNSCacheTTL             time.Duration
	MaxShortlinkConcurrency int
	MaxLoginConcurrency     int
	TCPProxy                string
	TCPProxyFallbackDirect  bool
	// DeviceIdentity 取 DeviceIdentityStable（每个账号固定一套设备）或
	// DeviceIdentityRandom（历史行为：每次登录/请求都换新设备）。
	DeviceIdentity string
	// LoginBudget 限制「业务请求能让 OAuth 层干活」的次数：取新票据、
	// 用票据做 ManualAuth、强制刷新都算。窗口内用完就拒绝，请求保留它原本的错误。
	// 0 表示关闭（DefaultConfig 就是 0，由服务层传入默认值）。
	LoginBudget int
	LoginWindow time.Duration
}

// DefaultConfig 返回协议连接池的默认配置。
func DefaultConfig() Config {
	return Config{
		ShortlinkTimeout:        8 * time.Second,
		LoginTimeout:            30 * time.Second,
		SessionTTL:              30 * time.Minute,
		DNSCacheTTL:             30 * time.Minute,
		MaxShortlinkConcurrency: 1000,
		MaxLoginConcurrency:     32,
		TCPProxyFallbackDirect:  true,
		DeviceIdentity:          DeviceIdentityStable,
	}
}

// WmpfSession 表示一个已建立的微信小程序框架（WMPF）会话，
// 包含 MMTLS 会话密钥、PSK 票据和短连接目标地址。
type WmpfSession struct {
	Session          AppSession
	PSK              pskEntry
	ShortlinkTargets []Target
	CreatedAt        time.Time
	TCPProxy         string
	// FallbackDirect 记录建立该会话时是否允许代理失败后降级直连。
	// 必须随会话一起持久化，否则换代理后短连接的重试策略会和登录时不一致。
	FallbackDirect bool

	// AutoAuth 是 ManualAuth 时服务端一并下发的独立原生恢复材料
	// （设备标识、AutoKey、EncKey）。有了它，PSK 失效或取码被拒时
	// 不必再走一次完整 OAuth 重新登录，可以直接凭这些材料恢复会话。
	AutoAuth autoAuthState
	// recoveryRow 是从库里读出来的原始会话行，原生替换时当 CAS 令牌用。
	// 它不随 toBlob 持久化，只在本次恢复流程的内存里流转。
	recoveryRow *store.SessionRow
	// nativePersisted 表示本次轮换出的新密钥已经落库。
	// 为true时上层不再重复 PutSession，避免把刚保存的密钥又覆盖掉。
	nativePersisted bool
	// CachedOnly 表示本次会话是「仅原会话可用」状态下拿到的缓存。
	// 为true时上层不再走代理直连回退——那条路会先删会话再重试，
	// 而这恰恰是 session_only 账号最不能做的事。
	CachedOnly bool
}

// Pool 是协议连接池，管理 MMTLS 会话的创建、缓存和复用。
// 通过信号量控制并发登录和短连接数量。
type Pool struct {
	cfg Config
	db  *store.DB

	mu sync.Mutex
	// locks 是按「账号 + 代理」维度的会话建立串行锁。
	// 用 channel 而不是 sync.Mutex，是为了让排队阶段也能被 ctx 取消，
	// 避免一次卡死的登录把后续请求全部拖到超时之后。
	locks map[string]*keyLock
	// codeLocks 让同一账号的 wx.login 串行执行。
	// 一次性 code 在并发消费时会互相作废，不同账号之间仍然可以并行。
	codeLocks map[int64]*keyLock
	// codeRepairs 记录「同账号+出口+AppID」最近一次取码恢复的尝试时间。
	// 失败后 5 分钟内不再重试，避免一次卡住的旧会话把请求放大成持续重登录。
	codeRepairs map[string]codeRepairAttempt
	// verified 记录每条会话最后一次成功取到 code 的时刻：紧接着的 -14
	// 只可能是该 AppID 自身被拒，不该再触发一次重新登录。
	verified map[string]verifiedSession
	// budget 限制业务流量能在窗口内让 OAuth 层干多少次活。
	budget *loginBudget
	// events 是运行日志。可为 nil（例如测试或未启用日志时），
	// eventlog.Logger 的所有方法都对 nil 接收者安全。
	events *eventlog.Logger

	// deviceSeedMu / deviceSeedValue 缓存实例级设备种子（落库，重启不变）。
	deviceSeedMu    sync.Mutex
	deviceSeedValue []byte

	loginSem     chan struct{}
	shortlinkSem chan struct{}
}

// NewPool 创建一个新的协议连接池。
func NewPool(cfg Config, db *store.DB) *Pool {
	def := DefaultConfig()
	if cfg.ShortlinkTimeout == 0 {
		cfg.ShortlinkTimeout = def.ShortlinkTimeout
	}
	if cfg.LoginTimeout == 0 {
		cfg.LoginTimeout = def.LoginTimeout
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = def.SessionTTL
	}
	if cfg.DNSCacheTTL == 0 {
		cfg.DNSCacheTTL = def.DNSCacheTTL
	}
	if cfg.DeviceIdentity == "" {
		cfg.DeviceIdentity = def.DeviceIdentity
	}
	if cfg.MaxShortlinkConcurrency == 0 {
		cfg.MaxShortlinkConcurrency = def.MaxShortlinkConcurrency
	}
	if cfg.MaxLoginConcurrency == 0 {
		cfg.MaxLoginConcurrency = def.MaxLoginConcurrency
	}
	return &Pool{
		cfg:          cfg,
		db:           db,
		locks:        map[string]*keyLock{},
		codeLocks:    map[int64]*keyLock{},
		codeRepairs:  map[string]codeRepairAttempt{},
		verified:     map[string]verifiedSession{},
		budget:       newLoginBudget(cfg.LoginBudget, cfg.LoginWindow),
		loginSem:     make(chan struct{}, cfg.MaxLoginConcurrency),
		shortlinkSem: make(chan struct{}, cfg.MaxShortlinkConcurrency),
	}
}

// SetEventLogger 挂载运行日志。协议层在恢复、轮换、取票据等关键节点落事件，
// 便于后台「运行日志」页复盘；未挂载时所有 Emit 都是空操作。
func (p *Pool) SetEventLogger(l *eventlog.Logger) {
	p.events = l
}

// GetCode 通过 MMTLS 短连接获取小程序的登录 code。
//
// wx.login 返回空 code 通常意味着会话已失效，这里会重建会话并重试一次；
// 其他操作（如云函数调用）可能有副作用，不做自动重放。
func (p *Pool) GetCode(ctx context.Context, loginBuffer, appID string, accountID int64, tcpProxy string, fallbackDirect bool, expected ...*store.WechatAccount) (map[string]any, error) {
	codeUnlock, err := acquireScoped(ctx, &p.mu, p.codeLocks, accountID, func() *keyLock {
		return &keyLock{ch: make(chan struct{}, 1)}
	})
	if err != nil {
		return nil, err
	}
	defer codeUnlock()

	call := func(ctx context.Context, st WmpfSession) (map[string]any, error) {
		sessDevice, err := p.ensureSessDevice(ctx, accountID)
		if err != nil {
			return nil, err
		}
		hostAppID := sessionHostAppID(st.Session.HostAppID)
		plain := buildJSAPIPlaintext(st.Session.UIN, appID, jsLoginURL, jsLoginCmdID, nil, hostAppID, sessDevice)
		envelope, err := buildTransferPacket(st.Session, plain)
		if err != nil {
			return nil, err
		}
		code, _, err := p.sendEnvelope(ctx, st, envelope)
		if err != nil {
			return nil, err
		}
		// 只有拿到非空 code 才算「这条会话被验证过」。空 code 说明这个应用
		// 取不到码，不代表会话可用——拿它当证据会让随后的会话级 -14 被误判成
		// 目标级拒绝，从而跳过本该做的恢复。
		if len(code) > 0 {
			p.markSessionVerified(firstAccount(expected), accountID, st)
		}
		return map[string]any{"code": string(code), "errMsg": "login:ok"}, nil
	}
	result, err := retryLoginCode(ctx, func() (map[string]any, error) {
		return p.run(ctx, loginBuffer, accountID, tcpProxy, fallbackDirect, call, expected...)
	}, func() error {
		if err := p.invalidateExpected(ctx, accountID, tcpProxy, expected...); err != nil {
			return err
		}
		if tcpProxy != "" && fallbackDirect {
			return p.invalidateExpected(ctx, accountID, "", expected...)
		}
		return nil
	})
	return result, err
}

// retryLoginCode 在拿到空 code 或短连接载荷异常时重建会话并重试一次。
func retryLoginCode(ctx context.Context, call func() (map[string]any, error), invalidate func() error) (map[string]any, error) {
	result, err := call()
	emptyCode := err == nil && (result == nil || result["code"] == nil || result["code"] == "")
	if emptyCode {
		// 空 code 是「这个应用拿不到 code」，不是「会话坏了」：
		// 不重建会话、不重试，直接把原因报上去。
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrLoginCodeUnavailable
	}
	// 单独的应用层 -14 不再触发重建（v0.2.50）：它只说明这一次请求没被受理，
	// 会话本身仍然可用。早期一律重建反而把瞬时拒绝放大成整轮重登录。
	if IsApplicationRequestRejection(err) || !errors.Is(err, ErrShortlinkPayload) {
		return result, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// 只有「短连接载荷解析失败」才值得重建一次会话：那说明本地会话确实坏了。
	if resetErr := invalidate(); resetErr != nil {
		return nil, errors.Join(err, fmt.Errorf("reset code session: %w", resetErr))
	}
	firstErr := err
	result, err = call()
	if err != nil {
		return nil, errors.Join(firstErr, fmt.Errorf("code retry after session rebuild failed: %w", err))
	}
	if result == nil || result["code"] == nil || result["code"] == "" {
		return nil, errors.Join(firstErr, fmt.Errorf("code retry after session rebuild: %w", ErrLoginCodeUnavailable))
	}
	return result, nil
}

// GetPhoneNumber 通过 MMTLS 短连接获取用户手机号。
func (p *Pool) GetPhoneNumber(ctx context.Context, loginBuffer, appID string, accountID int64, tcpProxy string, fallbackDirect bool, expected ...*store.WechatAccount) (map[string]any, error) {
	result, err := p.run(ctx, loginBuffer, accountID, tcpProxy, fallbackDirect, func(ctx context.Context, st WmpfSession) (map[string]any, error) {
		sessDevice, err := p.ensureSessDevice(ctx, accountID)
		if err != nil {
			return nil, err
		}
		// 渠道身份必须跟着会话走：电脑管家会话就用管家的 HostAppID，
		// 否则服务端会按错误的产品去校验授权。
		hostAppID := sessionHostAppID(st.Session.HostAppID)
		plain := buildPhoneRequest(st.Session.UIN, appID, hostAppID, sessDevice)
		envelope, err := buildTransferPacket(st.Session, plain)
		if err != nil {
			return nil, err
		}
		code, resp, err := p.sendEnvelope(ctx, st, envelope)
		if err != nil {
			return nil, err
		}
		return parseRawResponse(code, resp), nil
	}, expected...)
	// 取号 / 转发成功**不**算「会话已验证」：它们的响应体可以带业务错误，
	// Go 层无 error 并不代表这条会话能取到 wx.login code。
	return result, err
}

// OperateWXData 通过 MMTLS 短连接代理小程序云函数调用。
func (p *Pool) OperateWXData(ctx context.Context, loginBuffer, appID string, payload map[string]any, accountID int64, tcpProxy string, fallbackDirect bool, expected ...*store.WechatAccount) (map[string]any, error) {
	result, err := p.run(ctx, loginBuffer, accountID, tcpProxy, fallbackDirect, func(ctx context.Context, st WmpfSession) (map[string]any, error) {
		sessDevice, err := p.ensureSessDevice(ctx, accountID)
		if err != nil {
			return nil, err
		}
		plain, err := buildOperateRequest(st.Session.UIN, appID, payload, sessionHostAppID(st.Session.HostAppID), sessDevice)
		if err != nil {
			return nil, err
		}
		envelope, err := buildTransferPacket(st.Session, plain)
		if err != nil {
			return nil, err
		}
		code, resp, err := p.sendEnvelope(ctx, st, envelope)
		if err != nil {
			return nil, err
		}
		return parseRawResponse(code, resp), nil
	}, expected...)
	// 取号 / 转发成功**不**算「会话已验证」：它们的响应体可以带业务错误，
	// Go 层无 error 并不代表这条会话能取到 wx.login code。
	return result, err
}

// Invalidate 删除指定账号在指定代理下的会话。
// 代理地址由调用方解析完成后传入，这里不再回退到全局配置，
// 避免「账号级代理」与「全局代理」两条来源相互覆盖。
func (p *Pool) Invalidate(ctx context.Context, accountID int64, tcpProxy string) error {
	return p.db.InvalidateSession(ctx, accountID, tcpProxy)
}

// ensureSessDevice 返回指定账号的稳定设备标识（MAC 字节序列）。
// 从数据库读取该账号持久化的 device_id；若不存在则生成并保存，保证后续所有请求复用同一个值。
func (p *Pool) ensureSessDevice(ctx context.Context, accountID int64) ([]byte, error) {
	mac, err := p.db.EnsureAccountDeviceID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return []byte(mac), nil
}

// cachedSession 决定能不能复用已落库的会话。
//
// 关键点：本地记录的 expires_at 只代表「当时按 30 分钟 TTL 记下的期限」，
// 不是服务端会话的真实有效期。服务端下发的 PSK 期限往往长得多，
// 拿本地期限判断会把一堆还能用的会话判死，进而触发不必要的重新登录。
//
// 因此判定顺序是：先按正常路径读（未过期）；读不到再「忽略本地期限」读一次，
// 只有 PSK 未到期且密钥、票据都能解析时才复用——旧记录不改写期限，
// 也不因此去走 ManualAuth。
func (p *Pool) cachedSession(ctx context.Context, accountID int64, tcpProxy string) (WmpfSession, error) {
	row, err := p.db.GetSession(ctx, accountID, tcpProxy)
	legacyDeadline := errors.Is(err, sql.ErrNoRows)
	if legacyDeadline {
		row, err = p.db.GetSessionForRecovery(ctx, accountID, tcpProxy)
	}
	if err != nil {
		return WmpfSession{}, err
	}
	st, err := sessionFromBlob(row.SessionBlob)
	if err != nil {
		return WmpfSession{}, err
	}
	if st.PSK.ExpiredAt > 0 && st.PSK.ExpiredAt <= time.Now().Unix() {
		return WmpfSession{}, sql.ErrNoRows
	}
	if legacyDeadline {
		key, keyErr := hex.DecodeString(st.PSK.PreSharedKey)
		ticket, ticketErr := hex.DecodeString(st.PSK.TicketEntry)
		if st.PSK.PSKType != 1 || st.PSK.ExpiredAt <= time.Now().Unix() ||
			keyErr != nil || len(key) == 0 || ticketErr != nil || len(ticket) == 0 || st.TCPProxy != tcpProxy {
			return WmpfSession{}, sql.ErrNoRows
		}
	}
	return st, nil
}

// HasReusableSession 判断「这条出口上还有没有可能用的协议会话」。
//
// 用于凭据刷新被拒时的判定：OAuth 被拒并不等于 ILink 的 PSK 也被撤销，
// 上游实测过同一份会话在 refresh 被拒后仍能取到 32 字符 code。
// 只要缓存里的 PSK 未到期、密钥与票据都能解析，或者还留着独立原生恢复材料，
// 就不该把账号直接判死——那会把一条本来还能用的通道白白关掉。
func (p *Pool) HasReusableSession(ctx context.Context, accountID int64, tcpProxy string) bool {
	st, err := p.cachedSession(ctx, accountID, tcpProxy)
	if err != nil || st.PSK.PSKType != 1 || st.PSK.ExpiredAt <= time.Now().Unix() {
		return p.hasNativeRecovery(ctx, accountID, tcpProxy)
	}
	key, keyErr := hex.DecodeString(st.PSK.PreSharedKey)
	ticket, ticketErr := hex.DecodeString(st.PSK.TicketEntry)
	return keyErr == nil && len(key) > 0 && ticketErr == nil && len(ticket) > 0
}

// invalidateExpected 在给出账号快照时只删「与快照匹配」的会话。
//
// 差别很关键：如果调用方手里的是旧快照（账号刚被更新链接/合并换过身份），
// 按快照删就不会连带删掉新身份刚建立的会话。
func (p *Pool) invalidateExpected(ctx context.Context, accountID int64, proxy string, expected ...*store.WechatAccount) error {
	acc, err := p.db.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if len(expected) > 0 && !acc.SameAuthorization(expected[0]) {
		return store.ErrAccountAuthorizationChanged
	}
	// 过期的 PSK 不代表那套独立轮换的 AutoKey 也该丢掉：
	// 只把 PSK 标记为已到期，保留原生恢复材料，否则等于自断唯一退路。
	if saved, err := p.nativeRecoveryState(ctx, accountID, proxy); err == nil {
		saved.PSK.ExpiredAt = time.Now().Add(-time.Second).Unix()
		_, err = p.db.ReplaceNativeSessionForAccount(ctx, acc, saved.recoveryRow, saved.Session.UIN, saved.toBlob(), saved.PSK.ExpiredAt)
		return err
	}
	if acc.SessionOnly() {
		return ErrCachedSessionUnavailable
	}
	if len(expected) > 0 {
		return p.db.InvalidateSessionForAccount(ctx, expected[0], proxy)
	}
	return p.Invalidate(ctx, accountID, proxy)
}

func (p *Pool) run(ctx context.Context, loginBuffer string, accountID int64, tcpProxy string, fallbackDirect bool, op func(context.Context, WmpfSession) (map[string]any, error), expected ...*store.WechatAccount) (map[string]any, error) {
	st, err := p.state(ctx, loginBuffer, accountID, tcpProxy, fallbackDirect, expected...)
	if err == nil {
		st.FallbackDirect = fallbackDirect
		res, err := op(ctx, st)
		// 单独的应用层 -14 与 session_only 缓存都不走直连回退：
		// 回退会先删掉会话再重试，而这两种情况会话恰恰还要留着。
		if err == nil || tcpProxy == "" || !fallbackDirect || IsApplicationRequestRejection(err) || st.CachedOnly {
			return res, err
		}
		// 代理链路失败，丢掉该代理下的会话后改走直连重试一次
		_ = p.invalidateExpected(ctx, accountID, tcpProxy, expected...)
	}
	// 恢复失败已经是「试过一轮」的结论，再走直连只会用另一条路重复同一件错事。
	var recovery *CodeSessionRecoveryError
	if errors.As(err, &recovery) {
		return nil, err
	}
	if tcpProxy != "" && fallbackDirect {
		st, err = p.state(ctx, loginBuffer, accountID, "", false, expected...)
		if err != nil {
			return nil, err
		}
		return op(ctx, st)
	}
	return nil, err
}

func (p *Pool) state(ctx context.Context, loginBuffer string, accountID int64, tcpProxy string, fallbackDirect bool, expected ...*store.WechatAccount) (WmpfSession, error) {
	// 排队等待也要计入登录预算，而不是只算 socket 阶段
	ctx, cancel := context.WithTimeout(ctx, p.cfg.LoginTimeout)
	defer cancel()
	// 先校验一次快照再读缓存：账号若已被「更新链接/合并/跨渠道重扫」换掉身份，
	// 缓存里那条会话属于旧身份，直接返回等于让旧请求复活旧身份。
	acc, err := p.db.GetAccount(ctx, accountID)
	if err != nil {
		return WmpfSession{}, err
	}
	if len(expected) > 0 && !acc.SameAuthorization(expected[0]) {
		return WmpfSession{}, store.ErrAccountAuthorizationChanged
	}
	if st, err := p.cachedSession(ctx, accountID, tcpProxy); err == nil {
		st.CachedOnly = acc.SessionOnly()
		return st, nil
	} else if acc.SessionOnly() && !p.hasNativeRecovery(ctx, accountID, tcpProxy) {
		// 授权已被拒且没有可独立恢复的原生材料：不建立新会话，直接要求重扫。
		return WmpfSession{}, ErrCachedSessionUnavailable
	} else if !errors.Is(err, sql.ErrNoRows) {
		return WmpfSession{}, err
	}
	lockKey := fmt.Sprintf("%d\x00%s", accountID, tcpProxy)
	unlock, err := acquireScoped(ctx, &p.mu, p.locks, lockKey, func() *keyLock {
		return &keyLock{ch: make(chan struct{}, 1)}
	})
	if err != nil {
		return WmpfSession{}, err
	}
	defer unlock()
	// 排队期间状态可能变了（别人刚替换了授权），拿到锁后必须重新读一次
	if st, err := p.cachedSession(ctx, accountID, tcpProxy); err == nil {
		st.CachedOnly = acc.SessionOnly()
		return st, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return WmpfSession{}, err
	}
	acc, err = p.db.GetAccount(ctx, accountID)
	if err != nil {
		return WmpfSession{}, err
	}
	if len(expected) > 0 && !acc.SameAuthorization(expected[0]) {
		return WmpfSession{}, store.ErrAccountAuthorizationChanged
	}
	// 同账号固定一套设备：ManualAuth 的 AppDeviceId 由此派生，
	// 不再每次登录都换一台「新机器」。禁用时返回空值，登录处回落到随机。
	device := p.deviceFor(ctx, acc)
	// 完整 ManualAuth 之前先试一次独立原生恢复：它不需要 OAuth 凭据，
	// 只用 ManualAuth 时保存的设备 + AutoKey + EncKey，因此即便授权已被拒
	// （session_only）也仍然可能成功。
	if saved, err := p.nativeRecoveryState(ctx, accountID, tcpProxy); err == nil {
		restored, restoreErr := p.restoreNativeSession(ctx, acc, saved)
		if restoreErr != nil {
			return WmpfSession{}, &CodeSessionRecoveryError{Cause: restoreErr}
		}
		restored.CachedOnly = acc.SessionOnly()
		return restored, nil
	}
	if acc.SessionOnly() {
		return WmpfSession{}, ErrCachedSessionUnavailable
	}
	if err = acquire(ctx, p.loginSem); err != nil {
		return WmpfSession{}, err
	}
	defer release(p.loginSem)
	st, err := p.loginAndSession(ctx, loginBuffer, tcpProxy, fallbackDirect, device)
	if err != nil {
		return WmpfSession{}, err
	}
	uin := st.Session.UIN
	expiresAt := sessionExpiresAt(st, p.cfg.SessionTTL)
	blob := st.toBlob()
	// 用带条件的写入：登录过程可能持续数秒，期间账号若被更新链接/合并换过身份，
	// 这条会话属于旧身份，写进去就会让账号声称自己是别人。
	if err = p.db.PutSessionForAccount(ctx, acc, uin, blob, expiresAt, tcpProxy); err != nil {
		return WmpfSession{}, err
	}
	return st, nil
}

// keyLock 是按 key 复用的串行锁。
//
// 表里的条目必须能被回收：动态代理每次提取都会换出口，key 里带着代理地址，
// 只增不删的话长期运行会攒出成千上万条再也用不到的记录。
// refs 记录当前有多少调用方持有这条锁，归零时由 dropScoped 摘除表项。
type keyLock struct {
	ch   chan struct{}
	refs int
}

// acquireScoped 取得 key 对应的串行锁，返回解锁函数。
//
// 与 acquire/release 的区别在于它带引用计数并负责回收表项：
// 拿到锁之前先 +1，释放之后 -1，归零时把条目从表里删掉。
// 排队阶段同样可被 ctx 取消，取消时也会归还引用，不会留下泄漏的计数。
func acquireScoped[K comparable](ctx context.Context, mu *sync.Mutex, table map[K]*keyLock, key K, makeLock func() *keyLock) (func(), error) {
	mu.Lock()
	entry := table[key]
	if entry == nil {
		entry = makeLock()
		table[key] = entry
	}
	entry.refs++
	mu.Unlock()

	select {
	case entry.ch <- struct{}{}:
	case <-ctx.Done():
		dropScoped(mu, table, key, entry)
		return nil, ctx.Err()
	}
	return func() {
		<-entry.ch
		dropScoped(mu, table, key, entry)
	}, nil
}

// dropScoped 归还一次引用，归零时删除表项。
// 删除前确认表里仍是同一条记录，避免误删并发场景下后来新建的锁。
func dropScoped[K comparable](mu *sync.Mutex, table map[K]*keyLock, key K, entry *keyLock) {
	mu.Lock()
	entry.refs--
	if entry.refs <= 0 && table[key] == entry {
		delete(table, key)
	}
	mu.Unlock()
}

func (p *Pool) loginAndSession(ctx context.Context, loginBuffer, tcpProxy string, fallbackDirect bool, device DeviceIdentity) (WmpfSession, error) {
	if loginBuffer == "" {
		return WmpfSession{}, fmt.Errorf("login_buffer is empty")
	}
	// HTTPDNS 只是优化、不是前提。给它一份独立且有限的预算：它会串行试两个入口
	// （域名与备份 IP），若每次都拿满登录预算，两个入口加起来就能突破调用方的
	// 整体截止时间，官方域名回退也就没时间了。
	dnsTimeout := min(p.cfg.ShortlinkTimeout, p.cfg.LoginTimeout/3)
	dnsCtx, cancelDNS := context.WithTimeout(ctx, dnsTimeout)
	// getLonglinkTargets 不会返回错误：HTTPDNS 不可用时它退化为官方域名。
	targets, _ := getLonglinkTargets(dnsCtx, dnsTimeout, p.cfg.DNSCacheTTL)
	cancelDNS()

	targets = longlinkLoginTargets(targets, 6)
	st, err := tryLonglinkCandidates(ctx, targets, p.cfg.LoginTimeout, func(attemptCtx context.Context, target Target) (WmpfSession, error) {
		mc, err := connectMmtls(attemptCtx, target, p.cfg.LoginTimeout, tcpProxy, fallbackDirect)
		if err != nil {
			return WmpfSession{}, err
		}
		defer mc.close()
		return p.doLoginOnConn(mc, loginBuffer, device)
	})
	if err != nil {
		return WmpfSession{}, err
	}
	st.ShortlinkTargets = getShortlinkTargets(ctx, p.cfg.ShortlinkTimeout, p.cfg.DNSCacheTTL)
	st.TCPProxy = tcpProxy
	st.FallbackDirect = fallbackDirect
	st.CreatedAt = time.Now()
	return st, nil
}

// tryLonglinkCandidates 依次尝试 LongLink 候选，直到成功或预算耗尽。
//
// 两件事在这里做：
//   - 把剩余预算均分给剩余候选。静默端点（连得上但不响应）否则会独吞整份预算，
//     后面几个候选连尝试的机会都没有——包括列表末尾那个官方域名回退位；
//   - 汇总每一个已尝试端点的失败原因，而不是只留最后一条。排查时最需要知道的
//     是"哪个端点、哪个阶段挂了"（建连 vs 握手 vs 登录），只留一条等于没有线索。
func tryLonglinkCandidates(ctx context.Context, targets []Target, timeout time.Duration, login func(context.Context, Target) (WmpfSession, error)) (WmpfSession, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var failures []error
	for i, t := range targets {
		if err := ctx.Err(); err != nil {
			return WmpfSession{}, fmt.Errorf("LongLink login cancelled after %d/%d candidates: %w",
				i, len(targets), errors.Join(append(failures, err)...))
		}
		deadline, _ := ctx.Deadline()
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(targets)-i))
		st, err := login(attemptCtx, t)
		cancelAttempt()
		if err == nil {
			return st, nil
		}
		failures = append(failures, fmt.Errorf("candidate %d/%d %s: %w",
			i+1, len(targets), net.JoinHostPort(t.IP, strconv.Itoa(t.Port)), err))
		// 端点已经接受了握手、只是拒绝这张票：换 IP 重投同一张票不可能变好，
		// 继续遍历只会把剩余预算烧光，让调用方没时间拿新票。
		var rejected *ManualAuthRejectedError
		if errors.As(err, &rejected) && rejected.Code == -10 {
			return WmpfSession{}, fmt.Errorf("LongLink login rejected: %w", errors.Join(failures...))
		}
	}
	if len(failures) == 0 {
		failures = append(failures, fmt.Errorf("no LongLink candidates"))
	}
	return WmpfSession{}, fmt.Errorf("all LongLink candidates failed: %w", errors.Join(failures...))
}

// doLoginOnConn 在已建立的 MMTLS 连接上完成 ManualAuth 登录握手。
func (p *Pool) doLoginOnConn(mc *mmtlsClient, loginBuffer string, device DeviceIdentity) (WmpfSession, error) {
	meta, err := parseLoginBuffer(loginBuffer)
	if err != nil {
		return WmpfSession{}, err
	}
	// 稳定设备标识开启时用派生的 AppDeviceId；关闭（或种子读不到）时保持
	// 历史行为，每次登录换一个随机值。
	appDeviceID := append([]byte(nil), device.AppDeviceID...)
	if len(appDeviceID) == 0 {
		if appDeviceID, err = randomAppDeviceID(); err != nil {
			return WmpfSession{}, err
		}
	}
	temp := &manualAuthTemp{}
	body, err := buildLoginBody(loginBuffer, meta.DeviceID, appDeviceID, temp)
	if err != nil {
		return WmpfSession{}, err
	}
	if err = mc.sendApp(cmdManualAuth, body); err != nil {
		return WmpfSession{}, err
	}
	resp, err := mc.recvApp()
	if err != nil {
		return WmpfSession{}, err
	}
	if resp.Cmd != cmdManualAuth {
		return WmpfSession{}, fmt.Errorf("manualauth failed: cmd=%d", resp.Cmd)
	}
	mar, err := parseLoginResponse(resp.Body, temp)
	if err != nil {
		return WmpfSession{}, err
	}
	appSess, err := extractSession(mar)
	if err != nil {
		return WmpfSession{}, err
	}
	appSess.DeviceID = meta.DeviceID
	appSess.HostAppID = meta.HostAppID
	// ManualAuth 的响应里除了会话密钥，还带着一套独立原生恢复材料。
	// 一定要在这里存下来：等到 PSK 失效或取码被拒时再想恢复，
	// 凭据可能已经被上游撤销，那时就只剩重新扫码一条路了。
	autoAuth := autoAuthFromResponse(safeBytes(pbParse(mar)[3]), appDeviceID)
	psks, err := mc.extractPSKs()
	if err != nil {
		return WmpfSession{}, err
	}
	psk, ok := pickAccessPSK(psks)
	if !ok {
		return WmpfSession{}, fmt.Errorf("login finished but no access PSK was issued")
	}
	return WmpfSession{
		Session:  appSess,
		AutoAuth: autoAuth,
		PSK:      psk,
	}, nil
}

func (p *Pool) sendEnvelope(ctx context.Context, st WmpfSession, envelope []byte) ([]byte, []byte, error) {
	if err := acquire(ctx, p.shortlinkSem); err != nil {
		return nil, nil, err
	}
	defer release(p.shortlinkSem)
	reqCtx, cancel := context.WithTimeout(ctx, p.cfg.ShortlinkTimeout)
	defer cancel()
	// 是否允许代理失败后降级直连，随会话一起记录，
	// 保证短连接与建立会话时的策略一致。
	return send0RTT(reqCtx, st.ShortlinkTargets, st.PSK, st.Session.RecvKey, envelope, p.cfg.ShortlinkTimeout, st.TCPProxy, st.FallbackDirect)
}

func acquire(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func release(sem chan struct{}) {
	select {
	case <-sem:
	default:
	}
}

func sessionExpiresAt(st WmpfSession, ttl time.Duration) int64 {
	// PSK 是服务端下发的，代表会话真实的可用期限；本地 TTL 只是兜底。
	//
	// 早期实现把到期时间压到 now+30min（`if exp > ttlExp { exp = ttlExp }`），
	// 于是每 30 分钟就把一个仍然可用的会话判成过期、强制重新 ManualAuth。
	// 上游实测：本地缓存到期后原会话照样能完成认证并取到 32 字符 code，
	// 服务端下发的 PSK 期限更长。多出来的登录请求只会增加上游压力。
	if st.PSK.ExpiredAt > 0 {
		return st.PSK.ExpiredAt
	}
	return time.Now().Unix() + int64(ttl.Seconds())
}

func (st WmpfSession) toBlob() map[string]any {
	return map[string]any{
		"session": map[string]any{
			"send_key":   encBytes(st.Session.SendKey),
			"recv_key":   encBytes(st.Session.RecvKey),
			"f9":         encBytes(st.Session.F9),
			"uin":        st.Session.UIN,
			"ticket":     encBytes(st.Session.Ticket),
			"device_id":  encBytes(st.Session.DeviceID),
			"host_appid": encBytes(st.Session.HostAppID),
		},
		"psk_entry": st.PSK,
		"native_autoauth": map[string]any{
			"device": encBytes(st.AutoAuth.Device), "key": encBytes(st.AutoAuth.Key),
			"enc_key": encBytes(st.AutoAuth.EncKey), "attempted_at": st.AutoAuth.AttemptedAt,
		},
		"shortlink_targets": targetsToAny(st.ShortlinkTargets),
		"tcp_proxy":         st.TCPProxy,
		"fallback_direct":   st.FallbackDirect,
	}
}

func sessionFromBlob(blob map[string]any) (WmpfSession, error) {
	sm, ok := blob["session"].(map[string]any)
	if !ok {
		return WmpfSession{}, fmt.Errorf("session_blob missing session")
	}
	pm, ok := blob["psk_entry"].(map[string]any)
	if !ok {
		return WmpfSession{}, fmt.Errorf("session_blob missing psk_entry")
	}
	st := WmpfSession{
		Session: AppSession{
			SendKey:   decBytes(sm["send_key"]),
			RecvKey:   decBytes(sm["recv_key"]),
			F9:        decBytes(sm["f9"]),
			UIN:       int64FromAny(sm["uin"]),
			Ticket:    decBytes(sm["ticket"]),
			DeviceID:  decBytes(sm["device_id"]),
			HostAppID: decBytes(sm["host_appid"]),
		},
		PSK: pskEntry{
			PSKType:      int(int64FromAny(pm["psk_type"])),
			PreSharedKey: stringAny(pm["pre_shared_key"]),
			TicketEntry:  stringAny(pm["ticket_entry"]),
			Lifetime:     int64FromAny(pm["lifetime"]),
			ExpiredAt:    int64FromAny(pm["expired_at"]),
		},
		ShortlinkTargets: targetsFromAny(blob["shortlink_targets"]),
		TCPProxy:         stringAny(blob["tcp_proxy"]),
		FallbackDirect:   blob["fallback_direct"] == true,
		CreatedAt:        time.Now(),
	}
	if am, ok := blob["native_autoauth"].(map[string]any); ok {
		st.AutoAuth = autoAuthState{Device: decBytes(am["device"]), Key: decBytes(am["key"]),
			EncKey: decBytes(am["enc_key"]), AttemptedAt: int64FromAny(am["attempted_at"])}
	}
	if len(st.ShortlinkTargets) == 0 {
		st.ShortlinkTargets = []Target{{IP: "120.241.131.173", Port: 80}}
	}
	if len(st.Session.SendKey) == 0 || len(st.Session.RecvKey) == 0 || st.Session.UIN == 0 {
		return WmpfSession{}, fmt.Errorf("session_blob incomplete")
	}
	return st, nil
}

func encBytes(b []byte) map[string]any {
	if b == nil {
		b = []byte{}
	}
	return map[string]any{"__bytes__": hex.EncodeToString(b)}
}

func decBytes(v any) []byte {
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["__bytes__"].(string); ok {
			b, _ := hex.DecodeString(s)
			return b
		}
	}
	if s, ok := v.(string); ok {
		return []byte(s)
	}
	return nil
}

func targetsToAny(ts []Target) [][]any {
	out := make([][]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, []any{t.IP, t.Port})
	}
	return out
}

func targetsFromAny(v any) []Target {
	var out []Target
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	for _, it := range arr {
		pair, ok := it.([]any)
		if !ok || len(pair) < 2 {
			continue
		}
		out = append(out, Target{IP: stringAny(pair[0]), Port: int(int64FromAny(pair[1]))})
	}
	return out
}

func stringAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func int64FromAny(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	default:
		return 0
	}
}
