package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	httpSwagger "github.com/swaggo/http-swagger/v2"

	"yybwechat/internal/auth"
	"yybwechat/internal/eventlog"
	"yybwechat/internal/pcmgr"
	"yybwechat/internal/protocol"
	"yybwechat/internal/qr"
	"yybwechat/internal/store"
)

// Config 是 App 的配置参数，由 main 函数传入。
type Config struct {
	ResourceRoot string // 模板和静态资源根目录
	DataRoot     string // 持久化数据根目录（db、avatars、qr）
	// EmbeddedWebAssets 为真时使用二进制内嵌的模板与静态资源。
	// 只在「没有显式指定 --resource-root」时为 true：显式指定意味着部署方
	// 自己挂载了一份可写的 resource，那就以他的文件为准。
	EmbeddedWebAssets bool
	DBFilename        string        // SQLite 数据库文件名
	TCPProxy          string        // 可选的 TCP 代理地址
	SessionTTL        time.Duration // 登录会话有效期
	RequestTimeout    time.Duration // 短连接（ShortLink）请求超时
	AvatarTimeout     time.Duration // 下载头像的 HTTP 超时
	QRSessionTTL      time.Duration // 二维码会话过期时间
	CheckInterval     time.Duration // 后台健康检查间隔（0 禁用）
	CheckStale        time.Duration // 超过此时间未检查视为过期，触发刷新

	// ---- 管理员认证 ----
	AuthDBFilename  string        // 认证数据库文件名（位于 data-root/db 下）
	SessionDuration time.Duration // 管理会话有效期，默认 7 天
	CookieSecure    bool          // 是否为会话 Cookie 加 Secure（HTTPS 部署时开启）
	AdminUser       string        // 预设管理员用户名（可选，留空则走 /setup 初始化）
	AdminPassword   string        // 预设管理员口令（可选，需与 AdminUser 同时提供）

	// ---- 协议接口保护 ----
	ProtocolToken string // /wxapp/* 与 GET /accounts 的 Bearer 令牌，留空表示不校验

	// TrustProxy 表示是否信任反向代理传来的 X-Forwarded-For / X-Real-IP。
	// 默认关闭：直连场景下这些头可以任意伪造，用来绕过登录失败限流、
	// 或把会话的登录 IP 写成别人的地址。只有确实部署在可信代理后面才应打开。
	TrustProxy bool

	// ---- 凭据保活 ----
	KeepAliveInterval time.Duration // 保活扫描间隔（0 禁用）
	KeepAliveAhead    time.Duration // 凭据剩余有效期低于此值时提前刷新

	// EnablePCLogin 打开「本机微信授权」通道（quick-login）。
	// Linux 服务器上没用，留空即关闭，接口返回 404。
	EnablePCLogin bool

	// DeviceIdentity 取 stable（每个账号固定一套设备，默认）或 random
	// （历史行为：每次 ManualAuth 换一个新设备）。
	DeviceIdentity string

	// NotifyChannel / NotifyToken / NotifyTopic 配置额外的推送通道
	// （serverchan / pushplus）。企业微信仍走 Webhook 地址，不在这里重复配置。
	// 它们与 Webhook 共用同一套去重、状态跃迁判定与待补推队列。
	NotifyChannel string
	NotifyToken   string
	NotifyTopic   string

	// CodeMinGap 是同一账号两次发往上游的请求之间的最小间隔，0 表示关闭；
	// CodeMaxWait 是请求为等档期最多排队多久，超过就本地拒绝。
	// 数据库目录里的 tuning.json 可以在不重启的情况下覆盖这两个值。
	CodeMinGap  time.Duration
	CodeMaxWait time.Duration

	// LoginBudget 限制窗口内业务流量能让 OAuth 层干多少次活
	// （取新票据、ManualAuth、强制刷新）；0 表示关闭。tuning.json 可在运行时覆盖。
	LoginBudget int
	LoginWindow time.Duration

	// DisableCircuitBreaker 关闭本地取码熔断（目标级 + 会话级）。
	// 关闭后每次调用都会照常打到上游，只在连续失败时按退避重试。
	DisableCircuitBreaker bool

	// ASRefreshMode 决定应用宝凭据走哪个续期入口：
	//   legacy_auth（默认）—— 历史的 `_auth` 入口；
	//   source              —— 按凭据来源的现有入口。
	// 只影响应用宝 / PC YYB 凭据；腾讯电脑管家仍走独立原生续期。
	ASRefreshMode string
}

// 应用宝续期入口的取值。
const (
	ASRefreshLegacy = "legacy_auth"
	ASRefreshSource = "source"
)

// App 是服务的核心实例，聚合了 HTTP 路由、数据库、协议连接池和二维码客户端。
type App struct {
	cfg       Config
	resources resources      // 运行时资源目录
	db        *store.DB      // SQLite 数据库实例
	pool      *protocol.Pool // MMTLS 协议连接池
	qr        *qr.Client     // 微信二维码登录客户端
	// refreshCredentials / fetchLoginBuffer 是「续期」与「取票据」两个可注入的缝。
	// 拆成两个字段而不写成两个方法，是为了让测试能替换掉真实网络调用，
	// 从而精确验证「先存轮换结果、再取票据」这个顺序。
	refreshCredentials func(context.Context, protocol.LoginBufferCredentials) (protocol.LoginBufferCredentials, error)
	fetchLoginBuffer   func(context.Context, protocol.LoginBufferCredentials) (string, error)
	// checkLogin 是官方的 check-login 校验。legacy_auth 刷新入口把被拒的扫码
	// 授权包成一个含糊的 -109，只有再补一次 check-login 才能拿到明确的结论
	// （42007 → 需重扫），否则账号会永远停在 unknown 反复重试。
	checkLogin func(context.Context, protocol.LoginBufferCredentials) (protocol.LoginBufferCredentials, error)

	// circuit 是取码熔断。纯流量控制：不改账号状态、不清凭据、不碰数据库，
	// 只是把「已确认被上游拒绝」的请求挡在本地，避免重复触发风控。
	circuit *circuitBreaker
	// pacer 给同一账号的上游请求排队。熔断与登录预算都只对「失败」和
	// 「OAuth 调用」起作用，一串**成功**的取码能穿过两者——限速补的就是这一段。
	pacer *accountPacer
	auth  *auth.Store // 管理员认证库（独立 auth.db）

	mu         sync.Mutex
	qrSessions map[string]*qr.Session   // 活跃的二维码会话
	qrClients  map[string]*qr.Client    // 会话 ID → 该会话使用的客户端（可能绑定了账号代理）
	qrProxies  map[string]qrProxyChoice // 会话 ID → 本次扫码选定的登录网络代理（确认后写入新账号）
	// linkSessions 记录哪些二维码会话来自「一次性授权链接」，以及它们绑定的链接。
	// 与 qrSessions 分开存：普通扫码与链接扫码的确认逻辑完全不同（一个建账号、
	// 一个原地替换），混在一个结构里很容易在确认时走错分支。
	linkSessions map[string]*linkQRState
	// qrCancelled 记录最近被主动取消的二维码会话（id → 取消时间）。
	//
	// 「取消」与「过期」对使用者是两件事：前者是自己点的「重新生成」，
	// 后者是二维码自然失效。会话被删掉之后，迟到的 poll 只会看到「不存在」，
	// 前端也就只能提示「已失效」。这里留一个短窗口的标记，让它能如实回答
	// 「已取消」——前端与 OpenAPI 的枚举里本来就有这个状态。
	qrCancelled map[string]time.Time
	// quickSessions 是本机微信授权（quick-login）的临时会话，同样由 mu 保护。
	quickSessions map[string]quickLoginSession

	// accountLinkMu 串行化「检查已有链接 + 新建链接」，
	// 防止两次点击为同一账号生成两条一次性链接。
	accountLinkMu sync.Mutex

	loginLimit loginLimiter // 登录失败限流表

	webhookURL   string        // 当前 Webhook 地址（由 /settings/webhook API 动态读写）
	webhookMu    sync.RWMutex  // 保护 webhookURL 的并发读写
	stopHealth   chan struct{} // 通知后台健康检查 goroutine 退出
	stopKeep     chan struct{} // 通知后台保活 goroutine 退出
	stopLinks    chan struct{} // 通知一次性链接清理 goroutine 退出
	healthClient *http.Client  // Webhook 推送专用的 HTTP 客户端

	// events 是运行日志（固定事件词汇表，从不记录凭据/身份/原始错误）。
	// 为 nil 时所有 Emit 都是空操作，因此测试里可以完全不初始化它。
	events *eventlog.Logger

	// notifyRetryMu 让补推流程串行执行。健康检查与保活是两个独立的 ticker，
	// 同一秒内可能同时进入补推，导致同一条告警重复推送两次。
	notifyRetryMu sync.Mutex

	keepAliveMu    sync.Mutex          // 保护保活重退避状态表
	keepAliveNext  map[int64]time.Time // 账号 ID → 下次可重试时间（失败退避）
	keepAliveFails map[int64]int       // 账号 ID → 连续失败次数（用于递增退避）

	// alertSkipLogged 记录「因去重窗口跳过告警」的日志节流时间，
	// 仅为让这类跳过在日志里可见（每账号每小时最多一行），不参与业务判断。
	// 同样由 keepAliveMu 保护。
	alertSkipLogged map[int64]time.Time

	// lastRefreshErrs 记录每个账号最近一次刷新失败的原因。
	// 掉线后最常被问的就是「为什么」，把原因放进 /accounts 就不用再去翻容器日志。
	// 进程重启即清空，它是诊断信息、不是持久事实。同样由 keepAliveMu 保护。
	lastRefreshErrs map[int64]string

	// credentialRejects 记录「微信侧连续以同一业务码拒绝凭据」的次数。
	// 只有真正连续的同类拒绝才会累加，失败原因一变就归零（见 noteCredentialRejection）。
	// 字段与 keepAliveFails 分开：后者用于退避调度，混入网络抖动，
	// 而这里只认服务端明确给出的拒绝码，不能让代理故障把账号推到终态。
	// 同样由 keepAliveMu 保护。
	credentialRejects map[int64]int

	// ---- 代理运行时状态 ----
	// 动态代理的出口 IP 是短效的，用一份带 TTL 的租约缓存避免高频提取；
	// 同一账号的提取过程串行化，防止并发打出多个出口。
	proxyMu           sync.Mutex
	proxyLeases       map[int64]accountProxyLease // 账号 ID → 当前代理租约
	proxyLeaseLocks   map[int64]chan struct{}     // 账号 ID → 提取串行锁
	proxyLeaseLocksMu sync.Mutex                  // 保护 proxyLeaseLocks

	// refreshLocks 让同一账号的凭据刷新串行执行，
	// 避免保活与健康检查同时刷新导致凭据互相覆盖。
	refreshLocksMu sync.Mutex
	refreshLocks   map[int64]chan struct{}
}

// swaggerDocsHandler 是 Swagger UI 的 HTTP 处理器。
var swaggerDocsHandler = httpSwagger.Handler(
	httpSwagger.URL("/openapi.json"),
	httpSwagger.DocExpansion("list"),
	httpSwagger.DeepLinking(true),
	httpSwagger.DefaultModelsExpandDepth(httpSwagger.ShowModel),
)

