package httpapi

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"yybwechat/internal/auth"
	"yybwechat/internal/version"
)

// sessionCookie 是管理员会话 Cookie 的名称。
const sessionCookie = "yyb_session"

// authUserContextKey 是存放当前登录管理员的 context key 类型，避免与其它包冲突。
type authUserContextKey struct{}

// 登录失败限流的阈值与窗口。
const (
	loginMaxFailures = 8                // 同一 IP 允许的最大连续失败次数
	loginFailWindow  = 15 * time.Minute // 失败计数的时间窗口
	loginAttemptsMax = 512              // 限流表的最大条目数，防止内存无界增长
)

// loginAttempt 记录单个 IP 的登录失败情况。
type loginAttempt struct {
	Failures int
	Start    time.Time
}

// loginLimiter 是按 IP 限流的登录失败记录表。
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*loginAttempt
}

// ensure 惰性初始化记录表，使 loginLimiter 的零值即可直接使用。
func (l *loginLimiter) ensure() {
	if l.attempts == nil {
		l.attempts = make(map[string]*loginAttempt)
	}
}

// allow 判断该 IP 是否允许继续尝试登录。
func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.attempts[ip]
	if !ok {
		return true
	}
	// 窗口过期则重新开始计数
	if time.Since(a.Start) > loginFailWindow {
		delete(l.attempts, ip)
		return true
	}
	return a.Failures < loginMaxFailures
}

// recordFailure 记录一次登录失败。
func (l *loginLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensure()
	if len(l.attempts) >= loginAttemptsMax && l.attempts[ip] == nil {
		// 表已满时进行一轮清理，淘汰所有过期窗口的条目
		for k, v := range l.attempts {
			if time.Since(v.Start) > loginFailWindow {
				delete(l.attempts, k)
			}
		}
	}
	a, ok := l.attempts[ip]
	if !ok || time.Since(a.Start) > loginFailWindow {
		l.attempts[ip] = &loginAttempt{Failures: 1, Start: time.Now()}
		return
	}
	a.Failures++
}

// reset 清除指定 IP 的失败记录（登录成功时调用）。
func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.attempts == nil {
		return
	}
	delete(l.attempts, ip)
}

// currentUser 从请求上下文中取出已登录的管理员，未登录返回 nil。
// 仅对经过 requireBrowserSession 中间件的路由有效。
func currentUser(r *http.Request) *auth.User {
	if r == nil {
		return nil
	}
	u, _ := r.Context().Value(authUserContextKey{}).(*auth.User)
	return u
}

// currentAdmin 获取请求对应的管理员，未登录或令牌失效返回 nil。
// 与 currentUser 的区别：它会主动校验 Cookie/Authorization 中的令牌，
// 因此可用于登录页、初始化页这类不经过鉴权中间件的公开路由。
func (a *App) currentAdmin(r *http.Request) *auth.User {
	if a.auth == nil || r == nil {
		return nil
	}
	if u := currentUser(r); u != nil {
		return u
	}
	token := sessionTokenFromRequest(r)
	if token == "" {
		return nil
	}
	u, err := a.auth.UserBySession(r.Context(), token)
	if err != nil {
		return nil
	}
	return u
}

// requireBrowserSession 是管理类路由的登录中间件。
//
// 只认管理员会话令牌（Cookie 或 Authorization 头），不认任何静态令牌。
// 这里刻意不为脚本开后门：脚本需要的只读能力（GET /accounts 拉取账号列表）
// 已经注册在公开组，本身不需要任何凭据；真正会改动数据的管理接口
// （删除账号、刷新、重同步、改备注、Webhook 设置等）一律要求管理员登录。
//
// 未通过时按请求类型分流：机器调用（/api/*、/accounts*、/qr/*、/settings*）
// 返回 401 JSON，浏览器页面访问则重定向到登录页或初始化页。
// 注意 gin 的 Use 只对注册位置之后的路由生效，因此本中间件必须在公开路由之后注册。
func (a *App) requireBrowserSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		if a.auth == nil {
			c.Next()
			return
		}
		token := sessionTokenFromRequest(c.Request)
		if token != "" {
			if u, err := a.auth.UserBySession(c.Request.Context(), token); err == nil && u != nil {
				c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), authUserContextKey{}, u))
				c.Next()
				return
			}
			// 令牌无效或已过期，清理 Cookie 避免反复查询
			clearSessionCookie(c.Writer, a.cfg.CookieSecure)
		}
		if isManagementAPI(c.Request.URL.Path) {
			writeError(c.Writer, http.StatusUnauthorized, "请先登录")
			c.Abort()
			return
		}
		target := "/login"
		if ok, _ := a.auth.HasAdmin(c.Request.Context()); !ok {
			target = "/setup"
		}
		http.Redirect(c.Writer, c.Request, target, http.StatusSeeOther)
		c.Abort()
	}
}

// isAuthenticated 判断请求是否处于已登录的管理员会话中。
//
// 用途是给「注册在公开组、但内部含写操作」的路由做二次鉴权（例如 /accounts 的 DELETE）。
// 这类路由的 GET 必须免认证才能兼容自动化脚本，但绝不能因此把写操作也一起放开，
// 所以写操作仍然只认管理员会话，不接受任何静态令牌。
func (a *App) isAuthenticated(r *http.Request) bool {
	return a.currentAdmin(r) != nil
}

// sessionTokenFromRequest 依次从 Cookie 和 Authorization 头中提取会话令牌。
// 支持 Authorization 是为了让脚本能直接调用管理接口。
func sessionTokenFromRequest(r *http.Request) string {
	if ck, err := r.Cookie(sessionCookie); err == nil && ck.Value != "" {
		return ck.Value
	}
	if v := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(v), "bearer ") {
		return strings.TrimSpace(v[len("bearer "):])
	}
	return ""
}

