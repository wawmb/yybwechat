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
	"yybwechat/internal/protocol"
	"yybwechat/internal/qr"
	"yybwechat/internal/store"
	"yybwechat/internal/version"
)

// Config 是 App 的配置参数，由 main 函数传入。
type Config struct {
	ResourceRoot   string        // 模板和静态资源根目录
	DataRoot       string        // 持久化数据根目录（db、avatars、qr）
	DBFilename     string        // SQLite 数据库文件名
	TCPProxy       string        // 可选的 TCP 代理地址
	SessionTTL     time.Duration // 登录会话有效期
	RequestTimeout time.Duration // 短连接（ShortLink）请求超时
	AvatarTimeout  time.Duration // 下载头像的 HTTP 超时
	QRSessionTTL   time.Duration // 二维码会话过期时间
	CheckInterval  time.Duration // 后台健康检查间隔（0 禁用）
	CheckStale     time.Duration // 超过此时间未检查视为过期，触发刷新

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
}

// App 是服务的核心实例，聚合了 HTTP 路由、数据库、协议连接池和二维码客户端。
type App struct {
	cfg       Config
	resources resources      // 运行时资源目录
	db        *store.DB      // SQLite 数据库实例
	pool      *protocol.Pool // MMTLS 协议连接池
	qr        *qr.Client     // 微信二维码登录客户端
	auth      *auth.Store    // 管理员认证库（独立 auth.db）

	mu         sync.Mutex
	qrSessions map[string]*qr.Session // 活跃的二维码会话
	qrClients  map[string]*qr.Client  // 会话 ID → 该会话使用的客户端（可能绑定了账号代理）

	loginLimit loginLimiter // 登录失败限流表

	webhookURL   string        // 当前 Webhook 地址（由 /settings/webhook API 动态读写）
	webhookMu    sync.RWMutex  // 保护 webhookURL 的并发读写
	stopHealth   chan struct{} // 通知后台健康检查 goroutine 退出
	stopKeep     chan struct{} // 通知后台保活 goroutine 退出
	healthClient *http.Client  // Webhook 推送专用的 HTTP 客户端

	// notifyRetryMu 让补推流程串行执行。健康检查与保活是两个独立的 ticker，
	// 同一秒内可能同时进入补推，导致同一条告警重复推送两次。
	notifyRetryMu sync.Mutex

	keepAliveMu    sync.Mutex          // 保护保活重退避状态表
	keepAliveNext  map[int64]time.Time // 账号 ID → 下次可重试时间（失败退避）
	keepAliveFails map[int64]int       // 账号 ID → 连续失败次数（用于递增退避）

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
	res, err := ensureResources(cfg.ResourceRoot, cfg.DataRoot)
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
	pool := protocol.NewPool(poolCfg, db)
	app := &App{
		cfg:        cfg,
		resources:  res,
		db:         db,
		pool:       pool,
		qr:         qr.NewClient(cfg.RequestTimeout),
		qrSessions: map[string]*qr.Session{},
		qrClients:  map[string]*qr.Client{},
		healthClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		keepAliveNext:   map[int64]time.Time{},
		keepAliveFails:  map[int64]int{},
		proxyLeases:     map[int64]accountProxyLease{},
		proxyLeaseLocks: map[int64]chan struct{}{},
		refreshLocks:    map[int64]chan struct{}{},
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

	// 从数据库加载 webhook URL
	if url, err := db.GetSetting(context.Background(), "webhook_url"); err == nil && url != "" {
		app.webhookURL = url
		log.Printf("[webhook] 已加载 Webhook 地址: %s", sanitizeURL(url))
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
		log.Printf("[keepalive] 凭据保活已启动，间隔=%s，提前量=%s", cfg.KeepAliveInterval, cfg.KeepAliveAhead)
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
	router.Use(gin.Logger(), gin.Recovery())

	// ---- 1. 公开组 ----
	open := router.Group("/")
	open.Any("/health", func(c *gin.Context) {
		writeJSON(c.Writer, http.StatusOK, gin.H{"ok": true})
	})
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
	// /wx/oauth 是早期脚本使用的路径。转发到同一个处理器，
	// 因此两者的响应格式、校验规则完全一致，不会出现「同一个能力两种行为」。
	proto.Any("/wx/oauth", gin.WrapF(a.handleOAuth))
	// GET /accounts 免登录但受协议令牌保护：自动化脚本（青龙面板定时任务等）
	// 第一步就是用它探测服务并拉取账号列表，要求脚本先做交互式登录会直接打断
	// 无人值守的调用链，因此这里只要求可选的 Bearer 令牌（未配置时直接放行）。
	// 返回 AccountPublic，剔除了 login_buffer 与 credentials。
	// DELETE 等写操作在 handleAccountsRoot 内部二次鉴权，仍然要求管理员登录。
	proto.Any("/accounts", gin.WrapF(a.handleAccountsRoot))

	// ---- 3. 管理组（需登录）----
	secure := router.Group("/", a.requireBrowserSession())
	secure.Any("/", gin.WrapF(a.handleIndex))
	secure.Any("/scan", gin.WrapF(a.handleScan))
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
	secure.Any("/settings/webhook", gin.WrapF(a.handleSettingsWebhook))
	secure.Any("/settings/webhook/test", gin.WrapF(a.handleWebhookTest))
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
func (a *App) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ver, commit, buildDate := version.Info()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    ver,
		"commit":     commit,
		"build_date": buildDate,
	})
}

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
	if r.URL.Path != "/qr" {
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
	// 带上 ref 时为已有账号重新扫码：沿用该账号的代理出口，
	// 避免「用 A 地 IP 登录、之后用 B 地 IP 保活」导致微信侧风控。
	client := a.qrClientForScan(ctx, r.URL.Query().Get("ref"))
	img, err := client.GetQRCodeImage(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	a.mu.Lock()
	a.qrSessions[img.Session.ID] = img.Session
	a.qrClients[img.Session.ID] = client
	keep := make(map[string]bool, len(a.qrSessions))
	for sid := range a.qrSessions {
		keep[sid] = true
	}
	a.mu.Unlock()
	if !isSafeFileToken(img.Session.ID) {
		writeError(w, http.StatusBadGateway, "二维码会话标识异常，已拒绝写入")
		return
	}
	path := a.resources.qrPath(img.Session.ID)
	_ = os.WriteFile(path, img.ImageBytes, 0o644)
	a.cleanupQR(keep)
	out := map[string]any{
		"session_id": img.Session.ID,
		"status":     img.Session.Status,
		"image_url":  "/qr/" + img.Session.ID + "/image",
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
func (a *App) handleQR(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/qr/"), "/")
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
	case "poll":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		sess := a.getQRSession(sessionID)
		if sess == nil {
			writeError(w, http.StatusNotFound, "qr session not found")
			return
		}
		result, err := a.getQRClient(sessionID).PollQRCode(r.Context(), sess)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
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
		acc, err := a.storeFromScan(r.Context(), result.LoginBuffer, result.Credentials, userInfo)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		a.dropQRSession(sessionID)
		writeJSON(w, http.StatusOK, acc.Public())
	default:
		writeError(w, http.StatusNotFound, "qr session not found")
	}
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
		out := make([]store.AccountPublic, 0, len(accounts))
		for _, acc := range accounts {
			out = append(out, acc.Public())
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
	status, _, _ := a.refreshLiveness(r.Context(), acc)
	writeJSON(w, http.StatusOK, refreshOut(acc, status))
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
	if !acceptWXAppRoute(w, r, "/wxapp/getCode") {
		return
	}
	a.callWXApp(w, r, false, a.invokeGetCode)
}

func (a *App) handleGetPhoneNumber(w http.ResponseWriter, r *http.Request) {
	if !acceptWXAppRoute(w, r, "/wxapp/getPhoneNumber") {
		return
	}
	a.callWXApp(w, r, false, a.invokeGetPhoneNumber)
}

func (a *App) handleOperateWXData(w http.ResponseWriter, r *http.Request) {
	if !acceptWXAppRoute(w, r, "/wxapp/operateWxData") {
		return
	}
	a.callWXApp(w, r, true, a.invokeOperateWXData)
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
	result, err := a.invokeWXApp(r.Context(), acc, body.AppID, body.Payload, call)
	if err != nil {
		var expired accountExpiredError
		switch {
		case errors.As(err, &expired):
			writeError(w, http.StatusConflict, "account login_buffer expired (refresh failed); re-scan required")
		default:
			writeError(w, http.StatusBadGateway, "call failed: "+err.Error())
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
		status, _, _ := a.refreshLiveness(context.Background(), acc)
		out = append(out, refreshOut(acc, status))
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
	return a.db.UpsertAccount(ctx, openid, loginBuffer, stringPtrMaybe(nick), stringPtrMaybe(nick), stringPtrMaybe(avatar), userInfo, creds.ToMapForScan(), &status)
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

type accountExpiredError struct{ openid string }

func (e accountExpiredError) Error() string { return "account expired: " + e.openid }

// invokeWXApp 通过 MMTLS 协议连接调用 wxapp 接口，自动处理会话失效重试。
//
// 关键点：只有「明确的凭据失效」才允许把账号判死；纯网络/代理故障只丢弃会话缓存，
// 否则一次代理抖动就会把所有账号刷成 expired，并触发一轮误报告警。
func (a *App) invokeWXApp(ctx context.Context, acc *store.WechatAccount, appID string, payload map[string]any, call wxappCall) (map[string]any, error) {
	// 把排队、协议登录和凭据补救都算进同一份预算，避免层层超时叠加到客户端放弃之后
	ctx, cancel := context.WithTimeout(ctx, a.cfg.RequestTimeout+35*time.Second)
	defer cancel()
	if accountStatus(acc) == "expired" {
		// 脚本打到失效账号上时顺手告警：这条调用链本来就会被跳过，
		// 不借它发声，就可能只剩日志里一行错误
		a.alertExpired(acc)
		return nil, accountExpiredError{openid: acc.OpenID}
	}
	if accountNeedsRescan(acc) {
		return nil, accountExpiredError{openid: acc.OpenID}
	}
	proxyValue, fallbackDirect, err := a.resolveWXAppProxy(ctx, acc.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve account proxy: %w", err)
	}
	result, callErr := call(ctx, acc, appID, payload, proxyValue, fallbackDirect)
	if callErr == nil {
		return result, nil
	}
	var networkErr net.Error
	if ctx.Err() != nil || errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded) ||
		errors.As(callErr, &networkErr) || errors.Is(callErr, io.EOF) || errors.Is(callErr, io.ErrUnexpectedEOF) {
		// 传输层失败不能证明凭据过期：保留会话与原始诊断信息，也不重放这条慢调用
		return nil, callErr
	}
	_ = a.db.InvalidateSession(ctx, acc.ID, proxyValue)
	// 凭据补救始终走账号自己的刷新通道，即使本次协议调用显式选择了直连
	status, _, refreshErr := a.refreshAccount(ctx, acc, true)
	if status != "alive" {
		if status == "expired" {
			return nil, accountExpiredError{openid: acc.OpenID}
		}
		return nil, fmt.Errorf("refresh account credentials: %w", refreshErr)
	}
	if refreshErr != nil {
		return nil, fmt.Errorf("refresh account credentials: %w", refreshErr)
	}
	if fresh, getErr := a.db.GetAccount(ctx, acc.ID); getErr == nil && fresh != nil {
		acc = fresh
	}
	return call(ctx, acc, appID, payload, proxyValue, fallbackDirect)
}

// accountNeedsRescan 覆盖「刷新结论不明、但缓存凭据已经过期」的账号。
// 这类账号不能继续被每次脚本调用拖进协议层重试，必须先重新扫码。
func accountNeedsRescan(acc *store.WechatAccount) bool {
	if acc == nil || accountStatus(acc) != "unknown" || acc.Credentials == nil {
		return false
	}
	expiresAt := protocol.CredentialsFromMap(acc.Credentials).ExpiresAt
	return expiresAt > 0 && expiresAt <= time.Now().Unix()
}

func (a *App) invokeGetCode(ctx context.Context, acc *store.WechatAccount, appID string, _ map[string]any, proxyValue string, fallbackDirect bool) (map[string]any, error) {
	return a.pool.GetCode(ctx, acc.LoginBuffer, appID, acc.ID, proxyValue, fallbackDirect)
}

func (a *App) invokeGetPhoneNumber(ctx context.Context, acc *store.WechatAccount, appID string, _ map[string]any, proxyValue string, fallbackDirect bool) (map[string]any, error) {
	return a.pool.GetPhoneNumber(ctx, acc.LoginBuffer, appID, acc.ID, proxyValue, fallbackDirect)
}

func (a *App) invokeOperateWXData(ctx context.Context, acc *store.WechatAccount, appID string, payload map[string]any, proxyValue string, fallbackDirect bool) (map[string]any, error) {
	return a.pool.OperateWXData(ctx, acc.LoginBuffer, appID, payload, acc.ID, proxyValue, fallbackDirect)
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
	url := a.getWebhookURL()
	if url == "" {
		return false
	}

	var body []byte
	if isWeworkWebhook(url) {
		body = buildWeworkPayload("status_changed", oldStatus, newStatus, acc)
	} else {
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
	return a.postWebhook(url, body, deref(acc.Nickname))
}

// notifyWebhookRenewal 推送「凭据已过期且续不上」的告警。
//
// 与状态变更通知分开，是因为它的语义不同：账号此刻确实不可用了，
// 但还留在 unknown 继续重试（网络或代理恢复后能自愈），不能写成终态误导运营者去重扫。
func (a *App) notifyWebhookRenewal(acc *store.WechatAccount, status, reason string) bool {
	url := a.getWebhookURL()
	if url == "" {
		return false
	}

	var body []byte
	if isWeworkWebhook(url) {
		body = buildWeworkRenewalPayload(acc, status, reason)
	} else {
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
	return a.postWebhook(url, body, deref(acc.Nickname))
}

// postWebhook 是通知发送的唯一实现，返回是否送达。
func (a *App) postWebhook(url string, body []byte, nickname string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[webhook] 创建请求失败: %v", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := a.healthClient.Do(req)
	if err != nil {
		log.Printf("[webhook] 推送失败 (%s): %v", sanitizeURL(url), err)
		return false
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300

	if len(respBody) > 0 {
		var parsed map[string]any
		if json.Unmarshal(respBody, &parsed) == nil {
			if code, exists := parsed["errcode"]; exists {
				if c, ok2 := toFloat64(code); ok2 && c != 0 {
					msg, _ := parsed["errmsg"].(string)
					ok = false
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
	return ok
}

// notifyWebhookExpired 推送「账号已失效」告警，返回是否送达。
//
// 事件名与状态变更区分开（`account_expired`），让接收方能判断这是「刚失效」
// 还是「仍然失效」——后者是周期性重提醒，不是新的状态跃迁。
func (a *App) notifyWebhookExpired(acc *store.WechatAccount) bool {
	url := a.getWebhookURL()
	if url == "" {
		return false
	}
	var body []byte
	if isWeworkWebhook(url) {
		body = buildWeworkExpiredPayload(acc)
	} else {
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
	return a.postWebhook(url, body, deref(acc.Nickname))
}

// retryPendingNotifications 补推上一轮未能送达的状态变更。
//
// 背景：账号一旦被判为 expired，保活与健康检查都会跳过它（终态，救不回来）。
// 如果那次推送恰好失败，状态已经落库、后续再没有任何路径会重算它，
// 告警就永久丢失了。这里每轮扫描比对 status 与 status_notified，
// 把「已收敛但没送达」的变更重新推一次，送达后立即记账，因此不会重复打扰。
func (a *App) retryPendingNotifications(ctx context.Context) {
	if a.getWebhookURL() == "" {
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

func refreshOut(acc *store.WechatAccount, status string) map[string]any {
	return map[string]any{"id": acc.ID, "openid": acc.OpenID, "uin": acc.UIN, "nickname": acc.Nickname, "status": status}
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

// dropQRSession 删除二维码会话并清理对应的图片文件。
func (a *App) dropQRSession(id string) {
	a.mu.Lock()
	delete(a.qrSessions, id)
	delete(a.qrClients, id)
	a.mu.Unlock()
	_ = os.Remove(a.resources.qrPath(id))
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