// NewApp 创建 App 实例，完成资源目录初始化、数据库连接、
// 协议连接池创建和二维码客户端初始化。
func NewApp(cfg Config) (*App, error) {
	// 设备标识策略必须是明确的两选一：拼错就拒绝启动，
	// 否则会静默退化成某个默认值，而这两种行为的后果完全不同。
	cfg.DeviceIdentity = strings.ToLower(strings.TrimSpace(cfg.DeviceIdentity))
	switch cfg.DeviceIdentity {
	case "", protocol.DeviceIdentityStable:
		cfg.DeviceIdentity = protocol.DeviceIdentityStable
	case protocol.DeviceIdentityRandom:
	default:
		return nil, errors.New("YYB_DEVICE_IDENTITY must be stable or random")
	}
	cfg.NotifyChannel = strings.ToLower(strings.TrimSpace(cfg.NotifyChannel))
	cfg.NotifyToken = strings.TrimSpace(cfg.NotifyToken)
	cfg.NotifyTopic = strings.TrimSpace(cfg.NotifyTopic)
	switch {
	case cfg.NotifyChannel == "" || cfg.NotifyChannel == "none":
		cfg.NotifyChannel = ""
	case cfg.NotifyChannel == "qywx":
		// 企业微信已经有 Webhook 地址这条更完整的通路（支持 markdown 卡片）。
		// 再开一个 key 形态的入口，同一个群会收到两种排版的重复消息。
		return nil, errors.New("YYB_NOTIFY_CHANNEL=qywx 不再支持：企业微信请直接填 Webhook 地址")
	case !validExtraPushChannel(cfg.NotifyChannel):
		return nil, errors.New("YYB_NOTIFY_CHANNEL 只能是 serverchan 或 pushplus")
	case cfg.NotifyToken == "":
		return nil, errors.New("配置 YYB_NOTIFY_CHANNEL 时必须同时提供 YYB_NOTIFY_TOKEN")
	}

	// 续期入口同样必须两选一：拼错就拒绝启动，不静默退化。
	cfg.ASRefreshMode = strings.ToLower(strings.TrimSpace(cfg.ASRefreshMode))
	switch cfg.ASRefreshMode {
	case "", ASRefreshLegacy:
		cfg.ASRefreshMode = ASRefreshLegacy
	case ASRefreshSource:
	default:
		return nil, errors.New("YYB_APPSTORE_REFRESH_MODE must be source or legacy_auth")
	}
	if cfg.ResourceRoot == "" {
		cfg.ResourceRoot = filepath.Join(".", "resource")
	}
	if cfg.DataRoot == "" {
		cfg.DataRoot = cfg.ResourceRoot // 默认数据目录与资源目录相同，向后兼容
	}
	if cfg.DBFilename == "" {
		cfg.DBFilename = DefaultDBFilename
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 8 * time.Second
	}
	if cfg.AvatarTimeout == 0 {
		cfg.AvatarTimeout = 10 * time.Second
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 30 * time.Minute
	}
	if cfg.QRSessionTTL == 0 {
		cfg.QRSessionTTL = 5 * time.Minute
	}
	if cfg.CheckStale == 0 {
		cfg.CheckStale = 3600 * time.Second
	}
	if cfg.SessionDuration == 0 {
		cfg.SessionDuration = 7 * 24 * time.Hour
	}
	if cfg.KeepAliveAhead == 0 {
		cfg.KeepAliveAhead = 45 * time.Minute
	}
	res, err := ensureResources(cfg.ResourceRoot, cfg.DataRoot, cfg.EmbeddedWebAssets)
	if err != nil {
		return nil, err
	}
	dbPath, err := prepareDBPath(res.DB, cfg.DBFilename)
	if err != nil {
		return nil, err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	poolCfg := protocol.DefaultConfig()
	poolCfg.SessionTTL = cfg.SessionTTL
	poolCfg.ShortlinkTimeout = cfg.RequestTimeout
	poolCfg.TCPProxy = cfg.TCPProxy
	poolCfg.DeviceIdentity = cfg.DeviceIdentity
	// 0 表示关闭登录预算，这样没设它的调用方（测试、嵌入方）保持原有行为，
	// 默认值由 main 传进来。
	poolCfg.LoginBudget = cfg.LoginBudget
	poolCfg.LoginWindow = cfg.LoginWindow
	pool := protocol.NewPool(poolCfg, db)
	app := &App{
		cfg:           cfg,
		resources:     res,
		db:            db,
		pool:          pool,
		qr:            qr.NewClient(cfg.RequestTimeout),
		qrSessions:    map[string]*qr.Session{},
		qrCancelled:   map[string]time.Time{},
		qrClients:     map[string]*qr.Client{},
		quickSessions: map[string]quickLoginSession{},
		qrProxies:     map[string]qrProxyChoice{},
		healthClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		keepAliveNext:     map[int64]time.Time{},
		keepAliveFails:    map[int64]int{},
		credentialRejects: map[int64]int{},
		proxyLeases:       map[int64]accountProxyLease{},
		proxyLeaseLocks:   map[int64]chan struct{}{},
		refreshLocks:      map[int64]chan struct{}{},
	}

	// 打开独立的认证数据库。与主库分离，避免影响业务表结构。
	authPath := auth.AuthDBPath(res.DB, cfg.AuthDBFilename)
	authStore, err := auth.Open(authPath)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("打开认证数据库失败: %w", err)
	}
	app.auth = authStore

	// 支持通过环境变量预设管理员，便于容器化无人值守部署
	if cfg.AdminUser != "" && cfg.AdminPassword != "" {
		if has, _ := authStore.HasAdmin(context.Background()); !has {
			if _, err := authStore.CreateAdmin(context.Background(), cfg.AdminUser, cfg.AdminPassword); err != nil {
				log.Printf("[auth] 预设管理员创建失败: %v", err)
			} else {
				log.Printf("[auth] 已根据环境变量预设管理员: %s", cfg.AdminUser)
			}
		}
	}

	// 默认实现走直连客户端；带代理的路径由 *WithProxy 包装决定。
	app.refreshCredentials = app.qr.LoginBuffers().RefreshCredentials
	app.fetchLoginBuffer = app.qr.LoginBuffers().FetchLoginBuffer
	app.checkLogin = app.qr.LoginBuffers().CheckLogin
	app.circuit = newCircuitBreaker(circuitConfigFor(cfg.DisableCircuitBreaker))
	// 流量限制器的配置值；tuning.json 可以在运行时覆盖它们。
	trafficSettings := pacingSettings{
		MinGap:      cfg.CodeMinGap,
		MaxWait:     cfg.CodeMaxWait,
		LoginBudget: cfg.LoginBudget,
		LoginWindow: cfg.LoginWindow,
	}
	app.pacer = newAccountPacer(filepath.Join(res.DB, "tuning.json"), trafficSettings)
	// tuning.json 改登录预算时同步到运行中的连接池，不用重启。
	app.pacer.onChange = func(settings pacingSettings) {
		pool.SetLoginBudget(settings.LoginBudget, settings.LoginWindow)
	}

	// 运行日志。建不起来（比如磁盘只读）只降级为「没有日志」，不阻断启动——
	// 日志是诊断手段，不是服务的前置条件。
	if logger, err := eventlog.New(db); err != nil {
		log.Printf("[eventlog] 运行日志初始化失败，本次运行不记录运行日志: %v", err)
	} else {
		app.events = logger
		pool.SetEventLogger(logger)
		app.events.Emit("system.started", eventlog.Fields{})
	}

	// 从数据库加载 webhook URL
	if url, err := db.GetSetting(context.Background(), "webhook_url"); err == nil && url != "" {
		app.webhookURL = url
		log.Printf("[webhook] 已加载 Webhook 地址: %s", sanitizeURL(url))
	} else if cfg.NotifyChannel == "" {
		// 一个通道都没配时所有告警都会被静默跳过（函数第一行就 return）。
		// 以前这里只在配置成功时打日志，结果「为什么没推送」只能靠猜——
		// 启动时明确说一次，省掉这一整轮排查。
		log.Printf("[notify] 未配置任何推送通道：账号失效/续期失败都不会推送。" +
			"请在控制台「Webhook 地址」填入企业微信机器人地址（或配置 YYB_NOTIFY_CHANNEL）并点「测试」验证")
	} else {
		log.Printf("[notify] 已启用额外推送通道: %s（与 Webhook 共用去重与补推队列）", cfg.NotifyChannel)
	}

	// 启动后台健康检查（若 CheckInterval > 0）
	if cfg.CheckInterval > 0 {
		app.stopHealth = make(chan struct{})
		go app.runHealthChecker()
		log.Printf("[health] 后台健康检查已启动，间隔=%s，过期阈值=%s", cfg.CheckInterval, cfg.CheckStale)
	}

	// 启动凭据保活（若 KeepAliveInterval > 0）
	if cfg.KeepAliveInterval > 0 {
		app.stopKeep = make(chan struct{})
		go app.runKeepAlive()
		// 把两个会影响续期行为的开关一起打出来：排查「为什么续期走的是另一条路」
		// 时第一眼就要能看到它们，而不是回头翻环境变量。
		log.Printf("[keepalive] 凭据保活已启动，间隔=%s，提前量=%s，应用宝续期入口=%s，设备标识=%s",
			cfg.KeepAliveInterval, cfg.KeepAliveAhead, cfg.ASRefreshMode, cfg.DeviceIdentity)
	}

	// 一次性授权链接的清理。与保活/健康检查无关，始终启动：
	// 链接本身在 expires_at 之后即刻失效，这里只负责把过期行删掉。
	app.stopLinks = make(chan struct{})
	go app.runAccountLinkCleanup()

	// 启动即补推一轮。ticker 第一次触发要等满一个间隔（健康检查默认 30 分钟），
	// 而两个定时器都可能被配置成 0 关掉 —— 重启期间已失效的账号、
	// 停机前没送达的告警，都不能靠「等下一个 tick」。
	//
	// 条件是「此刻已配置 Webhook」：地址在 NewApp 开头就从数据库读入，
	// 所以这里判空等价于「这个部署确实要推送」。没配就跳过，避免白跑一趟；
	// 之后由 tick 里的补推接手（运营者补配地址时也能立刻生效）。
	if app.getWebhookURL() != "" {
		go app.pushStartupAlerts()
	}

	if cfg.CheckInterval <= 0 && cfg.KeepAliveInterval <= 0 {
		log.Printf("[notify] 健康检查与凭据保活均已关闭：只有服务启动时与手动操作会检查账号，" +
			"之后账号再失效不会被自动发现，Webhook 也不会有告警")
	}

	// 未初始化管理员时输出提示，引导访问 /setup
	if has, _ := authStore.HasAdmin(context.Background()); !has {
		log.Printf("[auth] %s", auth.FormatSetupHint(authPath))
	}

	return app, nil
}

// Close 关闭 App，停止后台 goroutine 并释放数据库连接等资源。
func (a *App) Close() error {
	if a.stopHealth != nil {
		close(a.stopHealth)
	}
	if a.stopKeep != nil {
		close(a.stopKeep)
	}
	if a.stopLinks != nil {
		close(a.stopLinks)
	}
	// 日志要先于数据库关闭：Close 会把队列里剩下的事件刷盘，
	// 反过来的话最后一批事件会因为库已关闭而全部丢失。
	a.events.Emit("system.stopped", eventlog.Fields{})
	a.events.Close()
	var err error
	if a.db != nil {
		err = a.db.Close()
	}
	if a.auth != nil {
		if aerr := a.auth.Close(); err == nil {
			err = aerr
		}
	}
	return err
}

