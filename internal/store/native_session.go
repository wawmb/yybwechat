package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrSessionChanged 表示会话或账号授权在本次替换期间已被改写。
var ErrSessionChanged = errors.New("protocol session or account authorization changed")

// ReplaceNativeSessionForAccount 只轮换已存在的、同一出口的会话。
//
// 与一次全新的 ManualAuth 登录不同：它**允许**保留 session_only 账号的原生授权。
// 换句话说，OAuth 被拒并不妨碍我们把协议层拿到的新会话密钥写回去——这正是
// v0.2.52 的核心观察：授权层与会话层是两件事。
//
// 它不会改动 OAuth 凭据、扫码时间或账号状态。
// WHERE 里带上整份旧 blob 与账号身份快照做 CAS：包括「已消费的密钥标记」在内，
// 任何并发写入都会让这次替换失败，而不是互相覆盖。
func (db *DB) ReplaceNativeSessionForAccount(ctx context.Context, account *WechatAccount, previous *SessionRow, uin int64, sessionBlob map[string]any, expiresAt int64) (*SessionRow, error) {
	if account == nil || previous == nil || previous.WechatAccountID != account.ID || previous.blobJSON == "" {
		return nil, ErrSessionChanged
	}
	blob, err := json.Marshal(sessionBlob)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	result, err := db.sql.ExecContext(ctx, `UPDATE sessions SET uin=?,session_blob=?,expires_at=?,updated_at=?
WHERE id=? AND wechat_account_id=? AND tcp_proxy=? AND session_blob=? AND EXISTS
(SELECT 1 FROM wechat_accounts WHERE id=? AND openid=? AND login_buffer=? AND credentials IS ?
AND COALESCE(status,'unknown')<>'expired')`, uin, string(blob), expiresAt, now,
		previous.ID, account.ID, previous.TCPProxy, previous.blobJSON,
		account.ID, account.OpenID, account.LoginBuffer, account.credentialJSON)
	if err != nil {
		return nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, ErrSessionChanged
	}
	next := *previous
	next.UIN, next.SessionBlob, next.ExpiresAt, next.UpdatedAt, next.blobJSON = &uin, sessionBlob, expiresAt, now, string(blob)
	return &next, nil
}
