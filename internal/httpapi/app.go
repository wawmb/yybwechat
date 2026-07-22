package httpapi

import (
	"bytes"
	"context"
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

	"yybwechat/internal/protocol"
	"yybwechat/internal/qr"
	"yybwechat/internal/store"
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
}

// App 是服务的核心实例，聚合了 HTTP 路由、数据库、协议连接池和二维码客户端。
type App struct {
	cfg       Config
	resources resources        // 运行时资源目录
	db        *store.DB        // SQLite 数据库实例
	pool      *protocol.Pool   // MMTLS 协议连接池
	qr        *qr.Client       // 微信二维码登录客户端

	mu         sync.Mutex
	qrSessions map[string]*qr.Session // 活跃的二维码会话

	webhookURL   string          // 当前 Webhook 地址（由 /settings/webhook API 动态读写）
	webhookMu    sync.RWMutex    // 保护 webhookURL 的并发读写
	stopHealth   chan struct{}   // 通知后台健康检查 goroutine 退出
	healthClient *http.Client    // Webhook 推送专用的 HTTP 客户端
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

	return app, nil
}

// Close 关闭 App，释放数据库连接等资源。
func (a *App) Close() error {
	if a.stopHealth != nil {
		close(a.stopHealth)
	}
	if a.db != nil {
		return a.db.Close()
	}
	return nil
}

// Handler 创建并返回配置好所有路由的 http.Handler。
// 包含首页、扫码页、API 文档、账号管理、wxapp 接口代理等路由。
func (a *App) Handler() http.Handler {
	if os.Getenv(gin.EnvGinMode) == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery())

	router.Any("/", gin.WrapF(a.handleIndex))
	router.Any("/scan", gin.WrapF(a.handleScan))
	router.Any("/docs", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/docs/index.html")
	})
	router.Any("/docs/*path", gin.WrapF(a.handleDocs))
	router.Any("/openapi.json", gin.WrapF(a.handleOpenAPI))
	router.Any("/health", func(c *gin.Context) {
		writeJSON(c.Writer, http.StatusOK, gin.H{"ok": true})
	})
	router.StaticFS("/static", http.Dir(a.resources.Static))
	router.Any("/qr", gin.WrapF(a.handleQRRoot))
	router.Any("/qr/*path", gin.WrapF(a.handleQR))
	router.Any("/accounts", gin.WrapF(a.handleAccountsRoot))
	router.Any("/accounts/avatar", gin.WrapF(a.handleAccountAvatar))
	router.Any("/accounts/refresh", gin.WrapF(a.handleAccountRefresh))
	router.Any("/accounts/resync", gin.WrapF(a.handleAccountResync))
	router.Any("/wxapp/getCode", gin.WrapF(a.handleGetCode))
	router.Any("/wxapp/getPhoneNumber", gin.WrapF(a.handleGetPhoneNumber))
	router.Any("/wxapp/operateWxData", gin.WrapF(a.handleOperateWXData))
	router.Any("/settings/webhook", gin.WrapF(a.handleSettingsWebhook))
	router.Any("/settings/webhook/test", gin.WrapF(a.handleWebhookTest))
	router.NoRoute(func(c *gin.Context) {
		writeError(c.Writer, http.StatusNotFound, "not found")
	})

	return router
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
		out["image_base64"] = qr.DataURIJPEG(img.ImageBytes)
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
		result, err := a.qr.GetLoginBuffer(r.Context(), sess)
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
	status, _ := a.refreshLiveness(r.Context(), acc)
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
		status, _ := a.refreshLiveness(context.Background(), acc)
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
// 返回 status ("alive"、"expired" 或 "unknown") 和新的 LoginBuffer
// (仅 status=="alive" 时非空)。
func (a *App) refreshLiveness(ctx context.Context, acc *store.WechatAccount) (status string, newLoginBuffer string) {
	if acc.Credentials == nil {
		_ = a.db.SetAccountStatus(ctx, acc.ID, "unknown")
		return "unknown", ""
	}
	creds := protocol.CredentialsFromMap(acc.Credentials)
	result, err := a.qr.RefreshLoginBuffer(ctx, creds)
	if err != nil {
		_ = a.db.SetAccountStatus(ctx, acc.ID, "expired")
		return "expired", ""
	}
	_ = a.db.SetAccountCredential(ctx, acc.ID, result.LoginBuffer, result.Credentials.ToMap())
	_ = a.db.SetAccountStatus(ctx, acc.ID, "alive")
	if avatar := a.resolveAvatar(ctx, acc.OpenID, acc.UserInfo); avatar != "" {
		_ = a.db.SetAccountProfile(ctx, acc.ID, acc.Nickname, &avatar, acc.UserInfo)
	}
	return "alive", result.LoginBuffer
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
	status, newBuffer := a.refreshLiveness(ctx, acc)
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

		// 跳过无需检查的账号：无凭证或 last_checked_at 仍在有效期内
		if acc.Credentials == nil {
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
		newStatus, _ := a.refreshLiveness(context.Background(), acc)

		if oldStatus != "" && oldStatus != newStatus {
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
