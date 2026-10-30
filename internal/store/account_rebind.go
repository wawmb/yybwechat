package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ErrAccountIdentityConflict 表示扫码得到的身份已经属于另一个账号。
var ErrAccountIdentityConflict = errors.New("scan identity already belongs to another account")

// ErrAccountLinkChanged 表示链接已消费/作废/过期，或目标账号的身份已被改过。
var ErrAccountLinkChanged = errors.New("update link expired or target identity changed")

// ErrAccountAuthorizationChanged 表示账号凭据在本次操作期间已被别人改写。
var ErrAccountAuthorizationChanged = errors.New("account authorization changed; retry with the current account")

// UpdateAccountFromLink 在原账号 ID 内原子替换授权。
//
// 这是「更新链接」最关键的一段：链接消费、身份唯一性校验、会话清理、旧链接作废
// 必须在同一个事务里，任何一步失败都整体回滚。否则会出现「凭据换了、旧会话还在」
// 或「链接被消费了但账号没换」这类既不能重试也无法解释的中间态。
//
// 只替换身份相关的列（openid / login_buffer / credentials / nickname / avatar / user_info）。
// 代理、备注、别名、设备标识等按账号 ID 挂的配置一律保留——运营者配置这些的成本
// 远高于重新扫码，替换身份时把它们冲掉会让这个功能不可用。
func (db *DB) UpdateAccountFromLink(ctx context.Context, link *AccountLink, openID, buffer string, nickname, avatar *string, userInfo, credentials map[string]any) (*WechatAccount, error) {
	if link == nil || link.Kind != "update" || openID == "" || buffer == "" {
		return nil, errors.New("invalid account replacement")
	}
	userJSON, err := marshalNullable(userInfo)
	if err != nil {
		return nil, err
	}
	credJSON, err := marshalNullable(credentials)
	if err != nil {
		return nil, err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()

	// 一条 SQL 同时校验：链接仍然可用、仍指向同一个账号、账号的 openid 还是
	// 链接签发时的那一个。三者任一变化都说明这次替换的前提已经不成立。
	var current string
	err = tx.QueryRowContext(ctx, `SELECT a.openid FROM wechat_accounts a JOIN account_links l ON l.account_id=a.id
WHERE l.id=? AND l.token_hash=? AND l.kind='update' AND a.id=? AND a.openid=? AND l.expected_openid=?
AND l.used_at IS NULL AND l.revoked_at IS NULL AND l.expires_at>?`, link.ID, link.TokenHash, link.AccountID, link.ExpectedOpenID, link.ExpectedOpenID, now).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountLinkChanged
	}
	if err != nil {
		return nil, err
	}

	// 新身份不能已经挂在别的账号上：否则两个账号会共享同一个 openid，
	// 保活会互相覆盖凭据，最终两个一起失效。
	var duplicate int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM wechat_accounts WHERE openid=? AND id<>?", openID, link.AccountID).Scan(&duplicate)
	if err == nil {
		return nil, ErrAccountIdentityConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// uin 必须清空：它是旧身份的会话产物，留着会被当成新身份的 UIN 使用。
	// uin 必须清空：它是旧身份的会话产物，留着会被当成新身份的 UIN 使用。
	// alias 只在原来为空时补上昵称，绝不用新昵称覆盖运营者设过的别名。
	_, err = tx.ExecContext(ctx, `UPDATE wechat_accounts SET openid=?, login_buffer=?, credentials=?, nickname=?,
avatar=?, user_info=?, uin=NULL, status='alive', last_checked_at=?, updated_at=?,
alias=COALESCE(NULLIF(TRIM(alias),''), ?) WHERE id=?`,
		openID, buffer, credJSON, nullableString(nickname), nullableString(avatar), userJSON, now, now,
		nullableString(nickname), link.AccountID)
	if err != nil {
		return nil, err
	}

	// 旧身份的 MMTLS 会话必须一起删掉，否则下一次取码会拿旧会话去换新身份的票据。
	if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE wechat_account_id=?", link.AccountID); err != nil {
		return nil, err
	}

	// 同一账号的其他待用链接一并作废：身份已经换了，它们携带的 expected_openid
	// 永远匹配不上，留着只会让人以为还有一条可用链接。
	if _, err = tx.ExecContext(ctx, "DELETE FROM account_links WHERE account_id=? AND kind='update'", link.AccountID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sqlite_sequence SET seq=(SELECT COALESCE(MAX(id),0) FROM account_links) WHERE name='account_links'"); err != nil {
		return nil, err
	}

	updated, err := scanAccountRows(tx.QueryRowContext(ctx, selectAccountSQL+" WHERE id=?", link.AccountID))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

// SameAuthorization 判断两份账号快照是否还是同一次授权（ID、openid、凭据三者都一致）。
//
// 比的是原始凭据 JSON 而不是解析后的 map：map 比较要处理浮点数、嵌套和类型差异，
// 而原始 JSON 是逐字节的，正是我们想确认的「还是不是同一版」。
func (a *WechatAccount) SameAuthorization(other *WechatAccount) bool {
	return a != nil && other != nil && a.ID == other.ID && a.OpenID == other.OpenID && a.credentialJSON == other.credentialJSON
}

// InvalidateSessionForAccount 删除某条授权对应的会话。
//
// 带 credentials IS ? 条件是有意的：如果凭据已经被刷新过，说明这条会话属于
// 上一版凭据，删掉是多余的；而如果凭据没变，说明会话确实属于这条授权，应当清掉。
func (db *DB) InvalidateSessionForAccount(ctx context.Context, account *WechatAccount, proxy string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM sessions WHERE wechat_account_id=? AND tcp_proxy=? AND EXISTS
(SELECT 1 FROM wechat_accounts WHERE id=? AND openid=? AND credentials IS ?)`, account.ID, proxy, account.ID, account.OpenID, account.credentialJSON)
	return err
}

// PutSessionForAccount 在写入会话时顺带校验账号授权没有被替换过。
//
// 场景：一次扫码登录还没走完，运营者用更新链接把账号换成了另一个微信号。
// 此时那条旧流程拿到的 uin 属于旧身份，写进去就会让账号声称自己是别人。
// 用 login_buffer 做条件，可以让迟到的旧请求直接失败而不是污染新身份。
func (db *DB) PutSessionForAccount(ctx context.Context, account *WechatAccount, uin int64, sessionBlob map[string]any, expiresAt int64, tcpProxy string) error {
	blob, err := json.Marshal(sessionBlob)
	if err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	// 三个前提条件缺一不可：
	//   · credentials 快照 —— 登录要几秒，期间保活可能已经轮换了 token。
	//     少了这条，旧会话会被写到新凭据上，下一次续期必然被拒。
	//   · 状态 —— expired / session_only 的账号不允许用一次新的 ManualAuth
	//     结果覆盖缓存会话，否则「授权已被拒绝」会被一次重新登录悄悄绕过。
	//   · openid + login_buffer —— 账号可能在登录期间被更新链接或合并换过身份。
	result, err := tx.ExecContext(ctx, `UPDATE wechat_accounts SET uin=? WHERE id=? AND openid=? AND login_buffer=?
AND credentials IS ? AND COALESCE(status,'unknown') NOT IN ('expired','session_only')`,
		uin, account.ID, account.OpenID, account.LoginBuffer, account.credentialJSON)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrAccountAuthorizationChanged
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions (wechat_account_id,uin,tcp_proxy,session_blob,expires_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(wechat_account_id,tcp_proxy) DO UPDATE SET
uin=excluded.uin,session_blob=excluded.session_blob,expires_at=excluded.expires_at,updated_at=excluded.updated_at`, account.ID, uin, tcpProxy, string(blob), expiresAt, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}
