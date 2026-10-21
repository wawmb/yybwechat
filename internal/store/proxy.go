package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DefaultProxyRefreshAheadSeconds 是动态代理（短效 IP）默认的提前续期秒数。
// 动态代理的租约通常只有几分钟，所以续期窗口比静态凭据的 45 分钟短得多。
const DefaultProxyRefreshAheadSeconds int64 = 300

// AccountProxySetting 表示单个账号的代理策略。
//
// Mode 取值：
//   - direct：直连，不经过任何代理；
//   - static：使用 StaticProxy 里写死的代理地址；
//   - api：从 APIURL 或供应商配置动态取一个出口 IP。
type AccountProxySetting struct {
	AccountID           int64  `json:"account_id"`
	Mode                string `json:"mode"`
	ProxyType           string `json:"proxy_type"`
	StaticProxy         string `json:"static_proxy"`
	APIURL              string `json:"api_url"`
	ProviderProfileID   *int64 `json:"provider_profile_id,omitempty"`
	RegionCode          string `json:"region_code"`
	RegionProvince      string `json:"region_province"`
	RegionCity          string `json:"region_city"`
	RefreshAheadSeconds int64  `json:"refresh_ahead_seconds"`
	ProtocolProxyMode   string `json:"protocol_proxy_mode"`
	CreatedAt           int64  `json:"created_at"`
	UpdatedAt           int64  `json:"updated_at"`
}

// GetAccountProxySetting 读取账号的代理配置，不存在时返回 sql.ErrNoRows。
func (db *DB) GetAccountProxySetting(ctx context.Context, accountID int64) (*AccountProxySetting, error) {
	setting := &AccountProxySetting{}
	err := db.sql.QueryRowContext(ctx, `
SELECT account_id, mode, proxy_type, static_proxy, api_url, provider_profile_id,
       region_code, region_province, region_city, refresh_ahead_seconds, protocol_proxy_mode, created_at, updated_at
FROM account_proxy_settings WHERE account_id=?`, accountID).Scan(
		&setting.AccountID, &setting.Mode, &setting.ProxyType, &setting.StaticProxy,
		&setting.APIURL, &setting.ProviderProfileID, &setting.RegionCode, &setting.RegionProvince,
		&setting.RegionCity, &setting.RefreshAheadSeconds, &setting.ProtocolProxyMode, &setting.CreatedAt, &setting.UpdatedAt,
	)
	return setting, err
}

// AccountProxySettingOrDefault 读取账号代理配置，未配置时返回「直连」默认值。
// 保活与协议层都依赖它，因此不能因为缺少记录而报错。
func (db *DB) AccountProxySettingOrDefault(ctx context.Context, accountID int64) (*AccountProxySetting, error) {
	setting, err := db.GetAccountProxySetting(ctx, accountID)
	if err == nil {
		return setting, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return &AccountProxySetting{
		AccountID:           accountID,
		Mode:                "direct",
		ProxyType:           "http",
		RefreshAheadSeconds: DefaultProxyRefreshAheadSeconds,
		ProtocolProxyMode:   "account",
	}, nil
}

// UpsertAccountProxySetting 写入或更新账号代理配置。
// protocolProxyMode 传 nil 表示保留原有策略，便于老客户端只更新其他字段。
func (db *DB) UpsertAccountProxySetting(ctx context.Context, accountID int64, mode, proxyType, staticProxy, apiURL string, profileID *int64, regionCode, regionProvince, regionCity string, refreshAheadSeconds int64, protocolProxyMode *string) (*AccountProxySetting, error) {
	if protocolProxyMode != nil && *protocolProxyMode != "account" && *protocolProxyMode != "direct" {
		return nil, fmt.Errorf("protocol_proxy_mode must be account or direct")
	}
	if refreshAheadSeconds <= 0 {
		refreshAheadSeconds = DefaultProxyRefreshAheadSeconds
	}
	now := time.Now().Unix()
	_, err := db.sql.ExecContext(ctx, `
INSERT INTO account_proxy_settings
(account_id, mode, proxy_type, static_proxy, api_url, provider_profile_id, region_code,
 region_province, region_city, refresh_ahead_seconds, protocol_proxy_mode, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,COALESCE(?, 'account'),?,?)
ON CONFLICT(account_id) DO UPDATE SET
mode=excluded.mode, proxy_type=excluded.proxy_type, static_proxy=excluded.static_proxy,
api_url=excluded.api_url, provider_profile_id=excluded.provider_profile_id,
region_code=excluded.region_code, region_province=excluded.region_province,
region_city=excluded.region_city, refresh_ahead_seconds=excluded.refresh_ahead_seconds,
protocol_proxy_mode=COALESCE(?, account_proxy_settings.protocol_proxy_mode),
updated_at=excluded.updated_at`,
		accountID, mode, proxyType, staticProxy, apiURL, profileID, regionCode,
		regionProvince, regionCity, refreshAheadSeconds, protocolProxyMode, now, now, protocolProxyMode,
	)
	if err != nil {
		return nil, err
	}
	return db.GetAccountProxySetting(ctx, accountID)
}

// CopyAccountProxySetting 把一个账号的代理配置复制到另一个账号（重新扫码时沿用）。
func (db *DB) CopyAccountProxySetting(ctx context.Context, sourceAccountID, targetAccountID int64) error {
	setting, err := db.AccountProxySettingOrDefault(ctx, sourceAccountID)
	if err != nil {
		return err
	}
	_, err = db.UpsertAccountProxySetting(ctx, targetAccountID, setting.Mode, setting.ProxyType,
		setting.StaticProxy, setting.APIURL, setting.ProviderProfileID, setting.RegionCode,
		setting.RegionProvince, setting.RegionCity, setting.RefreshAheadSeconds, &setting.ProtocolProxyMode)
	return err
}

// CountAccountsUsingProxyProfile 统计引用了某个供应商配置的账号数量，
// 删除供应商前用它拦截「仍被使用」的情况。
func (db *DB) CountAccountsUsingProxyProfile(ctx context.Context, profileID int64) (int, error) {
	var count int
	err := db.sql.QueryRowContext(ctx, "SELECT count(*) FROM account_proxy_settings WHERE provider_profile_id=?", profileID).Scan(&count)
	return count, err
}