// setSessionCookie 下发会话 Cookie。HttpOnly + SameSite=Lax 防止脚本读取与跨站携带。
func setSessionCookie(w http.ResponseWriter, token string, ttl time.Duration, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// clearSessionCookie 立即过期会话 Cookie。
func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// isManagementAPI 判断路径是否属于机器调用接口。这类接口未登录时返回 401 而非跳转。
func isManagementAPI(path string) bool {
	if strings.HasPrefix(path, "/api/") {
		return true
	}
	for _, p := range []string{"/accounts", "/qr", "/settings", "/wxapp/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// handleLogin 返回登录页面。已登录时直接跳转首页。
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.auth == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// 已登录用户无需再看登录页，直接回到控制台
	if a.currentAdmin(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	initialized, err := a.auth.HasAdmin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !initialized {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	serveFileOrText(w, r, a.templatePath("login.html"), fallbackLoginHTML)
}

// handleSetup 返回首次启动的管理员初始化页面。
// 管理员已存在时重定向到登录页，避免被重复执行。
func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.auth == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	initialized, err := a.auth.HasAdmin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if initialized {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	serveFileOrText(w, r, a.templatePath("setup.html"), fallbackSetupHTML)
}

// handleAuthStatus 返回认证状态，供前端判断应展示登录页还是初始化页。
func (a *App) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.writeAuthStatus(w, r)
}

func (a *App) writeAuthStatus(w http.ResponseWriter, r *http.Request) {
	if a.auth == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false, "initialized": true, "authenticated": true,
		})
		return
	}
	initialized, err := a.auth.HasAdmin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	u := a.currentAdmin(r)
	ver, commit, buildDate := version.Info()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":       true,
		"initialized":   initialized,
		"authenticated": u != nil,
		"username":      usernameOrEmpty(u),
		"version":       ver,
		"commit":        commit,
		"build_date":    buildDate,
	})
}

// handleAuthSetup 处理管理员初始化。仅当系统尚无管理员时允许执行。
func (a *App) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "authentication is disabled")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" {
		writeError(w, http.StatusBadRequest, "username is required")
		return
	}
	if len(body.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	u, err := a.auth.CreateAdmin(r.Context(), body.Username, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrAdminExists) {
			writeError(w, http.StatusConflict, "administrator already exists")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("[auth] 管理员账号已创建: user=%s ip=%s", u.Username, clientIP(r))
	// 初始化后直接下发会话，省去一次登录
	a.issueSession(w, r, u)
}

// handleAuthLogin 校验口令并下发会话 Cookie。
func (a *App) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "authentication is disabled")
		return
	}
	ip := clientIP(r)
	if !a.loginLimit.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "登录失败次数过多，请稍后再试")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	u, err := a.auth.Authenticate(r.Context(), body.Username, body.Password)
	if err != nil {
		a.loginLimit.recordFailure(ip)
		if errors.Is(err, auth.ErrInvalidCredentials) {
			// 统一提示，不区分用户名不存在与口令错误
			writeError(w, http.StatusUnauthorized, "用户名或密码错误")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.loginLimit.reset(ip)
	log.Printf("[auth] 登录成功: user=%s ip=%s", u.Username, ip)
	a.issueSession(w, r, u)
}

// issueSession 创建会话、下发 Cookie 并返回管理员信息。
func (a *App) issueSession(w http.ResponseWriter, r *http.Request, u *auth.User) {
	token, err := a.auth.CreateSession(r.Context(), u.ID, r.UserAgent(), clientIP(r), a.cfg.SessionDuration)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建会话失败: "+err.Error())
		return
	}
	setSessionCookie(w, token, a.cfg.SessionDuration, a.cfg.CookieSecure)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"username":      u.Username,
		"expires_in":    int(a.cfg.SessionDuration.Seconds()),
	})
}

// handleAuthLogout 注销当前会话。
func (a *App) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.auth == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	token := sessionTokenFromRequest(r)
	if token != "" {
		_ = a.auth.DeleteSession(r.Context(), token)
	}
	clearSessionCookie(w, a.cfg.CookieSecure)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAuthMe 返回当前登录管理员的信息。
func (a *App) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.writeAuthStatus(w, r)
}

// handleAuthPassword 修改管理员口令，成功后踢掉其它会话。
func (a *App) handleAuthPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	u := currentUser(r)
	if u == nil || a.auth == nil {
		writeError(w, http.StatusUnauthorized, "请先登录")
		return
	}
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.OldPassword == "" || body.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "old_password and new_password are required")
		return
	}
	if err := a.auth.ChangePassword(r.Context(), u.ID, body.OldPassword, body.NewPassword); err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			writeError(w, http.StatusUnauthorized, "当前密码不正确")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 口令变更后让其它设备重新登录，仅保留当前会话
	if n, err := a.auth.DeleteOtherSessions(r.Context(), u.ID, sessionTokenFromRequest(r)); err == nil && n > 0 {
		log.Printf("[auth] 口令已修改: user=%s ip=%s，已下线其它 %s 个会话",
			u.Username, clientIP(r), strconv.FormatInt(n, 10))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// clientIP 提取客户端 IP。部署在反向代理后时信任 X-Forwarded-For 的首个地址。
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if idx := strings.IndexByte(v, ','); idx > 0 {
			return strings.TrimSpace(v[:idx])
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// templatePath 返回模板目录下指定文件的路径。
func (a *App) templatePath(name string) string {
	return filepath.Join(a.resources.Templates, name)
}

func usernameOrEmpty(u *auth.User) string {
	if u == nil {
		return ""
	}
	return u.Username
}