// Handler 创建并返回配置好所有路由的 http.Handler。
//
// 路由按安全边界划分为三组：
//  1. 公开组：健康检查、静态资源、登录/初始化与认证接口、OpenAPI 文档；
//  2. 协议组：/wxapp/*，供自动化脚本调用，不以浏览器会话鉴权，
//     可选通过 ProtocolToken 做 Bearer 门禁；
//  3. 管理组：首页、扫码、账号管理、设置等，全部要求管理员登录。
//
// 使用 gin.Group 划分而非顺序 Use，避免中间件泄漏到后续分组。
func (a *App) Handler() http.Handler {
	if os.Getenv(gin.EnvGinMode) == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	// logRequests 必须排在 gin.Logger 之前：它读的是本轮请求的最终状态码，
	// 顺序反过来就会记录到中间件的状态。
	router.Use(a.logRequests(), gin.Logger(), gin.Recovery())

	// ---- 1. 公开组 ----
	open := router.Group("/")
	healthHandler := func(c *gin.Context) {
		writeJSON(c.Writer, http.StatusOK, gin.H{"ok": true})
	}
	open.Any("/health", healthHandler)
	// /healthz 是旧版缓存检查脚本使用的路径，保留兼容
	open.Any("/healthz", healthHandler)
	open.StaticFS("/static", http.Dir(a.resources.Static))
	open.Any("/login", gin.WrapF(a.handleLogin))
	open.Any("/setup", gin.WrapF(a.handleSetup))
	open.Any("/api/auth/status", gin.WrapF(a.handleAuthStatus))
	open.Any("/api/auth/setup", gin.WrapF(a.handleAuthSetup))
	open.Any("/api/auth/login", gin.WrapF(a.handleAuthLogin))
	open.Any("/api/auth/logout", gin.WrapF(a.handleAuthLogout))
	open.Any("/api/auth/me", gin.WrapF(a.handleAuthMe))
	open.Any("/openapi.json", gin.WrapF(a.handleOpenAPI))

	// ---- 2. 协议组（可选令牌门禁）----
	// 统一使用 /wxapp/* 命名空间，/wx/oauth 仅作为旧版自动化脚本的兼容别名保留。
	proto := router.Group("/", a.protocolTokenGate())
	proto.Any("/wxapp/getCode", gin.WrapF(a.handleGetCode))
	proto.Any("/wxapp/getPhoneNumber", gin.WrapF(a.handleGetPhoneNumber))
	proto.Any("/wxapp/operateWxData", gin.WrapF(a.handleOperateWXData))
	proto.Any("/wxapp/getUserInfo", gin.WrapF(a.handleGetUserInfo))
	proto.Any("/wxapp/oauth", gin.WrapF(a.handleOAuth))
	// ---- /wx/* 兼容别名 ----
	// 早期脚本与部分第三方客户端按小程序端 API 名拼路径（getCode / getUserInfo /
	// getPhoneNumber / getLatestUserKey ...）。这些入口全部转发到上面同一批处理器，
	// 响应格式、校验规则与错误码完全一致，不会出现「同一能力两种行为」。
	//
	// 必须注册在带 protocolTokenGate 的分组里：一旦挂到公开组，
	// 配置了令牌也拦不住这些能直接驱动账号凭据的接口。
	proto.Any("/wx/oauth", gin.WrapF(a.handleOAuth))
	// 旧客户端的扫码入口：与 /qr 共用处理器，走同一套令牌门禁
	proto.Any("/wx/qrcodeauth", gin.WrapF(a.handleQRRoot))
	proto.Any("/wx/qrcodeauth/*path", gin.WrapF(a.handleQR))
	proto.Any("/wx/code", gin.WrapF(a.handleWXCodeAlias))
	proto.Any("/wx/getuserinfo", gin.WrapF(a.handleWXGetUserInfo))
	proto.Any("/wx/getphonenumber", gin.WrapF(a.handleWXPhoneAlias))
	proto.Any("/wx/encryptkey", gin.WrapF(a.handleWXEncryptKey))
	proto.Any("/wx/getlatestuserkey", gin.WrapF(a.handleWXLatestUserKey))
	proto.Any("/wx/cloud", gin.WrapF(a.handleWXCloud))
	proto.Any("/wx/mpgeta8key", gin.WrapF(a.handleWXMPGetA8Key))
	proto.Any("/wx/appmsgext", gin.WrapF(a.handleWXAppMsgExt))
	proto.Any("/wx/appmsglike", gin.WrapF(a.handleWXAppMsgLike))
	// GET /accounts 免登录但受协议令牌保护：自动化脚本（青龙面板定时任务等）
	// 第一步就是用它探测服务并拉取账号列表，要求脚本先做交互式登录会直接打断
	// 无人值守的调用链，因此这里只要求可选的 Bearer 令牌（未配置时直接放行）。
	// 返回 AccountPublic，剔除了 login_buffer 与 credentials。
	// DELETE 等写操作在 handleAccountsRoot 内部二次鉴权，仍然要求管理员登录。
	proto.Any("/accounts", gin.WrapF(a.handleAccountsRoot))

	// 一次性授权链接。
	//
	// 刻意放在公开组：这个功能的用途就是把链接发给别人（可能是用手机、
	// 但没有管理员口令的人）去扫码。要求登录等于要求对方也拿到口令。
	// 安全性靠三层：一次性消费、短 TTL、更新类链接必须由原微信号扫码。
	open.Any("/account-link/*path", gin.WrapF(a.handleAccountLinkRoute))

	// ---- 3. 管理组（需登录）----
	secure := router.Group("/", a.requireBrowserSession())
	secure.Any("/", gin.WrapF(a.handleIndex))
	secure.Any("/scan", gin.WrapF(a.handleScan))
	secure.GET("/logs", gin.WrapF(a.handleSystemLogsPage))
	secure.GET("/api/system-logs", gin.WrapF(a.handleSystemLogs))
	// 本机微信授权（quick-login）：未开启 PC 登录时内部直接 404
	secure.Any("/quick-login", gin.WrapF(a.handleQuickLoginRoot))
	secure.Any("/quick-login/*path", gin.WrapF(a.handleQuickLogin))
	secure.Any("/docs", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/docs/index.html")
	})
	secure.Any("/docs/*path", gin.WrapF(a.handleDocs))
	secure.Any("/qr", gin.WrapF(a.handleQRRoot))
	secure.Any("/qr/*path", gin.WrapF(a.handleQR))
	secure.Any("/accounts/avatar", gin.WrapF(a.handleAccountAvatar))
	secure.Any("/accounts/refresh", gin.WrapF(a.handleAccountRefresh))
	secure.Any("/accounts/resync", gin.WrapF(a.handleAccountResync))
	secure.Any("/accounts/remark", gin.WrapF(a.handleAccountRemark))
	secure.Any("/accounts/repair", gin.WrapF(a.handleAccountRepair))
	secure.Any("/accounts/compact", gin.WrapF(a.handleAccountCompact))
	secure.Any("/accounts/status", gin.WrapF(a.handleAccountStatus))
	// 合并账号：把误建账号的授权移入原 ID（两步式，先预览再确认）
	secure.Any("/accounts/merge", gin.WrapF(a.handleAccountMerge))
	secure.Any("/settings/webhook", gin.WrapF(a.handleSettingsWebhook))
	secure.Any("/settings/webhook/test", gin.WrapF(a.handleWebhookTest))
	// 一次性授权链接：管理页面 + 它的接口（创建、列出、作废、删除）
	secure.Any("/account-links", gin.WrapF(a.handleAccountLinksPage))
	secure.Any("/api/account-links", gin.WrapF(a.handleAccountLinksAPI))
	secure.Any("/api/account-links/*path", gin.WrapF(a.handleAccountLinksAPI))
	// 系统默认扫码渠道
	secure.Any("/api/scan-settings", gin.WrapF(a.handleScanSettings))
	secure.Any("/api/version", gin.WrapF(a.handleVersion))
	// 修改口令必须挂在需要登录的分组下：处理器依赖中间件把当前管理员写进
	// context，注册到公开组会让 currentUser 永远取到空值，接口恒返回 401。
	secure.Any("/api/auth/password", gin.WrapF(a.handleAuthPassword))

	// ---- 4. 代理管理（需登录）----
	// 账号级代理：读写、连通性测试
	secure.Any("/accounts/proxy", gin.WrapF(a.handleAccountProxy))
	secure.Any("/accounts/proxy/test", gin.WrapF(a.handleAccountProxyTest))
	// 供应商配置与地区列表
	secure.Any("/api/proxy-profiles", gin.WrapF(a.handleProxyProfiles))
	secure.Any("/api/proxy-profiles/*path", gin.WrapF(a.handleProxyProfiles))
	// 按运营者位置推荐地区
	secure.Any("/api/proxy-location/recommend", gin.WrapF(a.handleProxyLocationRecommend))
	// 代理设置页
	secure.Any("/proxies", gin.WrapF(a.handleProxiesPage))

	router.NoRoute(func(c *gin.Context) {
		writeError(c.Writer, http.StatusNotFound, "not found")
	})

	return router
}

// protocolTokenGate 是协议接口的可选 Bearer 令牌门禁。
// 未配置 ProtocolToken 时直接放行（保持内网自托管的易用性）；
// 配置后要求请求携带 Authorization: Bearer <token>，使用常量时间比较防时序攻击。
//
// 已登录的管理员会话同样放行：控制台页面本身就要调 /accounts 和 /wxapp/*
// （账号列表、调用面板），而浏览器只会带会话 Cookie，不可能自己加协议令牌。
// 如果这里只认令牌，配了 --protocol-token 之后控制台会全部 401，
// 而前端把 401 当成「会话过期」直接踢回登录页——等于开着令牌就没法用控制台。
func (a *App) protocolTokenGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if a.cfg.ProtocolToken == "" {
			c.Next()
			return
		}
		// 先试令牌：脚本调用占多数，这一步不需要查库
		token := bearerToken(c.Request)
		if subtle.ConstantTimeCompare([]byte(token), []byte(a.cfg.ProtocolToken)) == 1 {
			c.Next()
			return
		}
		// 再退回到管理员会话，供浏览器端使用
		if a.isAuthenticated(c.Request) {
			c.Next()
			return
		}
		writeError(c.Writer, http.StatusUnauthorized, "invalid or missing protocol token")
		c.Abort()
	}
}

// bearerToken 从 Authorization 头或 query 参数中提取 Bearer 令牌。
func bearerToken(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if v == "" {
		v = r.URL.Query().Get("access_token")
	}
	if !strings.HasPrefix(strings.ToLower(v), "bearer ") {
		return ""
	}
	return strings.TrimSpace(v[len("bearer "):])
}

// handleVersion 返回服务构建元数据，供控制台顶栏展示。

// handleIndex 处理首页请求，返回 HTML 控制台页面。
func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	serveFileOrText(w, r, filepath.Join(a.resources.Templates, "index.html"), fallbackIndexHTML)
}

// handleScan 处理扫码页面请求，返回扫码交互 HTML 页面。
func (a *App) handleScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	serveFileOrText(w, r, filepath.Join(a.resources.Templates, "scan.html"), fallbackScanHTML)
}

// handleProxiesPage 处理代理设置页请求，返回供应商与账号代理的配置界面。
func (a *App) handleProxiesPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	serveFileOrText(w, r, filepath.Join(a.resources.Templates, "proxies.html"), fallbackProxiesHTML)
}

// handleAccountLinksPage 处理「已激活短链接」管理页。
//
// 之前侧栏有这个入口、接口也在（/api/account-links），但页面路由和模板都漏了，
// 点进去是 404 —— 功能其实全都能用，只是没有界面。这里补齐。
func (a *App) handleAccountLinksPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	serveFileOrText(w, r, filepath.Join(a.resources.Templates, "account-links.html"), fallbackAccountLinksHTML)
}

// handleDocs 处理 Swagger API 文档页面请求。
func (a *App) handleDocs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.Path == "/docs/" {
		http.Redirect(w, r, "/docs/index.html", http.StatusMovedPermanently)
		return
	}
	swaggerDocsHandler.ServeHTTP(w, r)
}

// handleOpenAPI 返回 OpenAPI 3.0 规范的 JSON 文档。
func (a *App) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeRawJSON(w, http.StatusOK, openAPISpec)
}

// handleQRRoot 处理 POST /qr 请求，创建新的扫码登录会话并返回二维码信息。
func (a *App) handleQRRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/qr" && r.URL.Path != "/wx/qrcodeauth" {
		writeError(w, http.StatusNotFound, "qr session not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.pruneQR()
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.RequestTimeout+35*time.Second)
	defer cancel()

	// 可选的请求体：指定本次扫码的「登录网络代理」。
	//
	// 这一步决定新账号在微信侧被识别成从哪里登录的，扫完再改代理是来不及的——
	// 登录 IP 与后续保活 IP 不一致，正是风控与掉线的常见来源。
	var body accountProxyIn
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if _, err := normalizeLoginProduct(body.Product); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	product := strings.ToLower(strings.TrimSpace(body.Product))
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		ref = strings.TrimSpace(body.Ref)
	}
	// 没显式指定渠道时：带 ref 的「重新扫码」沿用该账号原渠道。
	// 跨渠道重扫会把账号的身份换掉，绝不能因为漏传参数就悄悄切换。
	if product == "" && ref != "" {
		if acc, err := a.db.ResolveAccount(ctx, ref); err == nil && acc.Credentials != nil {
			product = protocol.CredentialsFromMap(acc.Credentials).Provider
		}
	}
	if product == "" {
		product = loginProductAppStore
	}

	client := a.qr
	var choice *qrProxyChoice
	if qrProxyRequested(body) {
		normalized, spec, err := a.normalizeAccountProxyInput(ctx, body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		bound, _, err := a.qrClientForSpec(ctx, spec)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		client = bound
		choice = &qrProxyChoice{In: normalized, Spec: spec}
	} else if ref != "" {
		// 不带代理参数但带 ref：沿用该账号的代理出口，
		// 避免「用 A 地 IP 登录、之后用 B 地 IP 保活」导致微信侧风控。
		client = a.qrClientForScan(ctx, ref)
	}

	img, err := client.GetQRCodeImageForProduct(ctx, product)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	// 会话标识会拼进文件路径，必须先校验再落任何状态，
	// 否则非法 ID 会留下一条永远清不掉的脏会话。
	if !isSafeFileToken(img.Session.ID) {
		writeError(w, http.StatusBadGateway, "二维码会话标识异常，已拒绝写入")
		return
	}
	a.mu.Lock()
	a.qrSessions[img.Session.ID] = img.Session
	a.qrClients[img.Session.ID] = client
	if choice != nil {
		a.qrProxies[img.Session.ID] = *choice
	} else {
		delete(a.qrProxies, img.Session.ID)
	}
	keep := make(map[string]bool, len(a.qrSessions))
	for sid := range a.qrSessions {
		keep[sid] = true
	}
	a.mu.Unlock()
	path := a.resources.qrPath(img.Session.ID)
	_ = os.WriteFile(path, img.ImageBytes, 0o644)
	a.cleanupQR(keep)
	basePath := "/qr"
	if r.URL.Path == "/wx/qrcodeauth" {
		basePath = "/wx/qrcodeauth"
	}
	out := map[string]any{
		"product":    product,
		"session_id": img.Session.ID,
		"status":     img.Session.Status,
		"image_url":  basePath + "/" + img.Session.ID + "/image",
	}
	if r.URL.Query().Get("as_base64") == "true" {
		out["image_base64"] = qr.DataURIImage(img.ImageBytes)
	} else {
		out["image_base64"] = nil
	}
	writeJSON(w, http.StatusOK, out)
}

