package httpapi

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"yybwechat/internal/store"
)

// 「更新链接」相关常量。
//
// 更新类链接默认只有 10 分钟：它的用途是「把手机递给别人扫一下」，不是长期凭证，
// 时间越短误用窗口越小。新增类宽松一些（30 分钟），因为可能要先装应用、登录微信。
const (
	accountLinkKindUpdate       = "update"
	accountLinkKindAdd          = "add"
	accountLinkUpdateDefaultTTL = 10 * time.Minute
	accountLinkAddDefaultTTL    = 30 * time.Minute
	accountLinkMaxTTL           = 7 * 24 * time.Hour

	// accountLinkCleanupInterval 决定过期链接多久被真正删除。
	// 链接本身在 expires_at 之后立即失效（查询时比对时间），这个间隔只影响清理。
	accountLinkCleanupInterval = 10 * time.Minute

	// accountLinkKeySetting 是链接明文副本的加密密钥在 settings 表里的键名。
	// 首次使用时随机生成并持久化：这样重启后旧链接仍能显示，而数据库泄露的
	// 单独一份拷贝不足以还原可用链接（还缺 settings 里的密钥）。
	accountLinkKeySetting = "account_link_key"
)

// linkQRState 记录「一次性链接二维码会话」与链接的绑定关系。
//
// 二维码会话本身复用主扫码那套（a.qrSessions / a.qrClients / 磁盘图片），
// 这里只补链接特有的字段。把两者分开的好处是：轮询、取图、回收都可以复用
// 已经验证过的代码，不需要为链接再实现一遍会话生命周期。
type linkQRState struct {
	LinkID          int64
	LinkHash        string
	BaseAccountID   int64
	Mode            string
	SwitchConfirmed bool
}

// newAccountLinkToken 生成一次性 token。
//
// 32 字节随机数去掉填充后正好 32 个 URL 安全字符，同时返回 SHA256 用于落库：
// 数据库里永远不存能直接使用的凭据。
func newAccountLinkToken() (string, string, error) {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "", "", fmt.Errorf("generate account link token: %w", err)
	}
	raw := base64.RawURLEncoding.EncodeToString(buffer)
	hash := sha256.Sum256([]byte(raw))
	return raw, hex.EncodeToString(hash[:]), nil
}

func accountLinkTokenHash(raw string) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(hash[:])
}

