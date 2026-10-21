package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"yybwechat/internal/protocol"
	"yybwechat/internal/proxysource"
	"yybwechat/internal/qr"
	"yybwechat/internal/store"
)

// 本文件实现账号级代理配置：读写、连通性测试、出口 IP 探测，
// 以及把「模式 + 供应商 + 地区」解析成实际代理地址的运行时逻辑。
//
// 动态代理的出口会变化，因此维护一份短 TTL 的租约缓存：
// 同一账号在租约有效期内复用同一条代理，避免每次请求都去提取新 IP。

type accountProxyIn struct {
	Product             string  `json:"product"`
	Ref                 string  `json:"ref"`
	Mode                string  `json:"mode"`
	ProxyType           string  `json:"proxy_type"`
	StaticProxy         string  `json:"static_proxy"`
	APIURL              string  `json:"api_url"`
	ProviderProfileID   *int64  `json:"provider_profile_id"`
	RegionCode          string  `json:"region_code"`
	RegionProvince      string  `json:"region_province"`
	RegionCity          string  `json:"region_city"`
	RefreshAheadMinutes int64   `json:"refresh_ahead_minutes"`
	ProtocolProxyMode   *string `json:"protocol_proxy_mode"`
}

const loginProductAppStore = "appstore"

// normalizeLoginProduct 校验登录产品类型。目前只验证过应用宝凭据兑换链路。
func normalizeLoginProduct(product string) (string, error) {
	product = strings.ToLower(strings.TrimSpace(product))
	if product == "" {
		return loginProductAppStore, nil
	}
	if product != loginProductAppStore {
		return "", fmt.Errorf("登录产品 %q 尚未完成凭据兑换验证，目前仅支持 appstore", product)
	}
	return product, nil
}

// accountProxyLease 是动态代理出口的短期缓存。
type accountProxyLease struct {
	Value            string
	SettingUpdatedAt int64
	ExpiresAt        time.Time
}

const accountProxyLeaseTTL = 45 * time.Second

func (body accountProxyIn) spec() proxysource.Spec {
	return proxysource.Spec{Mode: body.Mode, ProxyType: body.ProxyType, StaticProxy: body.StaticProxy, APIURL: body.APIURL}
}

func proxySpecFromSetting(setting *store.AccountProxySetting) proxysource.Spec {
	if setting == nil {
		return proxysource.Spec{Mode: "direct", ProxyType: "http"}
	}
	return proxysource.Spec{
		Mode: setting.Mode, ProxyType: setting.ProxyType,
		StaticProxy: setting.StaticProxy, APIURL: setting.APIURL,
	}
}

// proxySettingPublic 生成账号代理配置的对外视图，附带生命周期提示。
// 动态代理是短效出口，无法用于保活，这里直接把限制讲清楚，避免误配。
func proxySettingPublic(setting *store.AccountProxySetting, account *store.WechatAccount) map[string]any {
	expiresIn := protocol.CredentialsFromMap(account.Credentials).ExpiresIn
	tokenTTLMinutes := (expiresIn + 59) / 60
	dynamic := setting.Mode == "api"
	lifetimeClass := "stable"
	keepaliveSupported := true
	keepaliveNote := "可参与自动保活"
	if dynamic {
		lifetimeClass = "short"
		keepaliveSupported = false
		keepaliveNote = "短效代理，无法用于账号保活；仅用于扫码和临时请求"
	}
	return map[string]any{
		"account_id": setting.AccountID, "mode": setting.Mode, "proxy_type": setting.ProxyType,
		"static_proxy": setting.StaticProxy, "api_url": setting.APIURL,
		"provider_profile_id": setting.ProviderProfileID,
		"region_code":         setting.RegionCode, "region_province": setting.RegionProvince, "region_city": setting.RegionCity,
		"refresh_ahead_minutes": setting.RefreshAheadSeconds / 60,
		"protocol_proxy_mode":   setting.ProtocolProxyMode,
		"token_ttl_minutes":     tokenTTLMinutes,
		"lifetime_class":        lifetimeClass, "keepalive_supported": keepaliveSupported, "keepalive_note": keepaliveNote,
		"proxy_warning": proxyWarning(dynamic),
		"configured":    setting.Mode != "direct", "updated_at": setting.UpdatedAt,
	}
}

func proxyWarning(dynamic bool) string {
	if dynamic {
		return "短效代理每次提取都会产生新的出口，无法稳定维持微信登录会话；后台自动保活已关闭。"
	}
	return ""
}