// handleQR 处理 /qr/{session_id}/* 的子路由请求：
//   - GET  /qr/{id}/image   → 返回二维码图片
//   - GET  /qr/{id}/poll    → 轮询扫码状态
//   - POST /qr/{id}/confirm → 确认已授权会话并保存账号
//   - POST /qr/{id}/cancel  → 主动退役这张二维码（重新生成时用）
func (a *App) handleQR(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/qr/")
	if path == r.URL.Path {
		path = strings.TrimPrefix(r.URL.Path, "/wx/qrcodeauth/")
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		writeError(w, http.StatusNotFound, "qr session not found")
		return
	}
	sessionID, action := parts[0], parts[1]
	switch action {
	case "image":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		path := a.resources.qrPath(sessionID)
		// 防御性检查：确保解析后的路径仍在 QR 目录内，防止路径遍历攻击。
		if filepath.Base(path) != sessionID+".jpg" || strings.Contains(sessionID, "..") {
			writeError(w, http.StatusNotFound, "qr session not found")
			return
		}
		if _, err := os.Stat(path); err != nil {
			writeError(w, http.StatusNotFound, "qr session not found")
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		http.ServeFile(w, r, path)
	case "cancel":
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		// 「重新生成」必须先把旧会话退役：它没碰过账号库，但留着的会话会让
		// 一个迟到的 poll / confirm 把上一张二维码的扫码结果补完，
		// 用户看到的是「我明明刷新了，怎么还注册了刚才那个号」。
		a.cancelQRSession(sessionID)
		writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
	case "poll":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		sess := a.getQRSession(sessionID)
		if sess == nil {
			// 被主动取消（「重新生成」）的会话要给一个明确结论：
			// 报「不存在」会让用户以为二维码过期了，而实际上是他自己刷新的。
			if a.qrCancelledRecently(sessionID) {
				writeJSON(w, http.StatusOK, map[string]any{
					"session_id": sessionID,
					"status":     "cancelled",
				})
				return
			}
			writeError(w, http.StatusNotFound, "qr session not found")
			return
		}
		result, err := a.getQRClient(sessionID).PollQRCode(r.Context(), sess)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		// 终态会话立刻回收，而不是等下一次创建二维码时的 pruneQR。
		// 影响有两处：一是「迟到的 confirm」会拿到一个看着还能用的会话，
		// 白白去微信侧撞一次；二是磁盘上的二维码图片与内存里的会话会一直留着
		// （pruneQR 只在创建新二维码时跑，不再扫码就永远轮不到它）。
		// 先回收再返回，响应体仍带本次轮询到的终态，前端照常收尾。
		a.finishQRPoll(sessionID, result.Status)
		writeJSON(w, http.StatusOK, result)
	case "confirm":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		sess := a.getQRSession(sessionID)
		if sess == nil {
			writeError(w, http.StatusNotFound, "qr session not found")
			return
		}
		// 扫码与后续的换票、取资料必须用同一个客户端（同一条代理出口）
		client := a.getQRClient(sessionID)
		// 微信授权已完成但 login buffer 可能尚未就绪，短窗口内重试吸收这段竞态
		result, err := getLoginBufferWithRetry(r.Context(), client, sess)
		if err != nil {
			writeError(w, http.StatusConflict, "buffer not ready: "+err.Error())
			return
		}
		var userInfo map[string]any
		if ui, err := client.LoginBuffers().FetchUserInfo(r.Context(), result.Credentials); err == nil {
			userInfo = ui
		}
		// 「扫码前是否已存在」要在入库前判定：新账号才需要写入本次扫码选定的代理，
		// 已有账号重扫应当保留它原来的代理配置，不能被这次的临时选择覆盖。
		existed, err := a.accountExistsBeforeScan(r.Context(), result.Credentials.OpenID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		acc, err := a.storeFromScan(r.Context(), result.LoginBuffer, result.Credentials, userInfo)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if choice, ok := a.takeQRProxyChoice(sessionID); ok {
			if err := a.saveNewAccountProxy(r.Context(), acc.ID, existed, choice.In, choice.Spec); err != nil {
				// 代理没存住，账号就变成了「用代理登录、却按另一个出口保活」，
				// 这个不一致比让用户重扫一次更危险，所以新账号直接回滚。
				if !existed {
					_ = a.db.DeleteAccount(r.Context(), acc.ID)
				}
				writeError(w, http.StatusInternalServerError,
					"账号已扫码成功但代理配置保存失败，为避免登录出口与保活出口不一致已回滚，请重新扫码: "+err.Error())
				return
			}
		}
		a.dropQRSession(sessionID)
		writeJSON(w, http.StatusOK, acc.Public())
	default:
		writeError(w, http.StatusNotFound, "qr session not found")
	}
}

// accountView 在账号基础字段上附带「最近一次刷新失败原因」。
//
// 掉线之后最常被问的就是「为什么」，而这个原因以前只存在于容器日志里。
// 进程重启即清空：它是诊断信息，不是需要持久化的事实。
type accountView struct {
	store.AccountPublic
	LastError string `json:"last_error,omitempty"`
}

// handleAccountsRoot 处理 /accounts 的 GET 和 DELETE 请求。
// GET 返回所有已保存账号的列表，DELETE 删除指定账号。
//
// GET 走的是公开路由（免认证、无令牌），因为自动化脚本的调用链第一步就是它。
// 这里只放行只读：返回 AccountPublic（剔除 login_buffer 与 credentials），
// DELETE 与其它方法一律要求通过 a.isAuthenticated 走管理员会话鉴权，
// 避免「为了让脚本能读列表」而把删除账号的能力也一起放开。
func (a *App) handleAccountsRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/accounts" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		accounts, err := a.db.ListAccounts(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]accountView, 0, len(accounts))
		for _, acc := range accounts {
			out = append(out, accountView{
				AccountPublic: acc.Public(),
				LastError:     a.lastRefreshError(acc.ID),
			})
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodDelete:
		if !a.isAuthenticated(r) {
			writeError(w, http.StatusUnauthorized, "删除账号需要先登录")
			return
		}
		acc, ok := a.resolveAccountFromQuery(w, r)
		if !ok {
			return
		}
		if err := a.db.DeleteAccount(r.Context(), acc.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// 账号已经删掉，内存里跟它绑定的运行时状态也必须一起清。
		// 尤其是 /accounts/compact 会把 ID 重排复用：残留的代理租约会让
		// 新账号用上一个账号的出口，残留的退避与「连续拒绝」计数会让它
		// 无端被跳过刷新，甚至下一次 -109 就被判死。
		a.invalidateProxyLease(acc.ID)
		a.clearKeepAliveRetry(acc.ID)
		a.clearCredentialRejections(acc.ID)
		writeJSON(w, http.StatusOK, map[string]any{"deleted": acc.ID, "openid": acc.OpenID})
	default:
		if !a.isAuthenticated(r) {
			writeError(w, http.StatusUnauthorized, "请先登录")
			return
		}
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleAccountAvatar 返回指定账号的头像图片。
func (a *App) handleAccountAvatar(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/accounts/avatar" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	acc, ok := a.resolveAccountFromQuery(w, r)
	if !ok {
		return
	}
	a.serveAvatar(w, r, acc)
}

// handleAccountRefresh 刷新指定（或全部）账号的存活状态。
func (a *App) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/accounts/refresh" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body accountRefIn
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Ref == "" {
		a.refreshAll(w, r)
		return
	}
	acc, ok := a.resolveAccountRef(w, r, body.Ref)
	if !ok {
		return
	}
	status, _, err := a.refreshLiveness(r.Context(), acc)
	writeJSON(w, http.StatusOK, refreshOut(acc, status, err))
}

// handleAccountResync 重新同步指定（或全部）账号的个人资料。
func (a *App) handleAccountResync(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/accounts/resync" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body accountRefIn
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Ref == "" {
		a.resyncAll(w, r)
		return
	}
	acc, ok := a.resolveAccountRef(w, r, body.Ref)
	if !ok {
		return
	}
	updated, err := a.resyncProfile(r.Context(), acc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated.Public())
}

// handleSettingsWebhook 处理 Webhook 地址的读取与更新。
// GET 返回当前 Webhook URL；PUT 设置新的 Webhook URL。
func (a *App) handleSettingsWebhook(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/settings/webhook" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		url := a.getWebhookURL()
		writeJSON(w, http.StatusOK, map[string]any{"url": url})
	case http.MethodPut:
		var body struct {
			URL string `json:"url"`
		}
		if err := decodeOptionalJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := a.db.SetSetting(r.Context(), "webhook_url", body.URL); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		a.setWebhookURL(body.URL)
		log.Printf("[webhook] 地址已更新: %s", sanitizeURL(body.URL))
		writeJSON(w, http.StatusOK, map[string]any{"url": body.URL})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleWebhookTest 处理 POST /settings/webhook/test 请求，由服务端向指定 Webhook 地址发送测试推送。
func (a *App) handleWebhookTest(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/settings/webhook/test" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}

	var reqBody []byte
	if isWeworkWebhook(body.URL) {
		reqBody = buildWeworkTestPayload()
	} else {
		payload := map[string]any{
			"event":     "test",
			"timestamp": time.Now().Unix(),
			"account": map[string]any{
				"id":       0,
				"openid":   "test",
				"nickname": "测试推送",
			},
			"previous": "unknown",
			"current":  "unknown",
		}
		reqBody, _ = json.Marshal(payload)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, body.URL, bytes.NewReader(reqBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid webhook URL: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := a.healthClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "webhook unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	errDetail := ""

	// 解析响应体，检测常见的 Webhook 错误码（企业微信 / 钉钉 / 飞书等）
	if len(respBody) > 0 {
		var parsed map[string]any
		if json.Unmarshal(respBody, &parsed) == nil {
			if code, exists := parsed["errcode"]; exists {
				if c, ok2 := toFloat64(code); ok2 && c != 0 {
					msg, _ := parsed["errmsg"].(string)
					ok = false
					errDetail = fmt.Sprintf("errcode=%d errmsg=%s", int64(c), msg)
				}
			}
			// Slack / Discord: check "ok" boolean or "error" string
			if ok && parsed["ok"] == false {
				ok = false
				errDetail, _ = parsed["error"].(string)
			}
		}
	}

	if ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "http_status": resp.StatusCode})
	} else {
		if errDetail != "" {
			writeError(w, http.StatusBadGateway, "webhook rejected: "+errDetail)
		} else {
			writeError(w, http.StatusBadGateway, "webhook returned HTTP "+strconv.Itoa(resp.StatusCode))
		}
	}
}

func (a *App) handleGetCode(w http.ResponseWriter, r *http.Request) {
	a.serveGetCode(w, r, "/wxapp/getCode")
}

// handleWXCodeAlias 是 /wxapp/getCode 的兼容入口。
func (a *App) handleWXCodeAlias(w http.ResponseWriter, r *http.Request) {
	a.serveGetCode(w, r, "/wx/code")
}

func (a *App) serveGetCode(w http.ResponseWriter, r *http.Request, path string) {
	if !acceptWXAppRoute(w, r, path) {
		return
	}
	a.callWXApp(w, withWXOperation(r, eventlog.OpGetCode), false, a.invokeGetCode)
}

func (a *App) handleGetPhoneNumber(w http.ResponseWriter, r *http.Request) {
	a.serveGetPhoneNumber(w, r, "/wxapp/getPhoneNumber")
}

// handleWXPhoneAlias 是 /wxapp/getPhoneNumber 的兼容入口（小程序端叫 getPhoneNumber）。
func (a *App) handleWXPhoneAlias(w http.ResponseWriter, r *http.Request) {
	a.serveGetPhoneNumber(w, r, "/wx/getphonenumber")
}

func (a *App) serveGetPhoneNumber(w http.ResponseWriter, r *http.Request, path string) {
	if !acceptWXAppRoute(w, r, path) {
		return
	}
	a.callWXApp(w, withWXOperation(r, eventlog.OpGetPhoneNumber), false, a.invokeGetPhoneNumber)
}

func (a *App) handleOperateWXData(w http.ResponseWriter, r *http.Request) {
	if !acceptWXAppRoute(w, r, "/wxapp/operateWxData") {
		return
	}
	a.callWXApp(w, withWXOperation(r, eventlog.OpOperateWXData), true, a.invokeOperateWXData)
}

// encryptKeyOperation 是 operateWxData 里「取用户加密密钥」的服务端操作名。
const encryptKeyOperation = "webapi_getuserencryptkey"

// handleWXEncryptKey 暴露客户端习惯的 encryptkey 路径，
// 内部转发到 operateWxData 的加密密钥操作。
func (a *App) handleWXEncryptKey(w http.ResponseWriter, r *http.Request) {
	a.handleNamedWXOperation(w, r, "/wx/encryptkey", encryptKeyOperation)
}

// handleWXLatestUserKey 对应 wx.getUserCryptoManager().getLatestUserKey() 的名字。
func (a *App) handleWXLatestUserKey(w http.ResponseWriter, r *http.Request) {
	a.handleNamedWXOperation(w, r, "/wx/getlatestuserkey", encryptKeyOperation)
}

// handleWXCloud 对应云开发调用 cloud.callFunction。
func (a *App) handleWXCloud(w http.ResponseWriter, r *http.Request) {
	a.handleNamedWXOperation(w, r, "/wx/cloud", "cloud.callFunction")
}

// handleWXMPGetA8Key 对应公众号临时链接换取。
func (a *App) handleWXMPGetA8Key(w http.ResponseWriter, r *http.Request) {
	a.handleNamedWXOperation(w, r, "/wx/mpgeta8key", "mpGetA8Key")
}

// handleWXAppMsgExt 对应公众号文章扩展信息。
func (a *App) handleWXAppMsgExt(w http.ResponseWriter, r *http.Request) {
	a.handleNamedWXOperation(w, r, "/wx/appmsgext", "appmsgext")
}

// handleWXAppMsgLike 对应公众号文章点赞/阅读数据。
func (a *App) handleWXAppMsgLike(w http.ResponseWriter, r *http.Request) {
	a.handleNamedWXOperation(w, r, "/wx/appmsglike", "appmsglike")
}

// handleNamedWXOperation 把「按操作名命名的 /wx/* 入口」适配到通用的 operateWxData 通道。
//
// 调用方仍需提供完整 payload：这里只做路径层面的适配，不替调用方猜测业务参数。
// 之所以不自动补一个空 envelope，是因为多数操作缺参数会被微信侧拒绝，
// 自动补参只会把「参数漏了」伪装成「微信返回错误」，排查成本更高。
func (a *App) handleNamedWXOperation(w http.ResponseWriter, r *http.Request, path, apiName string) {
	if !acceptWXAppRoute(w, r, path) {
		return
	}
	var body wxappRequest
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Ref) == "" {
		writeError(w, http.StatusBadRequest, "ref is required")
		return
	}
	if body.AppID == "" {
		writeError(w, http.StatusBadRequest, "app_id is required")
		return
	}
	if body.Payload == nil {
		writeError(w, http.StatusBadRequest, "payload is required for "+apiName)
		return
	}
	acc, ok := a.resolveAccountRef(w, r, body.Ref)
	if !ok {
		return
	}
	// 操作名归一化在 invokeOperateWXData 里统一做，
	// /wxapp/operateWxData 与这里的所有别名共用同一条路径。
	a.writeWXAppResult(w, r, acc, body.AppID, body.Payload, a.invokeOperateWXData)
}

// normalizeEncryptKeyPayload 接受客户端侧的 API 拼写
// （wx.getUserCryptoManager().getLatestUserKey() 等），只改写操作名，
// 其余业务字段原样保留。
func normalizeEncryptKeyPayload(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	out := make(map[string]any, len(payload))
	for key, value := range payload {
		out[key] = value
	}
	if name, ok := out["api_name"].(string); ok && isEncryptKeyOperationAlias(name) {
		out["api_name"] = encryptKeyOperation
	}
	// 有些客户端把 api_name 塞在 data 里，两层都要认
	if nested, ok := out["data"].(map[string]any); ok {
		copyNested := make(map[string]any, len(nested))
		for key, value := range nested {
			copyNested[key] = value
		}
		if name, ok := copyNested["api_name"].(string); ok && isEncryptKeyOperationAlias(name) {
			copyNested["api_name"] = encryptKeyOperation
		}
		out["data"] = copyNested
	}
	return out
}

// isEncryptKeyOperationAlias 判断是否是加密密钥操作的客户端拼写。
func isEncryptKeyOperationAlias(name string) bool {
	return name == "getLatestUserKey" || name == "getUserEncryptKey"
}

func acceptWXAppRoute(w http.ResponseWriter, r *http.Request, path string) bool {
	if r.URL.Path != path {
		writeError(w, http.StatusNotFound, "not found")
		return false
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	return true
}

type accountRefIn struct {
	Ref string `json:"ref"`
}

type wxappRequest struct {
	Ref     string         `json:"ref"`
	AppID   string         `json:"app_id"`
	Payload map[string]any `json:"payload"`
}

type wxappCall func(ctx context.Context, acc *store.WechatAccount, appID string, payload map[string]any, proxyValue string, fallbackDirect bool) (map[string]any, error)

func (a *App) callWXApp(w http.ResponseWriter, r *http.Request, requirePayload bool, call wxappCall) {
	var body wxappRequest
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Ref == "" {
		writeError(w, http.StatusBadRequest, "ref is required")
		return
	}
	if body.AppID == "" {
		writeError(w, http.StatusBadRequest, "app_id is required")
		return
	}
	if requirePayload && body.Payload == nil {
		writeError(w, http.StatusBadRequest, "payload is required")
		return
	}
	acc, ok := a.resolveAccountRef(w, r, body.Ref)
	if !ok {
		return
	}
	a.writeWXAppResult(w, r, acc, body.AppID, body.Payload, call)
}

// writeWXAppResult 执行一次协议调用并把结果/错误写回 HTTP 响应。
//
// 抽出来是为了让 /wxapp/* 与 /wx/* 别名共用同一套错误映射：
// 凭据失效必须稳定映射成 409，其他失败统一 502，不会因为入口不同而变。
func (a *App) writeWXAppResult(w http.ResponseWriter, r *http.Request, acc *store.WechatAccount, appID string, payload map[string]any, call wxappCall) {
	result, err := a.invokeWXApp(r.Context(), acc, appID, payload, call)
	if err != nil {
		var expired accountExpiredError
		switch {
		case errors.As(err, &expired):
			a.writeAccountExpired(w, expired)
		case writeIfCircuitOpen(w, err):
			// 本地熔断：429 + Retry-After，与「上游 502」「需重扫 409」区分开。
			return
		default:
			a.writeProtocolCallError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"openid": acc.OpenID, "result": result})
}

// decodeOptionalJSON 从请求 body 解析 JSON。若 body 为空（EOF）则视为成功，不报错。
// 限制最大 1MB，防止恶意大请求耗尽内存。
func decodeOptionalJSON(r *http.Request, dst any) error {
	err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// resolveAccountFromQuery 从 URL 查询参数 ref 中解析账号引用（ID/UIN/openid）。
func (a *App) resolveAccountFromQuery(w http.ResponseWriter, r *http.Request) (*store.WechatAccount, bool) {
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		writeError(w, http.StatusBadRequest, "ref query param is required")
		return nil, false
	}
	return a.resolveAccountRef(w, r, ref)
}

// resolveAccountRef 根据引用字符串（ID/UIN/openid）解析并获取微信账号。
func (a *App) resolveAccountRef(w http.ResponseWriter, r *http.Request, ref string) (*store.WechatAccount, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		writeError(w, http.StatusBadRequest, "ref is required")
		return nil, false
	}
	acc, err := a.db.ResolveAccount(r.Context(), ref)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "account not found: "+ref)
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return nil, false
	}
	return acc, true
}

