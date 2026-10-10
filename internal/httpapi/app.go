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
	ProtocolToken string // /wxapp/* 的 Bearer 令牌，留空表示不校验

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

	loginLimit loginLimiter // 登录失败限流表

	webhookURL   string        // 当前 Webhook 地址（由 /settings/webhook API 动态读写）
	webhookMu    sync.RWMutex  // 保护 webhookURL 的并发读写
	stopHealth   chan struct{} // 通知后台健康检查 goroutine 退出
	stopKeep     chan struct{} // 通知后台保活 goroutine 退出
	healthClient *http.Client  // Webhook 推送专用的 HTTP 客户端

	keepAliveMu    sync.Mutex          // 保护保活重退避状态表
	keepAliveNext  map[int64]time.Time // 账号 ID → 下次可重试时间（失败退避）
	keepAliveFails map[int64]int       // 账号 ID → 连续失败次数（用于递增退避）
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
		healthClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		keepAliveNext:  map[int64]time.Time{},
		keepAliveFails: map[int64]int{},
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
	open.Any("/api/auth/password", gin.WrapF(a.handleAuthPassword))
	open.Any("/openapi.json", gin.WrapF(a.handleOpenAPI))
	// GET /accounts 免认证且不需要任何令牌：自动化脚本（青龙面板定时任务等）
	// 第一步就是用它探测服务并拉取账号列表，要求脚本先做交互式登录或配置静态令牌
	// 都会直接打断无人值守的调用链。
	// 放开的只是只读：返回 AccountPublic，剔除了 login_buffer 与 credentials。
	// DELETE 等写操作在 handleAccountsRoot 内部二次鉴权，仍然要求管理员登录。
	open.Any("/accounts", gin.WrapF(a.handleAccountsRoot))

	// ---- 2. 协议组（可选令牌门禁）----
	// 统一使用 /wxapp/* 命名空间，不另开平行的兼容别名路径。
	proto := router.Group("/", a.protocolTokenGate())
	proto.Any("/wxapp/getCode", gin.WrapF(a.handleGetCode))
	proto.Any("/wxapp/getPhoneNumber", gin.WrapF(a.handleGetPhoneNumber))
	proto.Any("/wxapp/operateWxData", gin.WrapF(a.handleOperateWXData))
	proto.Any("/wxapp/getUserInfo", gin.WrapF(a.handleGetUserInfo))
	proto.Any("/wxapp/oauth", gin.WrapF(a.handleOAuth))

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

	router.NoRoute(func(c *gin.Context) {
		writeError(c.Writer, http.StatusNotFound, "not found")
	})

	return router
}