// handleAccountProxy 读取或更新指定账号的代理配置。
func (a *App) handleAccountProxy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		acc, ok := a.resolveAccountFromQuery(w, r)
		if !ok {
			return
		}
		setting, err := a.db.AccountProxySettingOrDefault(r.Context(), acc.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, proxySettingPublic(setting, acc))
	case http.MethodPut:
		var body accountProxyIn
		if err := decodeOptionalJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		acc, ok := a.resolveAccountRef(w, r, body.Ref)
		if !ok {
			return
		}
		normalizedBody, normalized, err := a.normalizeAccountProxyInput(r.Context(), body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		setting, err := a.saveAccountProxyInput(r.Context(), acc.ID, normalizedBody, normalized)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// 代理变了，旧会话必须作废，否则会继续用旧出口的登录态
		if err := a.db.InvalidateAccountSessions(r.Context(), acc.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		a.invalidateProxyLease(acc.ID)
		writeJSON(w, http.StatusOK, proxySettingPublic(setting, acc))
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleAccountProxyTest 解析一次代理并探测出口 IP，用于配置阶段的连通性验证。
func (a *App) handleAccountProxyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body accountProxyIn
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	_, spec, err := a.normalizeAccountProxyInput(r.Context(), body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 只给了 ref、没给模式：表示「测一下这个账号已保存的配置」
	if strings.TrimSpace(body.Ref) != "" && strings.TrimSpace(body.Mode) == "" {
		acc, ok := a.resolveAccountRef(w, r, body.Ref)
		if !ok {
			return
		}
		setting, err := a.db.AccountProxySettingOrDefault(r.Context(), acc.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		spec, err = a.proxySpecForSetting(r.Context(), setting)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	resolved, err := a.resolveProxySpec(r.Context(), spec)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	result := map[string]any{"resolved": resolved != "", "proxy": proxysource.Mask(resolved)}
	if resolved != "" {
		probe, probeErr := a.probeProxyExit(r.Context(), resolved)
		if probeErr != nil {
			result["probe_error"] = "出口检测请求失败，请重试"
		} else {
			result["exit_ip"] = probe.IP
			result["exit_country"] = probe.Country
			result["exit_region"] = probe.Region
			result["exit_city"] = probe.City
		}
	}
	writeJSON(w, http.StatusOK, result)
}

type proxyExitProbe struct {
	IP      string `json:"ip"`
	Country string `json:"country"`
	Region  string `json:"region"`
	City    string `json:"city"`
}

// probeProxyExit 通过指定代理访问 IP 归属地接口，确认出口是否生效。
func (a *App) probeProxyExit(ctx context.Context, proxyValue string) (proxyExitProbe, error) {
	transport, err := protocol.NewHTTPTransport(proxyValue, false)
	if err != nil {
		return proxyExitProbe{}, err
	}
	client := &http.Client{Timeout: a.cfg.RequestTimeout, Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ip.sb/geoip", nil)
	if err != nil {
		return proxyExitProbe{}, err
	}
	req.Header.Set("User-Agent", "yybwechat proxy check")
	resp, err := client.Do(req)
	if err != nil {
		return proxyExitProbe{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return proxyExitProbe{}, fmt.Errorf("exit probe returned HTTP %d", resp.StatusCode)
	}
	var result proxyExitProbe
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
		return proxyExitProbe{}, err
	}
	if strings.TrimSpace(result.IP) == "" {
		return proxyExitProbe{}, fmt.Errorf("exit probe returned no IP")
	}
	return result, nil
}

// resolveProxySpec 执行代理策略，得到最终可用的代理地址。
// 请求供应商接口时走直连，避免为了拿代理而依赖代理。
func (a *App) resolveProxySpec(ctx context.Context, spec proxysource.Spec) (string, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Timeout: a.cfg.RequestTimeout, Transport: transport}
	return proxysource.Resolve(ctx, client, spec)
}

// resolveWXAppProxy 解析 /wxapp/* 协议调用应使用的代理。
// 配置里可以把小程序调用固定为直连，此时不消耗动态代理的提取次数。
func (a *App) resolveWXAppProxy(ctx context.Context, accountID int64) (string, bool, error) {
	setting, err := a.db.GetAccountProxySetting(ctx, accountID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	if err == nil && setting.ProtocolProxyMode == "direct" {
		return "", false, nil
	}
	return a.resolveAccountProxy(ctx, accountID)
}

// resolveAccountProxy 解析账号应使用的代理地址。
//
// 返回值的第二个字段表示「是否强制使用代理且不允许降级直连」。
// 未配置账号级代理时退回命令行传入的全局 --tcp-proxy，保持既有部署方式可用。
func (a *App) resolveAccountProxy(ctx context.Context, accountID int64) (string, bool, error) {
	setting, err := a.db.GetAccountProxySetting(ctx, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		if strings.TrimSpace(a.cfg.TCPProxy) == "" {
			return "", false, nil
		}
		normalized, normalizeErr := proxysource.NormalizeSpec(proxysource.Spec{Mode: "static", StaticProxy: a.cfg.TCPProxy})
		if normalizeErr != nil {
			return "", false, normalizeErr
		}
		return normalized.StaticProxy, true, nil
	}
	if err != nil {
		return "", false, err
	}
	spec, err := a.proxySpecForSetting(ctx, setting)
	if err != nil {
		return "", false, err
	}
	if spec.Mode != "api" {
		proxyValue, err := a.resolveProxySpec(ctx, spec)
		return proxyValue, false, err
	}
	// 动态代理：按账号串行化提取，并复用未过期的租约
	leaseLock := a.proxyLeaseLockFor(accountID)
	select {
	case leaseLock <- struct{}{}:
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
	defer func() { <-leaseLock }()
	a.proxyMu.Lock()
	if lease, ok := a.proxyLeases[accountID]; ok && lease.SettingUpdatedAt == setting.UpdatedAt && time.Now().Before(lease.ExpiresAt) {
		a.proxyMu.Unlock()
		return lease.Value, false, nil
	}
	a.proxyMu.Unlock()
	proxyValue, err := a.resolveProxySpec(ctx, spec)
	a.proxyMu.Lock()
	defer a.proxyMu.Unlock()
	if err == nil && proxyValue != "" {
		a.proxyLeases[accountID] = accountProxyLease{
			Value:            proxyValue,
			SettingUpdatedAt: setting.UpdatedAt,
			ExpiresAt:        time.Now().Add(accountProxyLeaseTTL),
		}
	} else {
		delete(a.proxyLeases, accountID)
	}
	return proxyValue, false, err
}

func (a *App) proxyLeaseLockFor(accountID int64) chan struct{} {
	a.proxyLeaseLocksMu.Lock()
	defer a.proxyLeaseLocksMu.Unlock()
	if lock := a.proxyLeaseLocks[accountID]; lock != nil {
		return lock
	}
	lock := make(chan struct{}, 1)
	a.proxyLeaseLocks[accountID] = lock
	return lock
}

// invalidateProxyLease 让账号的动态代理租约立即失效。
func (a *App) invalidateProxyLease(accountID int64) {
	a.proxyMu.Lock()
	delete(a.proxyLeases, accountID)
	a.proxyMu.Unlock()
}

// qrClientForSpec 为扫码流程创建绑定指定代理的客户端。
// resolved 为空时复用全局客户端（直连）。
func (a *App) qrClientForSpec(ctx context.Context, spec proxysource.Spec) (*qr.Client, string, error) {
	resolved, err := a.resolveProxySpec(ctx, spec)
	if err != nil {
		return nil, "", err
	}
	if resolved == "" {
		return a.qr, "", nil
	}
	client, err := qr.NewClientWithProxy(a.cfg.RequestTimeout, resolved, false)
	if err != nil {
		return nil, "", fmt.Errorf("创建代理客户端失败: %w", err)
	}
	return client, resolved, nil
}

func (a *App) saveAccountProxyInput(ctx context.Context, accountID int64, body accountProxyIn, normalized proxysource.Spec) (*store.AccountProxySetting, error) {
	refreshAheadSeconds := body.RefreshAheadMinutes * 60
	apiURL := normalized.APIURL
	// 引用供应商配置时不把提取地址落库，读取时按地区重新拼，避免地区变更后地址过期
	if body.ProviderProfileID != nil {
		apiURL = ""
	}
	return a.db.UpsertAccountProxySetting(ctx, accountID, normalized.Mode, normalized.ProxyType,
		normalized.StaticProxy, apiURL, body.ProviderProfileID,
		strings.TrimSpace(body.RegionCode), strings.TrimSpace(body.RegionProvince), strings.TrimSpace(body.RegionCity),
		refreshAheadSeconds, body.ProtocolProxyMode)
}

func (a *App) accountExistsBeforeScan(ctx context.Context, openID string) (bool, error) {
	_, err := a.db.GetAccountByOpenID(ctx, openID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// saveNewAccountProxy 只对「本次新扫码产生」的账号写入代理配置。
// 已存在的账号重新扫码时不覆盖其原有代理设置。
func (a *App) saveNewAccountProxy(ctx context.Context, accountID int64, existed bool, body accountProxyIn, normalized proxysource.Spec) error {
	if existed {
		return nil
	}
	_, err := a.saveAccountProxyInput(ctx, accountID, body, normalized)
	return err
}