// refreshAll 刷新所有账号的存活状态。
func (a *App) refreshAll(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.db.ListAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(accounts))
	for _, acc := range accounts {
		status, _, err := a.refreshLiveness(context.Background(), acc)
		out = append(out, refreshOut(acc, status, err))
	}
	writeJSON(w, http.StatusOK, out)
}

// resyncAll 重新同步所有账号的个人资料（昵称、头像等）。
func (a *App) resyncAll(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.db.ListAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]store.AccountPublic, 0, len(accounts))
	for _, acc := range accounts {
		updated, err := a.resyncProfile(context.Background(), acc)
		if err != nil {
			log.Printf("[resync] 账号 %d (%s) 同步失败: %v", acc.ID, acc.OpenID, err)
			continue
		}
		out = append(out, updated.Public())
	}
	writeJSON(w, http.StatusOK, out)
}

// serveAvatar 返回账号头像。优先返回本地缓存文件，其次重定向到远程 URL。
func (a *App) serveAvatar(w http.ResponseWriter, r *http.Request, acc *store.WechatAccount) {
	if acc.Avatar != nil && *acc.Avatar != "" {
		if _, err := os.Stat(*acc.Avatar); err == nil {
			w.Header().Set("Content-Type", "image/jpeg")
			http.ServeFile(w, r, *acc.Avatar)
			return
		}
		if strings.HasPrefix(*acc.Avatar, "http://") || strings.HasPrefix(*acc.Avatar, "https://") {
			http.Redirect(w, r, *acc.Avatar, http.StatusFound)
			return
		}
	}
	writeError(w, http.StatusNotFound, "no avatar")
}

// storeFromScan 将扫码登录结果保存为新的微信账号记录。
//
// 这里用 ToMapForScan 而不是 ToMap：扫码是 30 天授权窗口的起点，
// 只有扫码这一刻才该写入 refresh_token_observed_at。
func (a *App) storeFromScan(ctx context.Context, loginBuffer string, creds protocol.LoginBufferCredentials, userInfo map[string]any) (*store.WechatAccount, error) {
	openid := creds.OpenID
	nick := pickNickname(userInfo, creds.Nickname)
	avatar := a.resolveAvatar(ctx, openid, userInfo)
	status := "alive"
	acc, err := a.db.UpsertAccount(ctx, openid, loginBuffer, stringPtrMaybe(nick), stringPtrMaybe(nick), stringPtrMaybe(avatar), userInfo, creds.ToMapForScan(), &status)
	if err == nil && acc != nil {
		// 新授权从零开始：上一个授权留下的目标/会话熔断不能带过来，
		// 否则刚扫码的账号会被旧状态挡在本地。
		a.circuit.forget(acc.ID)
		a.events.Emit("account.scanned", eventlog.ForAccount(acc))
	}
	return acc, err
}

// refreshLiveness 的实现已迁移到 keepalive.go：刷新、状态判定与 Webhook 通知
// 统一收敛到 refreshAccount 一处，避免不同调用路径的通知行为不一致。

// resyncProfile 重新同步账号的个人资料信息。
func (a *App) resyncProfile(ctx context.Context, acc *store.WechatAccount) (*store.WechatAccount, error) {
	nick := pickNickname(acc.UserInfo, deref(acc.Nickname))
	avatar := a.resolveAvatar(ctx, acc.OpenID, acc.UserInfo)
	if avatar == "" {
		avatar = deref(acc.Avatar)
	}
	// 数据未变化时跳过数据库写入，减少不必要的 updated_at 更新
	if nick == deref(acc.Nickname) && avatar == deref(acc.Avatar) {
		return acc, nil
	}
	if err := a.db.SetAccountProfile(ctx, acc.ID, stringPtrMaybe(nick), stringPtrMaybe(avatar), acc.UserInfo); err != nil {
		return nil, err
	}
	return a.db.GetAccount(ctx, acc.ID)
}

// accountExpiredError 表示账号凭据已被上游明确拒绝，需要重新扫码。
//
// 除 openid 外还带上账号 ID 与原始原因：ID 用于日志定位，cause 供
// credentialDiagnostics 展开成结构化的 stage / upstream_code。
type accountExpiredError struct {
	openid    string
	accountID int64
	cause     error
}

func (e accountExpiredError) Error() string { return "account expired: " + e.openid }

// Unwrap 暴露内部原因，让 errors.Is / errors.As 能沿链走到真正的拒绝码。
//
// 少了它，「账号失效」这层包装就成了错误链的终点：上层拿不到
// RefreshRejectedError 之类的具体类型，只能看到一个 openid，
// 也就没法在排查时区分「微信说令牌失效」和「我们请求构造有问题」。
func (e accountExpiredError) Unwrap() error { return e.cause }

