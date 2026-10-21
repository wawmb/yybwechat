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
	"sync"
	"time"

	"yybwechat/internal/store"
)

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
		loginSem:     make(chan struct{}, cfg.MaxLoginConcurrency),
		shortlinkSem: make(chan struct{}, cfg.MaxShortlinkConcurrency),
	}
}

// GetCode 通过 MMTLS 短连接获取小程序的登录 code。
//
// wx.login 返回空 code 通常意味着会话已失效，这里会重建会话并重试一次；
// 其他操作（如云函数调用）可能有副作用，不做自动重放。
func (p *Pool) GetCode(ctx context.Context, loginBuffer, appID string, accountID int64, tcpProxy string, fallbackDirect bool) (map[string]any, error) {
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
		hostAppID := st.Session.HostAppID
		if len(hostAppID) == 0 {
			hostAppID = hostAppIDDefault
		}
		plain := buildJSAPIPlaintext(st.Session.UIN, appID, jsLoginURL, jsLoginCmdID, nil, hostAppID, sessDevice)
		envelope, err := buildTransferPacket(st.Session, plain)
		if err != nil {
			return nil, err
		}
		code, _, err := p.sendEnvelope(ctx, st, envelope)
		if err != nil {
			return nil, err
		}
		return map[string]any{"code": string(code), "errMsg": "login:ok"}, nil
	}
	return retryLoginCode(ctx, func() (map[string]any, error) {
		return p.run(ctx, loginBuffer, accountID, tcpProxy, fallbackDirect, call)
	}, func() error {
		if err := p.Invalidate(ctx, accountID, tcpProxy); err != nil {
			return err
		}
		if tcpProxy != "" && fallbackDirect {
			return p.Invalidate(ctx, accountID, "")
		}
		return nil
	})
}