// protocolTokenGate 是协议接口的可选 Bearer 令牌门禁。
// 未配置 ProtocolToken 时直接放行（保持内网自托管的易用性）；
// 配置后要求请求携带 Authorization: Bearer <token>，使用常量时间比较防时序攻击。
func (a *App) protocolTokenGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if a.cfg.ProtocolToken == "" {
			c.Next()
			return
		}
		token := bearerToken(c.Request)
		if subtle.ConstantTimeCompare([]byte(token), []byte(a.cfg.ProtocolToken)) != 1 {
			writeError(c.Writer, http.StatusUnauthorized, "invalid or missing protocol token")
			c.Abort()
			return
		}
		c.Next()
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
	img, err := a.qr.GetQRCodeImage(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	a.mu.Lock()
	a.qrSessions[img.Session.ID] = img.Session
	keep := make(map[string]bool, len(a.qrSessions))
	for sid := range a.qrSessions {
		keep[sid] = true
	}
	a.mu.Unlock()
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
		result, err := a.qr.PollQRCode(r.Context(), sess)
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
		// 微信授权已完成但 login buffer 可能尚未就绪，短窗口内重试吸收这段竞态
		result, err := getLoginBufferWithRetry(r.Context(), a.qr, sess)
		if err != nil {
			writeError(w, http.StatusConflict, "buffer not ready: "+err.Error())
			return
		}
		var userInfo map[string]any
		if ui, err := a.qr.LoginBuffers().FetchUserInfo(r.Context(), result.Credentials); err == nil {
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

type wxappCall func(ctx context.Context, acc *store.WechatAccount, appID string, payload map[string]any) (map[string]any, error)

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
func (a *App) storeFromScan(ctx context.Context, loginBuffer string, creds protocol.LoginBufferCredentials, userInfo map[string]any) (*store.WechatAccount, error) {
	openid := creds.OpenID
	nick := pickNickname(userInfo, creds.Nickname)
	avatar := a.resolveAvatar(ctx, openid, userInfo)
	status := "alive"
	return a.db.UpsertAccount(ctx, openid, loginBuffer, stringPtrMaybe(nick), stringPtrMaybe(nick), stringPtrMaybe(avatar), userInfo, creds.ToMap(), &status)
}

// refreshLiveness 刷新单个账号的存活状态。
//
// 返回 status ("alive"、"expired" 或 "unknown")、新的 LoginBuffer
// (仅 status=="alive" 时非空)，以及刷新过程中的错误。
//
// 状态判定规则（与 definitiveCredentialFailure 配合）：
//   - "alive"   ：刷新成功；
//   - "expired" ：刷新失败，且错误明确表明凭据已彻底失效，必须重新扫码；
//   - "unknown" ：刷新失败但属于网络抖动、超时等瞬时错误。此时保留原状态与 MMTLS
//     会话等待下一轮重试，避免把一次抖动误判成掉线并推送告警。
//
// 注意：只有 "expired" 会清理 MMTLS 会话；"unknown" 不清理，因为旧会话可能仍然可用。
func (a *App) refreshLiveness(ctx context.Context, acc *store.WechatAccount) (status string, newLoginBuffer string, err error) {
	if acc.Credentials == nil {
		_ = a.db.SetAccountStatus(ctx, acc.ID, "unknown")
		return "unknown", "", errors.New("account has no credentials")
	}
	creds := protocol.CredentialsFromMap(acc.Credentials)
	result, refreshErr := a.qr.RefreshLoginBuffer(ctx, creds)
	if refreshErr != nil {
		if definitiveCredentialFailure(refreshErr) {
			_ = a.db.SetAccountStatus(ctx, acc.ID, "expired")
			// 凭据已彻底失效，清理会话，避免后续请求继续复用过期会话
			a.invalidateAccountSessions(ctx, acc.ID)
			return "expired", "", refreshErr
		}
		// 瞬时错误：不改写为 expired，也不清理会话，等待下一轮重试自然恢复
		_ = a.db.SetAccountStatus(ctx, acc.ID, "unknown")
		return "unknown", "", refreshErr
	}
	_ = a.db.SetAccountCredential(ctx, acc.ID, result.LoginBuffer, result.Credentials.ToMap())
	// 凭据已更新，旧 MMTLS 会话不再有效，必须清理以触发重新登录
	a.invalidateAccountSessions(ctx, acc.ID)
	_ = a.db.SetAccountStatus(ctx, acc.ID, "alive")
	if avatar := a.resolveAvatar(ctx, acc.OpenID, acc.UserInfo); avatar != "" {
		_ = a.db.SetAccountProfile(ctx, acc.ID, acc.Nickname, &avatar, acc.UserInfo)
	}
	return "alive", result.LoginBuffer, nil
}

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
func (a *App) invokeWXApp(ctx context.Context, acc *store.WechatAccount, appID string, payload map[string]any, call wxappCall) (map[string]any, error) {
	proxy := a.cfg.TCPProxy
	if _, err := a.db.GetSession(ctx, acc.ID, proxy); err == nil {
		result, err := call(ctx, acc, appID, payload)
		if err == nil {
			return result, nil
		}
		_ = a.db.InvalidateSession(ctx, acc.ID, proxy)
	}
	status, newBuffer, _ := a.refreshLiveness(ctx, acc)
	if status != "alive" {
		return nil, accountExpiredError{openid: acc.OpenID}
	}
	// 直接使用刷新后的 LoginBuffer，避免因 GetAccount 失败而使用过期凭据
	if newBuffer != "" {
		acc.LoginBuffer = newBuffer
	}
	return call(ctx, acc, appID, payload)
}

func (a *App) invokeGetCode(ctx context.Context, acc *store.WechatAccount, appID string, _ map[string]any) (map[string]any, error) {
	return a.pool.GetCode(ctx, acc.LoginBuffer, appID, acc.ID, a.cfg.TCPProxy)
}

func (a *App) invokeGetPhoneNumber(ctx context.Context, acc *store.WechatAccount, appID string, _ map[string]any) (map[string]any, error) {
	return a.pool.GetPhoneNumber(ctx, acc.LoginBuffer, appID, acc.ID, a.cfg.TCPProxy)
}

func (a *App) invokeOperateWXData(ctx context.Context, acc *store.WechatAccount, appID string, payload map[string]any) (map[string]any, error) {
	return a.pool.OperateWXData(ctx, acc.LoginBuffer, appID, payload, acc.ID, a.cfg.TCPProxy)
}

// ---- Webhook 推送辅助 ----

// isWeworkWebhook 检测 URL 是否为企业微信机器人 Webhook 地址。
func isWeworkWebhook(url string) bool {
	return strings.Contains(url, "qyapi.weixin.qq.com/cgi-bin/webhook/send")
}

// buildWeworkPayload 构建企业微信 markdown_v2 消息格式的推送内容。
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

// scanAndCheck 扫描所有账号，对超时未检查的账号执行刷新，并在状态变更时推送 Webhook。
func (a *App) scanAndCheck() {
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
		if deref(acc.Status) == "expired" {
			continue
		}
		lastChecked := int64(0)
		if acc.LastCheckedAt != nil {
			lastChecked = *acc.LastCheckedAt
		}
		if lastChecked > staleThreshold {
			continue
		}

		oldStatus := deref(acc.Status)
		newStatus, _, refreshErr := a.refreshLiveness(context.Background(), acc)

		if refreshErr != nil {
			// 瞬时错误会归为 unknown 并等待重试，这里只记录原因，不推送告警
			log.Printf("[health] 账号 %s (%s) 刷新失败: %v",
				deref(acc.Nickname), acc.OpenID, refreshErr)
		}
		if shouldNotifyStatusChange(oldStatus, newStatus) {
			log.Printf("[health] 账号 %s (%s) 状态变更: %s → %s",
				deref(acc.Nickname), acc.OpenID, oldStatus, newStatus)
			a.notifyWebhook(acc, oldStatus, newStatus)
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
func (a *App) notifyWebhook(acc *store.WechatAccount, oldStatus, newStatus string) {
	url := a.getWebhookURL()
	if url == "" {
		return
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[webhook] 创建请求失败: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := a.healthClient.Do(req)
	if err != nil {
		log.Printf("[webhook] 推送失败 (%s): %v", url, err)
		return
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
						deref(acc.Nickname), int64(c), msg, resp.StatusCode)
				}
			}
		}
	}

	if ok {
		log.Printf("[webhook] 推送成功 (%s): %s → %s, HTTP %d",
			deref(acc.Nickname), oldStatus, newStatus, resp.StatusCode)
	} else {
		log.Printf("[webhook] 推送返回异常 (%s): HTTP %d, body=%s",
			url, resp.StatusCode, string(respBody))
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

// dropQRSession 删除二维码会话并清理对应的图片文件。
func (a *App) dropQRSession(id string) {
	a.mu.Lock()
	delete(a.qrSessions, id)
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