// invokeWXApp 通过 MMTLS 协议连接调用 wxapp 接口，自动处理会话失效重试。
//
// 关键点：只有「明确的凭据失效」才允许把账号判死；纯网络/代理故障只丢弃会话缓存，
// 否则一次代理抖动就会把所有账号刷成 expired，并触发一轮误报告警。
func (a *App) invokeWXApp(ctx context.Context, acc *store.WechatAccount, appID string, payload map[string]any, call wxappCall) (resultOut map[string]any, errOut error) {
	started := time.Now()
	op := wxOperation(ctx)
	// 每次协议调用落一条事件：被哪个 AppID、哪类请求、耗时多少、拒绝在哪一层。
	// 只在错误链上记「失败了」而没有目标与类型，排查连续 -14 时等于没线索。
	defer func() {
		fields := eventlog.ForAccount(acc).WithTarget(appID, op, wxAPIName(op, payload))
		fields.DurationMS = time.Since(started).Milliseconds()
		fields.Rejections = eventRejections(errOut)
		event := "protocol.succeeded"
		var open *circuitOpenError
		switch {
		case errors.As(errOut, &open) && open.Scope == "pacing":
			// 限速拒绝一分钟可能几百次，由 paceWXApp 的汇总事件统一上报，
			// 这里不再逐条记，否则日志会被刷满。
			return
		case errors.As(errOut, &open):
			// 被本地熔断挡下的请求根本没发往上游，不能记成「协议调用失败」。
			event = "protocol.circuit_deferred"
		case errOut != nil:
			event = "protocol.failed"
		}
		a.events.Emit(event, fields)
	}()
	// 把排队、协议登录和凭据补救都算进同一份预算，避免层层超时叠加到客户端放弃之后
	ctx, cancel := context.WithTimeout(ctx, a.cfg.RequestTimeout+35*time.Second)
	defer cancel()
	if accountStatus(acc) == "expired" {
		// 脚本打到失效账号上时顺手告警：这条调用链本来就会被跳过，
		// 不借它发声，就可能只剩日志里一行错误
		a.alertExpired(acc)
		return nil, accountExpiredError{openid: acc.OpenID, accountID: acc.ID}
	}
	if accountNeedsRescan(acc) {
		return nil, accountExpiredError{openid: acc.OpenID, accountID: acc.ID}
	}
	// 熔断先于一切上游请求：已经确认被拒的目标/会话不再重复发送，
	// 直接本地 429 让调用方退避。它不改账号状态、不清凭据。
	if breakerErr := a.circuit.check(acc.ID, appID, op); breakerErr != nil {
		return nil, breakerErr
	}
	// 上面被挡下的请求不占档期；凡是真会打到上游的，都按账号排队。
	// 这样一串脚本并发不会变成一次针对同一授权的登录风暴。
	if paceErr := a.paceWXApp(ctx, acc); paceErr != nil {
		return nil, paceErr
	}
	proxyValue, fallbackDirect, err := a.resolveWXAppProxy(ctx, acc.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve account proxy: %w", err)
	}
	result, callErr := call(ctx, acc, appID, payload, proxyValue, fallbackDirect)
	a.recordCircuitOutcome(acc, appID, op, callErr)
	if callErr == nil {
		return result, nil
	}
	// 账号在这次调用期间被换过身份（更新链接 / 合并 / 跨渠道重扫）时，
	// 失败原因是「你手里那份授权已经不在了」：既不能判它失效，也不该拿它去刷新凭据。
	// 直接返回明确错误，让调用方重新取一次账号信息再重试。
	if current, snapshotErr := a.db.GetAccount(ctx, acc.ID); snapshotErr == nil && !current.SameAuthorization(acc) {
		return nil, store.ErrAccountAuthorizationChanged
	}
	if errors.Is(callErr, store.ErrAccountAuthorizationChanged) || errors.Is(callErr, store.ErrSessionChanged) {
		return nil, store.ErrAccountAuthorizationChanged
	}
	// 「仅原会话可用」的账号：凭据层已经明确拒绝过，不能再走 OAuth 或 ManualAuth。
	if acc.SessionOnly() {
		if errors.Is(callErr, protocol.ErrCachedSessionUnavailable) {
			// 缓存也没了，这条路彻底走不通：转为需重扫，并停止凭据与倒计时。
			applied, err := a.db.ApplyCredentialRefresh(ctx, acc, "", nil, "expired")
			if err != nil {
				return nil, err
			}
			if !applied {
				return nil, store.ErrAccountAuthorizationChanged
			}
			return nil, accountExpiredError{accountID: acc.ID, openid: acc.OpenID, cause: callErr}
		}
		// 保留的 PSK 还能用，只是这次没成功：原样返回，不动凭据。
		return nil, callErr
	}
	if errors.Is(callErr, protocol.ErrLoginCodeUnavailable) {
		// 应用拿不到 code 不等于账号失效：同一份会话下别的 AppID 可能正常。
		// 直接返回，不重建会话、不刷新凭据、不清账号——否则一个应用的空结果
		// 会连带把同账号其他应用的可用会话一起销毁。
		return nil, callErr
	}
	// 恢复层已经试过一轮：无论成败都不再往下推断凭据失效。
	var codeRecovery *protocol.CodeSessionRecoveryError
	if protocol.IsApplicationRequestRejection(callErr) || errors.As(callErr, &codeRecovery) {
		return nil, callErr
	}
	var networkErr net.Error
	// 票据被拒（ManualAuth -10）与网络失败要分开处理：它发生在「具体 API 发出之前」，
	// 说明这张票已经不能用了，但账号凭据本身往往还是好的——换一张新票常常就能继续。
	var rejected *protocol.ManualAuthRejectedError
	bufferRejected := errors.As(callErr, &rejected) && rejected.Code == -10
	if ctx.Err() != nil || (!bufferRejected && (errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded) ||
		errors.As(callErr, &networkErr) || errors.Is(callErr, io.EOF) || errors.Is(callErr, io.ErrUnexpectedEOF))) {
		// 传输层失败不能证明凭据过期：保留会话与原始诊断信息，也不重放这条慢调用
		return nil, callErr
	}
	if bufferRejected {
		// 只换票、不轮换令牌：走的是「取票据」接口，不碰 refresh token、
		// 不改扫码时间与有效期。换到新票就重试一次原调用。
		// 但取票本身就是一次 OAuth 层调用，预算用完了就不再向上游要。
		if !a.pool.AllowLogin(acc.ID) {
			return nil, errors.Join(callErr, errLoginBudget)
		}
		buffer, bufferErr := a.fetchAccountLoginBuffer(ctx, acc)
		if bufferErr == nil {
			fresh := *acc
			fresh.LoginBuffer = buffer
			result, retryErr := call(ctx, &fresh, appID, payload, proxyValue, fallbackDirect)
			if retryErr != nil {
				return nil, errors.Join(callErr, fmt.Errorf("retry with fresh login buffer: %w", retryErr))
			}
			return result, nil
		}
		callErr = errors.Join(callErr, fmt.Errorf("fetch fresh login buffer: %w", bufferErr))
		var pcRejected *pcmgr.BufferRejectedError
		if errors.As(bufferErr, &pcRejected) && pcRejected.Code == 1 {
			// 电脑管家 20138/1 是「通用取票拒绝」：官方 DLL 在 key 续期成功时
			// 同样会返回它。这里必须直接返回，既不能在每次接口测试时都去轮换 key
			// （越换越短），也不能把它当成账号凭据被撤销。
			return nil, callErr
		}
		if ctx.Err() != nil || errors.Is(bufferErr, context.Canceled) || errors.Is(bufferErr, context.DeadlineExceeded) ||
			errors.As(bufferErr, &networkErr) || errors.Is(bufferErr, io.EOF) || errors.Is(bufferErr, io.ErrUnexpectedEOF) {
			return nil, callErr
		}
	}
	// 强制刷新是又一次 OAuth 层调用。预算用完就保留原错误并直接返回，
	// 缓存会话也不作废——为一次不会发生的补救清掉会话没有意义。
	if !a.pool.AllowLogin(acc.ID) {
		return nil, errors.Join(callErr, errLoginBudget)
	}
	_ = a.db.InvalidateSession(ctx, acc.ID, proxyValue)
	// 凭据补救始终走账号自己的刷新通道，即使本次协议调用显式选择了直连。
	// 打上 on_demand 标记：这次刷新是业务调用失败后触发的，不是面板手动点的。
	status, _, refreshErr := a.refreshAccountWithSource(withRefreshSource(ctx, eventlog.SourceOnDemand), acc, true)
	if status == "expired" {
		// cause 必须同时带上「首次调用失败」与「刷新链路」：
		// 409 的 upstream_errors 是从这里展开的，只带后者会丢掉
		// ManualAuth / 取票据这类更上游的拒绝类型，排查时看不到真正原因。
		return nil, accountExpiredError{openid: acc.OpenID, accountID: acc.ID, cause: errors.Join(callErr, refreshErr)}
	}
	if refreshErr != nil {
		// 首次调用失败的原因必须一并保留：它才是根因，
		// 刷新失败只是「补救动作没救回来」。只报后者会让排查失去方向——
		// 看到 -109 却不知道最初那次调用为什么失败，只能靠猜。
		// 用 %w 而不是 %v 包住首次失败：那里带着 ManualAuth / 取票据的
		// 类型化错误，拍平成一个字符串后，上层就再也区分不出它到底是哪一类拒绝。
		return nil, fmt.Errorf("refresh account credentials: %w",
			errors.Join(refreshErr, fmt.Errorf("首次调用失败: %w", callErr)))
	}
	// refreshErr 为空但状态也不是 alive 时（例如本轮判定为无需续期而跳过刷新），
	// 凭据并没有被证明失效，直接重试一次原调用，不要伪造一条错误出来。
	if fresh, getErr := a.db.GetAccount(ctx, acc.ID); getErr == nil && fresh != nil {
		acc = fresh
	}
	return call(ctx, acc, appID, payload, proxyValue, fallbackDirect)
}

// fetchAccountLoginBuffer 只「取票据」，不续期、不轮换令牌。
//
// 它与 refreshAccount 是两件事：续期会换掉 refresh token 并推进有效期，
// 而取票据只是拿一份新的 login_buffer。协议调用遇到 ManualAuth 拒绝时应该走
// 这一条——先试最便宜的补救，失败再考虑续期，避免无谓地消耗 token。
//
// 出口用的是账号自己配置的代理，即使本次协议调用显式选择了直连：
// 凭据相关请求换出口等于换登录地，反而会触发风控。
func (a *App) fetchAccountLoginBuffer(ctx context.Context, acc *store.WechatAccount) (string, error) {
	proxyValue, fallbackDirect, err := a.resolveAccountProxy(ctx, acc.ID)
	if err != nil {
		return "", err
	}
	creds := protocol.CredentialsFromMap(acc.Credentials)
	// 统一走 fetchLoginBufferWithProxy：它内部再用 fetchLoginBuffer 这个缝。
	// 之前这里直接调客户端，等于绕开了缝——测试替换掉取票实现后，
	// -10 恢复分支仍然去打真实网络，表现成「不去取票、反而去续期」。
	return a.fetchLoginBufferWithProxy(ctx, creds, proxyValue, fallbackDirect)
}

// accountNeedsRescan 覆盖「刷新结论不明、但缓存凭据已经过期」的账号。
// 这类账号不能继续被每次脚本调用拖进协议层重试，必须先重新扫码。
func accountNeedsRescan(acc *store.WechatAccount) bool {
	if acc == nil || accountStatus(acc) != "unknown" || acc.Credentials == nil {
		return false
	}
	creds := protocol.CredentialsFromMap(acc.Credentials)
	// 管家的"有效期"是下次续期计划，不是 OAuth 到期；拿它当失效证据会误判。
	if creds.Provider == "pcmgr" {
		return false
	}
	return creds.ExpiresAt > 0 && creds.ExpiresAt <= time.Now().Unix()
}

// recordCircuitOutcome 把一次协议结果折算成熔断计数。
//
// 只有「应用层 -14」和「空 code」会累计：网络失败、ManualAuth 拒绝、OAuth 拒绝
// 都有各自的处理路径，把它们也算进来会让熔断在完全无关的故障上误触发。
// 一次成功则把该账号的两个层级一起清空——会话又能用了。
func (a *App) recordCircuitOutcome(acc *store.WechatAccount, appID, op string, callErr error) {
	outcome := circuitOther
	switch {
	case callErr == nil:
		outcome = circuitOK
	case protocol.IsApplicationRequestRejection(callErr):
		outcome = circuitAppReject
	case errors.Is(callErr, protocol.ErrLoginCodeUnavailable):
		outcome = circuitEmptyCode
	}
	if scope := a.circuit.record(acc.ID, appID, op, outcome); scope != "" {
		fields := eventlog.ForAccount(acc).WithTarget(appID, op, "")
		fields.Rejections = []eventlog.Rejection{{Stage: "circuit_" + scope}}
		a.events.Emit("protocol.circuit_open", fields)
		log.Printf("[circuit] 账号 %d 的%s已熔断，暂停向上游重复请求", acc.ID, scope)
	}
}

func (a *App) invokeGetCode(ctx context.Context, acc *store.WechatAccount, appID string, _ map[string]any, proxyValue string, fallbackDirect bool) (map[string]any, error) {
	result, err := a.pool.GetCode(ctx, acc.LoginBuffer, appID, acc.ID, proxyValue, fallbackDirect, acc)
	if err == nil || !protocol.IsApplicationRequestRejection(err) {
		return result, err
	}
	// 单独的已认证应用层 -14：不重建会话、不刷新凭据，改为在隔离候选会话里
	// 先验证「同一目标确实能取到 code」，验证通过才原子替换缓存。
	return a.pool.RecoverCodeSession(ctx, acc, appID, proxyValue, fallbackDirect, err, func(ctx context.Context) (string, error) {
		return a.fetchAccountLoginBuffer(ctx, acc)
	})
}

func (a *App) invokeGetPhoneNumber(ctx context.Context, acc *store.WechatAccount, appID string, _ map[string]any, proxyValue string, fallbackDirect bool) (map[string]any, error) {
	return a.pool.GetPhoneNumber(ctx, acc.LoginBuffer, appID, acc.ID, proxyValue, fallbackDirect, acc)
}

func (a *App) invokeOperateWXData(ctx context.Context, acc *store.WechatAccount, appID string, payload map[string]any, proxyValue string, fallbackDirect bool) (map[string]any, error) {
	// 操作名归一化放在这唯一的出口上：
	// 无论是 /wxapp/operateWxData 还是 /wx/encryptkey、/wx/getlatestuserkey，
	// 都走同一条通道，客户端拼写差异不会导致「换个入口就调不通」。
	return a.pool.OperateWXData(ctx, acc.LoginBuffer, appID, normalizeEncryptKeyPayload(payload), acc.ID, proxyValue, fallbackDirect, acc)
}

// ---- Webhook 推送辅助 ----

// isWeworkWebhook 检测 URL 是否为企业微信机器人 Webhook 地址。
func isWeworkWebhook(url string) bool {
	return strings.Contains(url, "qyapi.weixin.qq.com/cgi-bin/webhook/send")
}