// validAccountLinkToken 在查库前先做格式校验。
//
// 好处有两个：明显畸形的 token 不必打一次数据库；同时把 URL 里可能出现的
// 路径片段（"..", "/"）挡在外面，避免被拼进任何文件路径。
func validAccountLinkToken(raw string) bool {
	if len(raw) != 32 {
		return false
	}
	for _, char := range raw {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

// accountLinkForToken 校验 token 并返回仍然可用的链接。
func (a *App) accountLinkForToken(ctx context.Context, raw string) (*store.AccountLink, error) {
	if !validAccountLinkToken(raw) {
		return nil, sql.ErrNoRows
	}
	link, err := a.db.GetAccountLinkByHash(ctx, accountLinkTokenHash(raw))
	if err != nil {
		return nil, err
	}
	if link.UsedAt != nil || link.RevokedAt != nil || link.ExpiresAt <= time.Now().Unix() {
		return nil, errors.New("账号授权链接已失效或已使用")
	}
	if link.Kind != accountLinkKindUpdate && link.Kind != accountLinkKindAdd {
		return nil, errors.New("账号授权链接类型无效")
	}
	return link, nil
}

// runAccountLinkCleanup 周期性清理过期链接。
//
// 清理失败只记日志：这是尽力而为的维护任务，不能因为一次数据库忙就影响接口可用性，
// 更不能让还没到期的链接跟着失效。
func (a *App) runAccountLinkCleanup() {
	ctx := context.Background()
	cleanup := func() {
		removed, err := a.db.PurgeExpiredAccountLinks(ctx)
		if err != nil {
			log.Printf("[account-links] 清理过期链接失败: %v", err)
			return
		}
		if removed > 0 {
			log.Printf("[account-links] 已清理 %d 条已消费/作废/过期的链接", removed)
		}
	}
	cleanup()
	ticker := time.NewTicker(accountLinkCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopLinks:
			return
		case <-ticker.C:
			cleanup()
		}
	}
}

// ---- 链接 token 的加密副本（只用于管理页「复制链接」） ----

func (a *App) accountLinkKey(ctx context.Context) []byte {
	seed, err := a.db.GetSetting(ctx, accountLinkKeySetting)
	if err != nil || seed == "" {
		buffer := make([]byte, 32)
		if _, err := rand.Read(buffer); err == nil {
			seed = base64.RawURLEncoding.EncodeToString(buffer)
			// 写入失败不致命：本次进程内仍可用，只是重启后旧链接无法再展示明文。
			_ = a.db.SetSetting(ctx, accountLinkKeySetting, seed)
		}
	}
	h := sha256.Sum256([]byte("yyb-account-link-key:" + seed))
	return h[:]
}

func (a *App) encryptAccountLinkToken(ctx context.Context, token string) string {
	block, err := aes.NewCipher(a.accountLinkKey(ctx))
	if err != nil {
		return ""
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ""
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return ""
	}
	sealed := gcm.Seal(nonce, nonce, []byte(token), nil)
	return base64.RawURLEncoding.EncodeToString(sealed)
}

func (a *App) decryptAccountLinkToken(ctx context.Context, value string) (string, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(a.accountLinkKey(ctx))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", errors.New("invalid token ciphertext")
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// firstAccountLabel 依次取第一个非空标签，用于给账号起一个可读的名字。
func firstAccountLabel(values ...*string) string {
	for _, value := range values {
		if value != nil && strings.TrimSpace(*value) != "" {
			return strings.TrimSpace(*value)
		}
	}
	return ""
}

// ---- 管理接口：创建 / 列出 / 作废链接 ----

func (a *App) handleAccountLinksAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/account-links")
	if path == "" || path == "/" {
		if r.Method == http.MethodGet {
			a.listAccountLinks(w, r)
			return
		}
	} else {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) == 2 && parts[1] == "revoke" && r.Method == http.MethodPost {
			a.revokeAccountLink(w, r, parts[0])
			return
		}
		if len(parts) == 1 && r.Method == http.MethodDelete {
			a.deleteAccountLink(w, r, parts[0])
			return
		}
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Kind          string `json:"kind"`
		Ref           string `json:"ref"`
		TTLSeconds    int64  `json:"ttl_seconds"`
		Product       string `json:"product"`
		ConfirmSwitch bool   `json:"confirm_switch"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	body.Kind = strings.ToLower(strings.TrimSpace(body.Kind))
	if body.Kind != accountLinkKindUpdate && body.Kind != accountLinkKindAdd {
		writeError(w, http.StatusBadRequest, "kind must be update or add")
		return
	}
	if body.TTLSeconds == 0 {
		defaultTTL := accountLinkUpdateDefaultTTL
		if body.Kind == accountLinkKindAdd {
			defaultTTL = accountLinkAddDefaultTTL
		}
		body.TTLSeconds = int64(defaultTTL / time.Second)
	}
	if body.TTLSeconds < 60 || body.TTLSeconds > int64(accountLinkMaxTTL/time.Second) {
		writeError(w, http.StatusBadRequest, "ttl_seconds must be between 60 and 604800")
		return
	}
	account, ok := a.resolveAccountRef(w, r, body.Ref)
	if !ok {
		return
	}
	// 不指定渠道时，更新类链接默认沿用账号原渠道：跨渠道等于换身份，
	// 必须由调用方显式要求并确认，不能作为默认行为。
	product := body.Product
	if product == "" && body.Kind == accountLinkKindUpdate {
		product = account.Public().ScanProduct
	}
	if product == "" {
		product = loginProductAppStore
	}
	product, err := normalizeLoginProduct(product)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	sourceProduct := account.Public().ScanProduct
	if body.Kind == accountLinkKindUpdate && product != sourceProduct && !body.ConfirmSwitch {
		writeError(w, http.StatusUnprocessableEntity, "请确认切换渠道：将保留账号 ID，并替换 OpenID、凭据和会话")
		return
	}
	expectedOpenID := ""
	if body.Kind == accountLinkKindUpdate {
		expectedOpenID = account.OpenID
	}

	// 串行化「检查已有链接 + 插入」。没有这把锁，两次点击（或两个客户端并发）
	// 都会看到空集合，于是同一账号上出现两条一次性链接。
	a.accountLinkMu.Lock()
	defer a.accountLinkMu.Unlock()
	if _, err := a.db.PurgeExpiredAccountLinks(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "清理旧授权链接失败："+err.Error())
		return
	}
	active, err := a.db.ListActiveAccountLinksForTarget(r.Context(), body.Kind, account.ID, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取已有授权链接失败："+err.Error())
		return
	}
	existing := make([]map[string]any, 0, len(active))
	for _, item := range active {
		existing = append(existing, a.accountLinkManagementJSON(r.Context(), r, item))
	}
	if len(existing) > 0 {
		writeRawJSON(w, http.StatusConflict, apiEnvelope{
			Code: http.StatusConflict,
			Msg:  "该账号已有未消费授权链接，请先使用或作废旧链接",
			Data: map[string]any{
				"code":           "active_link_exists",
				"message":        "该账号已有未消费授权链接，请先使用或作废旧链接",
				"existing":       existing,
				"existing_count": len(existing),
			},
		})
		return
	}

	rawToken, tokenHash, err := newAccountLinkToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	link, err := a.db.CreateAccountLinkForProduct(r.Context(), tokenHash, a.encryptAccountLinkToken(r.Context(), rawToken), body.Kind, account.ID, nil, expectedOpenID, time.Now().Add(time.Duration(body.TTLSeconds)*time.Second).Unix(), product, body.ConfirmSwitch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("[account-links] 已创建%s链接 id=%d 账号=%d 渠道=%s 有效期至=%s",
		body.Kind, link.ID, account.ID, product, time.Unix(link.ExpiresAt, 0).Format("15:04:05"))
	writeJSON(w, http.StatusOK, map[string]any{
		"id":             link.ID,
		"kind":           link.Kind,
		"product":        product,
		"source_product": sourceProduct,
		"switched":       body.Kind == accountLinkKindUpdate && product != sourceProduct,
		"url":            a.accountLinkURL(r, rawToken),
		"expires_at":     link.ExpiresAt,
		"one_time":       true,
		"existing":       existing,
		"existing_count": len(existing),
	})
}

func (a *App) accountLinkManagementJSON(ctx context.Context, r *http.Request, item store.AccountLinkRecord) map[string]any {
	entry := map[string]any{
		"id": item.ID, "kind": item.Kind, "account_id": item.AccountID,
		"expires_at": item.ExpiresAt, "created_at": item.CreatedAt, "status": item.Status,
		"used_at": item.UsedAt, "revoked_at": item.RevokedAt, "url": "", "product": item.TargetProduct,
	}
	if item.TokenCiphertext != "" {
		if token, err := a.decryptAccountLinkToken(ctx, item.TokenCiphertext); err == nil && validAccountLinkToken(token) {
			entry["url"] = a.accountLinkURL(r, token)
		}
	}
	return entry
}

func (a *App) listAccountLinks(w http.ResponseWriter, r *http.Request) {
	items, err := a.db.ListAccountLinks(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	accounts, _ := a.db.ListAccounts(r.Context())
	accountMap := map[int64]map[string]any{}
	for _, acc := range accounts {
		accountMap[acc.ID] = map[string]any{"id": acc.ID, "openid": acc.OpenID, "nickname": acc.Nickname, "remark": acc.Remark, "alias": acc.Alias}
	}
	out := make([]map[string]any, 0, len(items))
	counts := map[string]int{"active": 0, "used": 0, "expired": 0, "revoked": 0}
	for _, item := range items {
		entry := a.accountLinkManagementJSON(r.Context(), r, item)
		entry["account"] = accountMap[item.AccountID]
		out = append(out, entry)
		counts[item.Status]++
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out, "counts": counts})
}

func (a *App) revokeAccountLink(w http.ResponseWriter, r *http.Request, rawID string) {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid link id")
		return
	}
	if err = a.db.RevokeAccountLink(r.Context(), id, nil); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "链接不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

func (a *App) deleteAccountLink(w http.ResponseWriter, r *http.Request, rawID string) {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid link id")
		return
	}
	if err = a.db.DeleteAccountLink(r.Context(), id, nil); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "链接不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// ---- 链接 URL 构造 ----

func (a *App) accountLinkURL(r *http.Request, token string) string {
	scheme := "http"
	if strings.EqualFold(forwardedValue(r.Header.Get("X-Forwarded-Proto")), "https") ||
		strings.EqualFold(forwardedParameter(r.Header.Get("Forwarded"), "proto"), "https") ||
		a.cfg.CookieSecure {
		scheme = "https"
	}
	host := forwardedValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = forwardedParameter(r.Header.Get("Forwarded"), "host")
	}
	if !validPublicHost(host) {
		host = r.Host
	}
	if !validPublicHost(host) {
		host = "localhost"
	}
	return scheme + "://" + host + "/account-link/" + token
}

// forwardedValue 取逗号分隔代理头的第一个值。
// 反向代理通常把自己的跳追加在末尾，第一个值才是浏览器侧的公网地址。
func forwardedValue(value string) string {
	value = strings.TrimSpace(strings.Split(value, ",")[0])
	return strings.Trim(value, "\"")
}

func forwardedParameter(value, name string) string {
	for _, part := range strings.Split(forwardedValue(value), ";") {
		key, raw, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		return strings.Trim(strings.TrimSpace(raw), "\"")
	}
	return ""
}

// validPublicHost 校验 Host 是否像正常主机名，挡住 Host 头注入。
// 谁能控制 Host，谁就能让生成的链接指向自己的域名——而链接里带着一次性凭证。
func validPublicHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, "/\\?#@\r\n") {
		return false
	}
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 || net.ParseIP(host[1:end]) == nil {
			return false
		}
		if len(host) > end+1 && host[end+1] != ':' {
			return false
		}
		return true
	}
	if strings.Count(host, ":") > 1 {
		return false
	}
	if strings.Contains(host, ":") {
		_, port, err := net.SplitHostPort(host)
		if err != nil || port == "" {
			return false
		}
	}
	return true
}

// ---- 公开的链接页面与二维码流程 ----
//
// 注意：/account-link/* 是故意不要求管理员登录的。
// 这个功能的全部意义就是「把链接发给另一个人，让他用手机扫」——
// 要求登录就等于要求对方也拿到管理员口令。
// 安全性由三层保证：一次性消费、短 TTL、以及更新类链接必须由原微信号扫码

// handleAccountLinkRoute 分发 /account-link/* 下的两类请求：
//   - /account-link/{token}          → 链接页面
//   - /account-link/{token}/...      → 二维码流程接口
//
// 两条路径合并注册是因为 gin 不允许在同一层级同时挂 :token 与 *path，
// 而这里按「有没有后续段落」区分本来就足够准确。
func (a *App) handleAccountLinkRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/account-link/")
	if rest == r.URL.Path || rest == "" {
		writeError(w, http.StatusNotFound, "account link not found")
		return
	}
	if strings.Contains(rest, "/") {
		a.handleAccountLink(w, r)
		return
	}
	a.handleAccountLinkPage(w, r)
}

// handleAccountLinkPage 返回链接专用页面。
func (a *App) handleAccountLinkPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/account-link/")
	if strings.Contains(token, "/") {
		writeError(w, http.StatusNotFound, "account link not found")
		return
	}
	if _, err := a.accountLinkForToken(r.Context(), token); err != nil {
		writeError(w, http.StatusGone, "账号授权链接已失效或不存在")
		return
	}
	path := filepath.Join(a.resources.Templates, "account-link.html")
	data, err := os.ReadFile(path)
	if err != nil {
		// 模板缺失（例如部署时挂载了旧的 resource 目录）不能让扫码的人看到 500：
		// 退回内嵌的精简版，至少保证这条路能走通。
		log.Printf("[account-links] 读取链接页面失败，改用内嵌后备页面: %v", err)
		data = []byte(fallbackAccountLinkHTML)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 页面里带着一次性凭证，禁止任何中间层缓存
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (a *App) handleAccountLink(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/account-link/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "account link not found")
		return
	}
	token := parts[0]
	link, err := a.accountLinkForToken(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusGone, "账号授权链接已失效或不存在")
		return
	}
	if parts[1] == "qr" && len(parts) == 2 {
		a.handleAccountLinkQRCreate(w, r, link)
		return
	}
	if parts[1] == "meta" && len(parts) == 2 && r.Method == http.MethodGet {
		base, err := a.db.GetAccount(r.Context(), link.AccountID)
		if err != nil {
			writeError(w, http.StatusGone, "基础账号已不存在")
			return
		}
		product, err := a.linkScanProduct(r.Context(), link, base, "")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "读取扫码渠道失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"product":          product,
			"source_product":   base.Public().ScanProduct,
			"switch_confirmed": link.SwitchConfirmed,
			"kind":             link.Kind,
			"target":           firstAccountLabel(base.Remark, base.Nickname, base.Alias),
			"account_id":       base.ID,
			"expires_at":       link.ExpiresAt,
		})
		return
	}
	if len(parts) != 4 || parts[1] != "qr" {
		writeError(w, http.StatusNotFound, "account link route not found")
		return
	}
	sessionID, action := parts[2], parts[3]
	// 会话必须属于这条链接：否则拿到任意一条有效链接的人，
	// 就能拿别人（或更早）的二维码会话去确认，绕过 TTL 与一次性约束。
	state := a.getLinkQRState(sessionID)
	if a.getQRSession(sessionID) == nil || state == nil || state.LinkID != link.ID || state.LinkHash != link.TokenHash {
		writeError(w, http.StatusNotFound, "二维码会话不存在或不属于此链接")
		return
	}
	switch action {
	case "cancel":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.dropQRSession(sessionID)
		writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
	case "image":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		path := a.resources.qrPath(sessionID)
		if filepath.Base(path) != sessionID+".jpg" || strings.Contains(sessionID, "..") {
			writeError(w, http.StatusNotFound, "qr session not found")
			return
		}
		if _, err := os.Stat(path); err != nil {
			writeError(w, http.StatusNotFound, "qr session not found")
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, path)
	case "poll":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		sess := a.getQRSession(sessionID)
		result, err := a.getQRClient(sessionID).PollQRCode(r.Context(), sess)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		if qrStatusTerminal(result.Status) {
			a.dropQRSession(sessionID)
		}
		writeJSON(w, http.StatusOK, result)
	case "confirm":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.confirmLinkQR(w, r, token, link, sessionID, state)
	default:
		writeError(w, http.StatusNotFound, "account link route not found")
	}
}

// handleAccountLinkQRCreate 为链接生成二维码。
//
// 客户端固定沿用基础账号的代理出口：更新链接的目的是「换凭据」，不是「换出口」。
// 如果这次用另一个 IP 扫码，账号的登录地就会莫名其妙地漂移。
func (a *App) handleAccountLinkQRCreate(w http.ResponseWriter, r *http.Request, link *store.AccountLink) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	base, err := a.db.GetAccount(r.Context(), link.AccountID)
	if err != nil {
		writeError(w, http.StatusGone, "基础账号已不存在")
		return
	}
	var body struct {
		Product       string `json:"product"`
		ConfirmSwitch bool   `json:"confirm_switch"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	product, err := a.linkScanProduct(r.Context(), link, base, body.Product)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	sourceProduct := base.Public().ScanProduct
	switching := link.Kind == accountLinkKindUpdate && product != sourceProduct
	confirmed := body.ConfirmSwitch || (link.SwitchConfirmed && link.TargetProduct == product)
	if switching && !confirmed {
		writeError(w, http.StatusUnprocessableEntity, "请先确认切换渠道后再生成二维码")
		return
	}
	a.pruneQR()
	client := a.qrClientForScan(r.Context(), base.OpenID)
	img, err := client.GetQRCodeImageForProduct(r.Context(), product)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if !isSafeFileToken(img.Session.ID) {
		writeError(w, http.StatusBadGateway, "二维码会话标识异常，已拒绝写入")
		return
	}
	a.mu.Lock()
	a.qrSessions[img.Session.ID] = img.Session
	a.qrClients[img.Session.ID] = client
	delete(a.qrProxies, img.Session.ID)
	if a.linkSessions == nil {
		a.linkSessions = map[string]*linkQRState{}
	}
	a.linkSessions[img.Session.ID] = &linkQRState{
		LinkID:          link.ID,
		LinkHash:        link.TokenHash,
		BaseAccountID:   link.AccountID,
		Mode:            link.Kind,
		SwitchConfirmed: switching && confirmed,
	}
	keep := make(map[string]bool, len(a.qrSessions))
	for sid := range a.qrSessions {
		keep[sid] = true
	}
	a.mu.Unlock()
	if err := os.WriteFile(a.resources.qrPath(img.Session.ID), img.ImageBytes, 0o644); err != nil {
		log.Printf("[account-links] 写入二维码图片失败: %v", err)
	}
	a.cleanupQR(keep)
	writeJSON(w, http.StatusOK, map[string]any{
		"product":         product,
		"source_product":  sourceProduct,
		"switched":        switching,
		"session_id":      img.Session.ID,
		"status":          img.Session.Status,
		"image_url":       "/account-link/" + tokenForLinkPath(r.URL.Path) + "/qr/" + img.Session.ID + "/image",
		"expires_in":      int64(a.cfg.QRSessionTTL.Seconds()),
		"qr_expires_at":   time.Now().Add(a.cfg.QRSessionTTL).Unix(),
		"link_expires_at": link.ExpiresAt,
		"mode":            link.Kind,
	})
}

// linkScanProduct 决定这条链接应该用哪个渠道扫码。
//
// 顺序是「页面显式选择 → 链接签发时锁定的渠道 → 原账号渠道 / 系统默认」。
// 中间那一步很关键：链接一旦签发，渠道就固定了，避免页面被改参数后
// 悄悄把账号换到另一个产品线上。
func (a *App) linkScanProduct(ctx context.Context, link *store.AccountLink, base *store.WechatAccount, explicit string) (string, error) {
	if explicit != "" {
		return normalizeLoginProduct(explicit)
	}
	if link.TargetProduct != "" {
		return normalizeLoginProduct(link.TargetProduct)
	}
	if link.Kind == accountLinkKindUpdate {
		return base.Public().ScanProduct, nil
	}
	return a.defaultScanProduct(ctx), nil
}

func tokenForLinkPath(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/account-link/"), "/")
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

// getLinkQRState 线程安全地读取链接二维码会话的附加状态。
func (a *App) getLinkQRState(id string) *linkQRState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.linkSessions[id]
}

// confirmLinkQR 完成链接扫码后的落库动作。
//
// 两条分支的差别是整个功能的核心：
//   - update：在原账号 ID 内原子替换凭据/OpenID/资料/会话，不新增账号；
//   - add：走普通扫码入库，把新账号建出来。
func (a *App) confirmLinkQR(w http.ResponseWriter, r *http.Request, token string, link *store.AccountLink, sessionID string, state *linkQRState) {
	client := a.getQRClient(sessionID)
	sess := a.getQRSession(sessionID)
	if sess == nil {
		writeError(w, http.StatusNotFound, "二维码会话不存在")
		return
	}
	result, err := getLoginBufferWithRetry(r.Context(), client, sess)
	if err != nil {
		writeError(w, http.StatusConflict, "buffer not ready: "+err.Error())
		return
	}
	// 重新校验一次链接：从扫码到确认之间它可能刚好过期，或被另一台设备用掉。
	if _, err := a.accountLinkForToken(r.Context(), token); err != nil {
		writeError(w, http.StatusGone, "账号授权链接已失效或已使用")
		return
	}
	// 更新链接只接受「原账号」扫码。这条校验不能省：否则任何微信号扫一下
	// 都能把这条链接的授权换掉，等于把账号交到别人手里。
	if link.Kind == accountLinkKindUpdate && !state.SwitchConfirmed && result.Credentials.OpenID != link.ExpectedOpenID {
		writeError(w, http.StatusConflict, "请使用目标账号扫码：该链接仅限指定账号更新，当前账号未被修改")
		return
	}
	product, err := normalizeLoginProduct(result.Credentials.Provider)
	if err != nil || product != sess.Product {
		writeError(w, http.StatusConflict, "授权渠道与二维码不一致，请重新生成二维码")
		return
	}

	var userInfo map[string]any
	if ui, err := client.LoginBuffers().FetchUserInfo(r.Context(), result.Credentials); err == nil {
		userInfo = ui
	}

	if link.Kind == accountLinkKindUpdate {
		before, err := a.db.GetAccount(r.Context(), link.AccountID)
		if err != nil {
			writeError(w, http.StatusGone, "目标账号已不存在")
			return
		}
		if before.OpenID != link.ExpectedOpenID {
			writeError(w, http.StatusGone, "账号授权已更新，请重新生成链接")
			return
		}
		// 与保活共享同一把账号锁：它的条件更新会拒绝任何更旧的快照，
		// 从而避免「链接替换」和「后台刷新」把彼此的凭据盖掉。
		lock := a.refreshLockFor(before.ID)
		select {
		case lock <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		nick := pickNickname(userInfo, result.Credentials.Nickname)
		avatar := a.resolveAvatar(r.Context(), result.Credentials.OpenID, userInfo)
		acc, err := a.db.UpdateAccountFromLink(r.Context(), link, result.Credentials.OpenID, result.LoginBuffer,
			stringPtrMaybe(nick), stringPtrMaybe(avatar), userInfo, result.Credentials.ToMapForScan())
		<-lock
		if errors.Is(err, store.ErrAccountIdentityConflict) {
			writeError(w, http.StatusConflict, "该渠道身份已属于其他账号，请先处理重复账号；原账号未修改")
			return
		}
		if errors.Is(err, store.ErrAccountLinkChanged) {
			writeError(w, http.StatusGone, "授权链接已消费或目标账号已更新")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "保存账号更新失败")
			return
		}
		// 账号身份换了，所有跟旧身份绑定的运行时状态都要作废：
		// 代理租约是按旧出口谈下来的，退避计数是针对旧凭据积累的。
		a.invalidateProxyLease(acc.ID)
		a.clearKeepAliveRetry(acc.ID)
		a.clearCredentialRejections(acc.ID)
		a.dropQRSession(sessionID)
		log.Printf("[account-links] 账号 %d 已替换授权：%s → %s（渠道 %s）",
			acc.ID, link.ExpectedOpenID, acc.OpenID, product)
		writeJSON(w, http.StatusOK, map[string]any{"replaced": true, "account": acc.Public()})
		return
	}

	// kind=add：与普通扫码一致
	consumed, err := a.db.ConsumeAccountLink(r.Context(), link.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !consumed {
		writeError(w, http.StatusGone, "账号授权链接已失效或已使用")
		return
	}
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
	if link.Kind == accountLinkKindAdd && !existed {
		if err := a.db.CopyAccountProxySetting(r.Context(), link.AccountID, acc.ID); err != nil {
			_ = a.db.DeleteAccount(r.Context(), acc.ID)
			writeError(w, http.StatusInternalServerError, "复制基础账号代理失败："+err.Error())
			return
		}
	}
	// 一次性：用完即删，同时释放链接编号。
	if err := a.db.DeleteAccountLink(r.Context(), link.ID, link.OwnerUserID); err != nil {
		log.Printf("[account-links] 删除已消费链接 id=%d 失败: %v", link.ID, err)
	}
	a.dropQRSession(sessionID)
	writeJSON(w, http.StatusOK, map[string]any{"replaced": false, "account": acc.Public()})
}