// retryLoginCode 在拿到空 code 或短连接载荷异常时重建会话并重试一次。
func retryLoginCode(ctx context.Context, call func() (map[string]any, error), invalidate func() error) (map[string]any, error) {
	result, err := call()
	emptyCode := err == nil && (result == nil || result["code"] == nil || result["code"] == "")
	if !emptyCode && !errors.Is(err, ErrShortlinkPayload) {
		return result, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if resetErr := invalidate(); resetErr != nil {
		return nil, fmt.Errorf("reset code session: %w", resetErr)
	}
	result, err = call()
	if err != nil {
		return nil, fmt.Errorf("code retry after session rebuild failed: %w", err)
	}
	if result == nil || result["code"] == nil || result["code"] == "" {
		return nil, fmt.Errorf("wx.login returned empty code after session rebuild")
	}
	return result, nil
}

// GetPhoneNumber 通过 MMTLS 短连接获取用户手机号。
func (p *Pool) GetPhoneNumber(ctx context.Context, loginBuffer, appID string, accountID int64, tcpProxy string, fallbackDirect bool) (map[string]any, error) {
	return p.run(ctx, loginBuffer, accountID, tcpProxy, fallbackDirect, func(ctx context.Context, st WmpfSession) (map[string]any, error) {
		sessDevice, err := p.ensureSessDevice(ctx, accountID)
		if err != nil {
			return nil, err
		}
		plain := buildPhoneRequest(st.Session.UIN, appID, sessDevice)
		envelope, err := buildTransferPacket(st.Session, plain)
		if err != nil {
			return nil, err
		}
		code, resp, err := p.sendEnvelope(ctx, st, envelope)
		if err != nil {
			return nil, err
		}
		return parseRawResponse(code, resp), nil
	})
}

// OperateWXData 通过 MMTLS 短连接代理小程序云函数调用。
func (p *Pool) OperateWXData(ctx context.Context, loginBuffer, appID string, payload map[string]any, accountID int64, tcpProxy string, fallbackDirect bool) (map[string]any, error) {
	return p.run(ctx, loginBuffer, accountID, tcpProxy, fallbackDirect, func(ctx context.Context, st WmpfSession) (map[string]any, error) {
		sessDevice, err := p.ensureSessDevice(ctx, accountID)
		if err != nil {
			return nil, err
		}
		plain, err := buildOperateRequest(st.Session.UIN, appID, payload, sessDevice)
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
	})
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

func (p *Pool) run(ctx context.Context, loginBuffer string, accountID int64, tcpProxy string, fallbackDirect bool, op func(context.Context, WmpfSession) (map[string]any, error)) (map[string]any, error) {
	st, err := p.state(ctx, loginBuffer, accountID, tcpProxy, fallbackDirect)
	if err == nil {
		st.FallbackDirect = fallbackDirect
		res, err := op(ctx, st)
		if err == nil || tcpProxy == "" || !fallbackDirect {
			return res, err
		}
		// 代理链路失败，丢掉该代理下的会话后改走直连重试一次
		_ = p.Invalidate(ctx, accountID, tcpProxy)
	}
	if tcpProxy != "" && fallbackDirect {
		st, err = p.state(ctx, loginBuffer, accountID, "", false)
		if err != nil {
			return nil, err
		}
		return op(ctx, st)
	}
	return nil, err
}

func (p *Pool) state(ctx context.Context, loginBuffer string, accountID int64, tcpProxy string, fallbackDirect bool) (WmpfSession, error) {
	// 排队等待也要计入登录预算，而不是只算 socket 阶段
	ctx, cancel := context.WithTimeout(ctx, p.cfg.LoginTimeout)
	defer cancel()
	if row, err := p.db.GetSession(ctx, accountID, tcpProxy); err == nil {
		return sessionFromBlob(row.SessionBlob)
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
	if row, err := p.db.GetSession(ctx, accountID, tcpProxy); err == nil {
		return sessionFromBlob(row.SessionBlob)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return WmpfSession{}, err
	}
	acc, err := p.db.GetAccount(ctx, accountID)
	if err != nil {
		return WmpfSession{}, err
	}
	if err = acquire(ctx, p.loginSem); err != nil {
		return WmpfSession{}, err
	}
	defer release(p.loginSem)
	st, err := p.loginAndSession(ctx, loginBuffer, tcpProxy, fallbackDirect)
	if err != nil {
		return WmpfSession{}, err
	}
	uin := st.Session.UIN
	_ = p.db.SetAccountUIN(ctx, acc.ID, uin)
	expiresAt := sessionExpiresAt(st, p.cfg.SessionTTL)
	blob := st.toBlob()
	if err = p.db.PutSession(ctx, accountID, &uin, blob, expiresAt, tcpProxy); err != nil {
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

func (p *Pool) loginAndSession(ctx context.Context, loginBuffer, tcpProxy string, fallbackDirect bool) (WmpfSession, error) {
	if loginBuffer == "" {
		return WmpfSession{}, fmt.Errorf("login_buffer is empty")
	}
	targets, err := getLonglinkTargets(ctx, p.cfg.LoginTimeout, p.cfg.DNSCacheTTL)
	if err != nil {
		return WmpfSession{}, fmt.Errorf("HTTPDNS LongLink failed: %w", err)
	}
	targets = orderLonglinkTargets(targets, 6)
	var last error
	for _, t := range targets {
		mc, err := connectMmtls(ctx, t, p.cfg.LoginTimeout, tcpProxy, fallbackDirect)
		if err != nil {
			last = err
			continue
		}
		st, err := p.doLoginOnConn(mc, loginBuffer)
		mc.close()
		if err != nil {
			last = err
			continue
		}
		shortTargets := getShortlinkTargets(ctx, p.cfg.ShortlinkTimeout, p.cfg.DNSCacheTTL)
		st.ShortlinkTargets = shortTargets
		st.TCPProxy = tcpProxy
		st.FallbackDirect = fallbackDirect
		st.CreatedAt = time.Now()
		return st, nil
	}
	if last == nil {
		last = fmt.Errorf("no LongLink candidates")
	}
	return WmpfSession{}, fmt.Errorf("all LongLink candidates failed: %w", last)
}

// doLoginOnConn 在已建立的 MMTLS 连接上完成 ManualAuth 登录握手。
func (p *Pool) doLoginOnConn(mc *mmtlsClient, loginBuffer string) (WmpfSession, error) {
	meta, err := parseLoginBuffer(loginBuffer)
	if err != nil {
		return WmpfSession{}, err
	}
	appDeviceID, err := randomAppDeviceID()
	if err != nil {
		return WmpfSession{}, err
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
	psks, err := mc.extractPSKs()
	if err != nil {
		return WmpfSession{}, err
	}
	psk, ok := pickAccessPSK(psks)
	if !ok {
		return WmpfSession{}, fmt.Errorf("login finished but no access PSK was issued")
	}
	return WmpfSession{
		Session: appSess,
		PSK:     psk,
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
	now := time.Now().Unix()
	exp := st.PSK.ExpiredAt
	ttlExp := now + int64(ttl.Seconds())
	if exp == 0 || exp > ttlExp {
		exp = ttlExp
	}
	return exp
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
		"psk_entry":         st.PSK,
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