// buildWeworkRenewalPayload 构造「凭据已过期且续不上」的企业微信卡片。
//
// 与状态变更卡片刻意区分：这张卡在说「账号现在用不了了」，但状态可能仍留在
// 待确认并继续重试，所以不能写成「已失效，请重扫」——那会把网络问题误导成账号问题。
func buildWeworkRenewalPayload(acc *store.WechatAccount, status, reason string) []byte {
	nick := deref(acc.Nickname)
	if nick == "" {
		nick = acc.OpenID
	}
	advice := "检查代理与网络连通性；若持续失败且刷新被微信侧拒绝，请重新扫码。"
	content := fmt.Sprintf(
		"## ⚠️ 账号无法续期\n"+
			"> 账号: <font color=\"comment\">%s</font>\n"+
			"> OpenID: <font color=\"comment\">%s</font>\n"+
			"> 当前状态: <font color=\"warning\">%s</font>\n"+
			"> 原因: <font color=\"warning\">%s</font>\n"+
			"> 时间: <font color=\"comment\">%s</font>\n"+
			"> 建议: <font color=\"comment\">%s</font>\n",
		nick, acc.OpenID, status, reason,
		time.Now().Format("2006-01-02 15:04:05"), advice,
	)

	wrapper := map[string]any{
		"msgtype": "markdown_v2",
		"markdown_v2": map[string]string{
			"content": content,
		},
	}
	body, _ := json.Marshal(wrapper)
	return body
}

// buildWeworkExpiredPayload 构造「账号已失效，必须重新扫码」的卡片。
//
// 与状态变更卡片（alive → expired）不同：这张卡的语义是「它现在仍然是失效的」，
// 用于周期性的重提醒。它不依赖状态跃迁，因此不存在「跃迁没被捕捉到就永远沉默」的问题。
func buildWeworkExpiredPayload(acc *store.WechatAccount) []byte {
	nick := deref(acc.Nickname)
	if nick == "" {
		nick = acc.OpenID
	}
	content := fmt.Sprintf(
		"## ❌ 账号已失效，需重新扫码\n"+
			"> 账号: <font color=\"comment\">%s</font>\n"+
			"> OpenID: <font color=\"comment\">%s</font>\n"+
			"> 当前状态: <font color=\"warning\">expired</font>\n"+
			"> 时间: <font color=\"comment\">%s</font>\n"+
			"> 说明: <font color=\"comment\">凭据已彻底失效，自动保活已停止；重新扫码后恢复。</font>\n",
		nick, acc.OpenID, time.Now().Format("2006-01-02 15:04:05"),
	)

	wrapper := map[string]any{
		"msgtype": "markdown_v2",
		"markdown_v2": map[string]string{
			"content": content,
		},
	}
	body, _ := json.Marshal(wrapper)
	return body
}

func buildWeworkPayload(event, oldStatus, newStatus string, acc *store.WechatAccount) []byte {
	nick := deref(acc.Nickname)
	if nick == "" {
		nick = acc.OpenID
	}
	emoji := "⚠️"
	if newStatus == "alive" {
		emoji = "✅"
	} else if newStatus == "expired" {
		emoji = "❌"
	}
	content := fmt.Sprintf(
		"## %s 账号状态变更\n"+
			"> 账号: <font color=\"comment\">%s</font>\n"+
			"> OpenID: <font color=\"comment\">%s</font>\n"+
			"> 状态: <font color=\"warning\">%s</font> → <font color=\"info\">%s</font>\n"+
			"> 时间: <font color=\"comment\">%s</font>\n",
		emoji, nick, acc.OpenID,
		oldStatus, newStatus,
		time.Now().Format("2006-01-02 15:04:05"),
	)

	wrapper := map[string]any{
		"msgtype": "markdown_v2",
		"markdown_v2": map[string]string{
			"content": content,
		},
	}
	body, _ := json.Marshal(wrapper)
	return body
}

// buildWeworkTestPayload 构建企业微信 markdown_v2 格式的测试推送内容。
func buildWeworkTestPayload() []byte {
	content := fmt.Sprintf(
		"## 🧪 yybwechat Webhook 测试\n"+
			"> 这是一条测试消息，用于验证 Webhook 配置是否正常\n"+
			"> 时间: <font color=\"comment\">%s</font>\n"+
			"> 服务: <font color=\"comment\">yybwechat</font>",
		time.Now().Format("2006-01-02 15:04:05"),
	)
	wrapper := map[string]any{
		"msgtype": "markdown_v2",
		"markdown_v2": map[string]string{
			"content": content,
		},
	}
	body, _ := json.Marshal(wrapper)
	return body
}

// ---- 后台健康检查 ----

// getWebhookURL 线程安全地获取当前 Webhook URL。
func (a *App) getWebhookURL() string {
	a.webhookMu.RLock()
	defer a.webhookMu.RUnlock()
	return a.webhookURL
}

// setWebhookURL 线程安全地更新内存中的 Webhook URL。
func (a *App) setWebhookURL(url string) {
	a.webhookMu.Lock()
	a.webhookURL = url
	a.webhookMu.Unlock()
}

// runHealthChecker 是后台 goroutine，定期扫描 last_checked_at 超时的账号并刷新存活状态。
// 状态发生变化时通过 Webhook 推送通知。
func (a *App) runHealthChecker() {
	ticker := time.NewTicker(a.cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.stopHealth:
			log.Println("[health] 健康检查已停止")
			return
		case <-ticker.C:
			a.scanAndCheck()
		}
	}
}

// scanAndCheck 扫描所有账号，对超时未检查的账号执行刷新。
// 状态变更的 Webhook 通知由 refreshAccount 内部统一处理，这里不再重复推送。
func (a *App) scanAndCheck() {
	// 先把上一轮没送达的通知补推掉：账号一旦被判为 expired 就不会再被扫描，
	// 漏掉这一次就再没有机会补上。
	a.retryPendingNotifications(context.Background())

	accounts, err := a.db.ListAccounts(context.Background())
	if err != nil {
		log.Printf("[health] 获取账号列表失败: %v", err)
		return
	}
	if len(accounts) == 0 {
		return
	}

	staleThreshold := time.Now().Add(-a.cfg.CheckStale).Unix()
	for _, acc := range accounts {
		select {
		case <-a.stopHealth:
			return
		default:
		}

		// 跳过无需检查的账号：无凭证、已判定失效、或 last_checked_at 仍在有效期内
		if acc.Credentials == nil {
			// 没有凭据就永远刷不回来，必须让运营者知道
			a.alertUnusableCredentials(acc, accountStatus(acc))
			continue
		}
		// 与凭据保活保持一致：一旦判定为 expired，凭据已彻底失效，只能靠用户重新扫码。
		// 这里如果不跳过，健康检查会在 stale 窗口过期后反复尝试刷新一个救不回来的账号。
		if accountStatus(acc) == "expired" {
			// 跳过刷新不等于跳过告警：只要还挂着 expired，就按重提醒窗口持续推送
			a.alertExpired(acc)
			continue
		}
		lastChecked := int64(0)
		if acc.LastCheckedAt != nil {
			lastChecked = *acc.LastCheckedAt
		}
		if lastChecked > staleThreshold {
			// 距上次检查还不够久，本轮不刷。但凭据可能已经过期而账号正卡在
			// 失败退避里——这种「跳过刷新」的账号以前没有任何告警出口。
			a.alertUnusableCredentials(acc, accountStatus(acc))
			continue
		}

		status, _, refreshErr := a.refreshAccount(context.Background(), acc, true)
		if refreshErr != nil {
			// 瞬时错误会归为 unknown 并等待重试，这里只记录原因
			log.Printf("[health] 账号 %s (%s) 刷新失败（状态 %s）: %v",
				deref(acc.Nickname), acc.OpenID, status, refreshErr)
		}
	}

	// 每轮健康检查结束后清理过期 session，避免 sessions 表无限增长。
	if n, err := a.db.PurgeExpiredSessions(context.Background()); err != nil {
		log.Printf("[health] 清理过期会话失败: %v", err)
	} else if n > 0 {
		log.Printf("[health] 已清理 %d 条过期会话", n)
	}
}

// shouldNotifyStatusChange 判断一次状态变化是否值得推送 Webhook。
//
// 只有收敛到终态（alive 可用 / expired 需重扫）才通知。unknown 表示本轮无法判定
// （网络抖动、超时等），下一轮重试即可恢复，推送只会制造告警噪音。
func shouldNotifyStatusChange(oldStatus, newStatus string) bool {
	if oldStatus == "" || oldStatus == newStatus {
		return false
	}
	return newStatus == "alive" || newStatus == "expired"
}

// notifyWebhook 向已配置的 Webhook 地址 POST 账号状态变更通知。
// 自动检测企业微信 Webhook 地址并适配消息格式。
//
// 返回值表示这次推送是否真正送达。调用方据此决定要不要把该状态记为「已通知」：
// 失败时不记，下一轮扫描会重新推送，避免一次网络抖动把掉线告警永久吞掉。
func (a *App) notifyWebhook(acc *store.WechatAccount, oldStatus, newStatus string) bool {
	delivered := false
	url := a.getWebhookURL()
	var body []byte
	if isWeworkWebhook(url) && url != "" {
		body = buildWeworkPayload("status_changed", oldStatus, newStatus, acc)
	} else if url != "" {
		payload := map[string]any{
			"event":     "status_changed",
			"timestamp": time.Now().Unix(),
			"account": map[string]any{
				"id":       acc.ID,
				"openid":   acc.OpenID,
				"uin":      acc.UIN,
				"nickname": acc.Nickname,
			},
			"previous": oldStatus,
			"current":  newStatus,
		}
		body, _ = json.Marshal(payload)
	}
	if url != "" {
		delivered = a.postWebhook(url, body, deref(acc.Nickname))
	}
	// 同一条告警再发一份到额外通道；只要有一路送达就算送达，
	// 否则待补推队列会把已经推出去的消息再推一遍。
	title, content := statusChangeMessage(acc, oldStatus, newStatus)
	return a.pushExtraChannel(title, content) || delivered
}

// notifyWebhookRenewal 推送「凭据已过期且续不上」的告警。
//
// 与状态变更通知分开，是因为它的语义不同：账号此刻确实不可用了，
// 但还留在 unknown 继续重试（网络或代理恢复后能自愈），不能写成终态误导运营者去重扫。
func (a *App) notifyWebhookRenewal(acc *store.WechatAccount, status, reason string) bool {
	delivered := false
	url := a.getWebhookURL()
	var body []byte
	if isWeworkWebhook(url) && url != "" {
		body = buildWeworkRenewalPayload(acc, status, reason)
	} else if url != "" {
		payload := map[string]any{
			"event":     "renewal_failed",
			"timestamp": time.Now().Unix(),
			"account": map[string]any{
				"id":       acc.ID,
				"openid":   acc.OpenID,
				"uin":      acc.UIN,
				"nickname": acc.Nickname,
			},
			"current": status,
			"reason":  reason,
		}
		body, _ = json.Marshal(payload)
	}
	if url != "" {
		delivered = a.postWebhook(url, body, deref(acc.Nickname))
	}
	title, content := renewalFailureMessage(acc, status, reason)
	return a.pushExtraChannel(title, content) || delivered
}

// postWebhook 是通知发送的唯一实现，返回是否送达。
//
// 失败时立刻再试一次：定时器的下一轮要等 60 秒，而一次网络抖动就足以让掉线告警
// 晚一分钟到达。重复推一条消息的代价，远小于漏掉一条掉线告警，所以这里偏向重试。
// 只有「可重试的失败」（连接错误、5xx）才重试；Webhook 明确拒绝（4xx /
// 企业微信 errcode≠0，例如频率限制）重试也会被同样拒绝，直接交给下一轮。
func (a *App) postWebhook(url string, body []byte, nickname string) bool {
	const attempts = 2
	for attempt := 1; attempt <= attempts; attempt++ {
		ok, retryable := a.postWebhookOnce(url, body, nickname)
		if ok {
			return true
		}
		if !retryable || attempt == attempts {
			return false
		}
		log.Printf("[webhook] 第 %d 次推送失败，%.0f 秒后重试", attempt, webhookRetryBackoff.Seconds())
		time.Sleep(webhookRetryBackoff)
	}
	return false
}

// webhookRetryBackoff 是单次推送失败后的立即重试间隔。
const webhookRetryBackoff = 800 * time.Millisecond

// postWebhookOnce 执行一次推送，同时告诉调用方这次失败是否值得重试。
func (a *App) postWebhookOnce(url string, body []byte, nickname string) (ok bool, retryable bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[webhook] 创建请求失败: %v", err)
		return false, false
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := a.healthClient.Do(req)
	if err != nil {
		log.Printf("[webhook] 推送失败 (%s): %v", sanitizeURL(url), err)
		return false, true
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	ok = resp.StatusCode >= 200 && resp.StatusCode < 300
	if !ok {
		// 5xx 视为接收端暂时不可用，值得立刻重试；4xx 是请求本身的问题
		retryable = resp.StatusCode >= 500
	}

	if len(respBody) > 0 {
		var parsed map[string]any
		if json.Unmarshal(respBody, &parsed) == nil {
			if code, exists := parsed["errcode"]; exists {
				if c, ok2 := toFloat64(code); ok2 && c != 0 {
					msg, _ := parsed["errmsg"].(string)
					ok = false
					retryable = false
					log.Printf("[webhook] 推送被拒绝 (%s): errcode=%d errmsg=%s，HTTP %d",
						nickname, int64(c), msg, resp.StatusCode)
				}
			}
		}
	}

	if ok {
		log.Printf("[webhook] 推送成功 (%s), HTTP %d", nickname, resp.StatusCode)
	} else {
		log.Printf("[webhook] 推送返回异常 (%s): HTTP %d, body=%s",
			sanitizeURL(url), resp.StatusCode, string(respBody))
	}
	return ok, retryable
}

// notifyWebhookExpired 推送「账号已失效」告警，返回是否送达。
//
// 事件名与状态变更区分开（`account_expired`），让接收方能判断这是「刚失效」
// 还是「仍然失效」——后者是周期性重提醒，不是新的状态跃迁。
func (a *App) notifyWebhookExpired(acc *store.WechatAccount) bool {
	delivered := false
	url := a.getWebhookURL()
	var body []byte
	if isWeworkWebhook(url) && url != "" {
		body = buildWeworkExpiredPayload(acc)
	} else if url != "" {
		payload := map[string]any{
			"event":     "account_expired",
			"timestamp": time.Now().Unix(),
			"account": map[string]any{
				"id":       acc.ID,
				"openid":   acc.OpenID,
				"uin":      acc.UIN,
				"nickname": acc.Nickname,
			},
			"current": "expired",
		}
		body, _ = json.Marshal(payload)
	}
	if url != "" {
		delivered = a.postWebhook(url, body, deref(acc.Nickname))
	}
	title, content := expiredMessage(acc)
	return a.pushExtraChannel(title, content) || delivered
}

// retryPendingNotifications 补推上一轮未能送达的状态变更。
//
// 背景：账号一旦被判为 expired，保活与健康检查都会跳过它（终态，救不回来）。
// 如果那次推送恰好失败，状态已经落库、后续再没有任何路径会重算它，
// 告警就永久丢失了。这里每轮扫描比对 status 与 status_notified，
// 把「已收敛但没送达」的变更重新推一次，送达后立即记账，因此不会重复打扰。
func (a *App) retryPendingNotifications(ctx context.Context) {
	if !a.notifyChannelsConfigured() {
		return
	}
	// 健康检查与保活是两条独立的定时器，不加锁时同一秒内可能并发补推同一条记录
	a.notifyRetryMu.Lock()
	defer a.notifyRetryMu.Unlock()

	pending, err := a.db.ListPendingNotifications(ctx)
	if err != nil {
		log.Printf("[notify] 读取待补推列表失败: %v", err)
		return
	}
	for _, item := range pending {
		acc := &store.WechatAccount{ID: item.AccountID, OpenID: item.OpenID, Nickname: item.Nickname, UIN: item.UIN}
		log.Printf("[notify] 补推账号 %s (%s) 的状态变更: %q → %s",
			deref(item.Nickname), item.OpenID, item.NotifiedStatus, item.Status)
		if !a.notifyWebhook(acc, item.NotifiedStatus, item.Status) {
			continue
		}
		if err := a.db.MarkStatusNotified(ctx, item.AccountID, item.Status); err != nil {
			log.Printf("[notify] 记录补推结果失败 (账号 %d): %v", item.AccountID, err)
		}
		// 与 alertExpired 共用同一记账列：这一条已经把「账号失效」送到人了，
		// 本轮再走失效告警时会被重提醒窗口拦住，不会连发两条同义消息。
		if item.Status == "expired" {
			if err := a.db.MarkExpiryAlerted(ctx, item.AccountID, time.Now().Unix()); err != nil {
				log.Printf("[notify] 记录失效告警时间失败 (账号 %d): %v", item.AccountID, err)
			}
		}
	}
}

func refreshOut(acc *store.WechatAccount, status string, err error) map[string]any {
	out := map[string]any{"id": acc.ID, "openid": acc.OpenID, "uin": acc.UIN, "nickname": acc.Nickname, "status": status}
	if err == nil {
		return out
	}
	// 刷新失败必须回传：早期版本把错误丢进 _，接口一律 200 + 空状态，
	// 控制台永远提示「存活刷新完成」，用户点了没变化却查不到原因。
	out["error"] = err.Error()
	// 刷新的诊断同样结构化：脚本据此判断"该重扫还是该重试"，
	// 不必再去解析人话错误串。
	if diagnostics := credentialDiagnostics(err); len(diagnostics) > 0 {
		out["upstream_errors"] = diagnostics
	}
	if definitiveCredentialFailure(err) || status == "expired" {
		out["rescan_required"] = true
	}
	switch {
	case definitiveCredentialFailure(err), status == "expired":
		out["hint"] = "凭据已被微信侧拒绝，需要重新扫码"
	case refreshRejected(err):
		out["hint"] = "微信侧拒绝了刷新请求，稍后自动重试；持续出现通常意味着需要重新扫码"
	default:
		out["hint"] = "刷新未成功，多为网络或代理问题，稍后自动重试"
	}
	return out
}

// pickNickname 从用户信息中提取昵称，若不存在则使用 fallback。
func pickNickname(userInfo map[string]any, fallback string) string {
	if s := stringFromAny(userInfo["nick_name"]); s != "" {
		return s
	}
	return fallback
}

// pickAvatarURL 从用户信息中提取头像 URL。
func pickAvatarURL(userInfo map[string]any) string {
	for _, k := range []string{"head_img_url", "head_url", "headimgurl", "avatar"} {
		if s := stringFromAny(userInfo[k]); s != "" {
			return s
		}
	}
	return ""
}

// resolveAvatar 下载并缓存头像到本地，返回本地路径或远程 URL。
func (a *App) resolveAvatar(ctx context.Context, openid string, userInfo map[string]any) string {
	u := pickAvatarURL(userInfo)
	if u == "" {
		return ""
	}
	dest := a.resources.avatarPath(openid)
	if downloadAvatar(ctx, u, dest, a.cfg.AvatarTimeout) {
		return dest
	}
	return u
}

// downloadAvatar 从远程 URL 下载头像图片，限制最大 8MB，验证是否为常见图片格式。
func downloadAvatar(ctx context.Context, url, dest string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || resp.StatusCode != 200 || !looksLikeImage(data) {
		return false
	}
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	return os.WriteFile(dest, data, 0o644) == nil
}

// looksLikeImage 通过文件头魔数判断数据是否为常见图片格式（JPEG/PNG/GIF）。
func looksLikeImage(data []byte) bool {
	if len(data) < 64 {
		return false
	}
	magics := [][]byte{{0xff, 0xd8, 0xff}, {0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, []byte("GIF87a"), []byte("GIF89a")}
	for _, m := range magics {
		if strings.HasPrefix(string(data), string(m)) {
			return true
		}
	}
	return false
}

// getQRSession 线程安全地获取二维码会话。
func (a *App) getQRSession(id string) *qr.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.qrSessions[id]
}

// getQRClient 返回该扫码会话使用的客户端；找不到时退回全局直连客户端。
func (a *App) getQRClient(id string) *qr.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if client := a.qrClients[id]; client != nil {
		return client
	}
	return a.qr
}

// qrClientForScan 根据账号 ref 选出扫码应使用的客户端。
//
// ref 为空（全新扫码）或账号未配置代理时使用全局直连客户端；
// 代理解析失败同样退回直连，保证扫码入口不会因代理不可用而整体失效。
func (a *App) qrClientForScan(ctx context.Context, ref string) *qr.Client {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return a.qr
	}
	acc, err := a.db.ResolveAccount(ctx, ref)
	if err != nil {
		return a.qr
	}
	proxyValue, fallbackDirect, err := a.resolveAccountProxy(ctx, acc.ID)
	if err != nil || proxyValue == "" {
		return a.qr
	}
	client, err := qr.NewClientWithProxy(a.cfg.RequestTimeout, proxyValue, fallbackDirect)
	if err != nil {
		return a.qr
	}
	return client
}

// dropQRSession 删除二维码会话并清理对应的图片文件与待写入的代理选择。
func (a *App) dropQRSession(id string) {
	a.mu.Lock()
	delete(a.qrSessions, id)
	delete(a.qrClients, id)
	delete(a.qrProxies, id)
	// 链接会话的附加状态必须一起删：留着它会让一条已经回收的二维码
	// 在下次同名会话出现时被误认为是链接会话。
	delete(a.linkSessions, id)
	a.mu.Unlock()
	_ = os.Remove(a.resources.qrPath(id))
}

// cancelQRSession 取消一个二维码会话，并留下短窗口的「已取消」标记。
//
// 与 dropQRSession 的区别：那个只负责回收（确认成功、过期清理也用同一个），
// 不能顺便标成「用户主动取消」，否则正常流程也会被报成取消。
func (a *App) cancelQRSession(id string) {
	a.dropQRSession(id)
	now := time.Now()
	a.mu.Lock()
	// 顺手清理：标记只为迟到几百毫秒的 poll 服务，留一个二维码 TTL 足够。
	for key, at := range a.qrCancelled {
		if now.Sub(at) > a.cfg.QRSessionTTL {
			delete(a.qrCancelled, key)
		}
	}
	a.qrCancelled[id] = now
	a.mu.Unlock()
}

// qrCancelledRecently 判断这个会话是不是刚被主动取消。
func (a *App) qrCancelledRecently(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	at, ok := a.qrCancelled[id]
	if !ok {
		return false
	}
	if time.Since(at) > a.cfg.QRSessionTTL {
		delete(a.qrCancelled, id)
		return false
	}
	return true
}

// qrStatusTerminal 判断二维码是否已进入终态（不可能再有后续动作）。
//
// "unknown" 也算终态：微信侧返回了无法识别的状态，继续轮询或确认都不会成功，
// 与其抱着会话等 TTL，不如尽早回收。
func qrStatusTerminal(status string) bool {
	switch status {
	case "expired", "cancelled", "unknown":
		return true
	}
	return false
}

// finishQRPoll 在轮询结果为终态时回收会话。
func (a *App) finishQRPoll(sessionID, status string) {
	if qrStatusTerminal(status) {
		a.dropQRSession(sessionID)
	}
}

// pruneQR 清理已过期的二维码会话。
func (a *App) pruneQR() {
	a.mu.Lock()
	var drop []string
	for sid, sess := range a.qrSessions {
		if sess.Age() > a.cfg.QRSessionTTL {
			drop = append(drop, sid)
		}
	}
	for _, sid := range drop {
		delete(a.qrSessions, sid)
		delete(a.qrClients, sid)
		delete(a.qrProxies, sid)
	}
	a.mu.Unlock()
	for _, sid := range drop {
		_ = os.Remove(a.resources.qrPath(sid))
	}
}

// cleanupQR 清理不在保留列表中的二维码图片文件。
func (a *App) cleanupQR(keep map[string]bool) {
	files, _ := filepath.Glob(filepath.Join(a.resources.QR, "*.jpg"))
	for _, f := range files {
		sid := strings.TrimSuffix(filepath.Base(f), ".jpg")
		if !keep[sid] {
			_ = os.Remove(f)
		}
	}
}

// apiEnvelope 是统一 API 响应信封，所有接口均遵循此格式。
type apiEnvelope struct {
	Code int    `json:"code"` // 业务状态码，0 表示成功
	Msg  string `json:"msg"`  // 提示信息
	Data any    `json:"data"` // 实际数据载荷
}

// writeJSON 将数据包装在 apiEnvelope 中并以 JSON 格式写入响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	writeRawJSON(w, status, apiEnvelope{
		Code: 0,
		Msg:  "success",
		Data: v,
	})
}

// writeRawJSON 直接写入 JSON（不包装 apiEnvelope），用于 OpenAPI 等特殊场景。
func writeRawJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError 以 apiEnvelope 格式返回错误响应。
func writeError(w http.ResponseWriter, status int, detail string) {
	writeRawJSON(w, status, apiEnvelope{
		Code: status,
		Msg:  detail,
		Data: nil,
	})
}

func serveFileOrText(w http.ResponseWriter, r *http.Request, path, fallback string) {
	if _, err := os.Stat(path); err == nil {
		http.ServeFile(w, r, path)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(fallback))
}

// stringFromAny 安全地从 interface{} 提取字符串值。
func stringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// toFloat64 安全地将 interface{} 转为 float64，用于 JSON 数字解析。
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// itoa 把 int64 格式化为十进制字符串，用于构造 JSON 对象键。
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// stringPtrMaybe 将空字符串转为 nil 指针，非空字符串转为指针。
func stringPtrMaybe(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// deref 安全解引用字符串指针，nil 返回空字符串。
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// safeName 过滤字符串，仅保留字母、数字、短横线和下划线。
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sanitizeURL 脱敏 URL 中的敏感参数（query string），用于日志输出。
func sanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.RawQuery != "" {
		u.RawQuery = "***"
	}
	return u.String()
}
