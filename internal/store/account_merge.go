package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// MergeAccountAuthorization 把「来源账号」的授权移入「保留账号」，然后删除来源。
//
// 典型场景：扫码时误建了一条新记录，而原来那条记录带着完好的代理、备注与保活设置。
// 直接删旧建新会把这些配置一起丢掉，所以这里选择「保留目标 ID，只搬授权」。
//
// 不变量：
//   - 目标账号的 ID 不变，代理/备注/别名等按 ID 挂的配置全部保留；
//   - 来源账号被删除，同一事务内先删来源再写目标，避免 openid 唯一约束冲突；
//   - 两份快照在写入前都要确认没被改过（CAS），否则拒绝而不是覆盖。
func (db *DB) MergeAccountAuthorization(ctx context.Context, source, target *WechatAccount) (*WechatAccount, error) {
	if source == nil || target == nil {
		return nil, errors.New("merge requires source and target")
	}
	if source.ID == target.ID {
		return nil, errors.New("source and target must differ")
	}
	if source.LoginBuffer == "" || len(source.Credentials) == 0 {
		return nil, errors.New("source authorization is not usable")
	}
	if source.Status != nil && *source.Status == "expired" {
		return nil, errors.New("source authorization is expired")
	}
	userJSON, err := marshalNullable(source.UserInfo)
	if err != nil {
		return nil, err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 逐条复核快照：从预览到确认之间，任一方可能已经被保活刷新或重新扫码。
	// 这里用 SameAuthorization 比对 ID/openid/凭据三者，任一变化都视为「换了授权」。
	for _, snapshot := range []*WechatAccount{source, target} {
		current, err := scanAccountRows(tx.QueryRowContext(ctx, selectAccountSQL+" WHERE id=?", snapshot.ID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAccountAuthorizationChanged
		}
		if err != nil {
			return nil, err
		}
		if !current.SameAuthorization(snapshot) || current.LoginBuffer != snapshot.LoginBuffer {
			return nil, ErrAccountAuthorizationChanged
		}
	}

	// 先删来源：openid 是唯一索引，顺序反了会在写目标那一刻撞约束。
	if _, err = tx.ExecContext(ctx, "DELETE FROM wechat_accounts WHERE id=?", source.ID); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	// alias 只在目标原来为空时补上来源的别名：运营者设过的标注比来源账号的昵称更可信。
	_, err = tx.ExecContext(ctx, `UPDATE wechat_accounts SET openid=?, login_buffer=?, credentials=?,
nickname=?, alias=COALESCE(NULLIF(TRIM(alias),''), ?), avatar=?, user_info=?, uin=NULL,
status='alive', last_checked_at=?, updated_at=? WHERE id=?`,
		source.OpenID, source.LoginBuffer, source.credentialJSON, nullableString(source.Nickname),
		nullableString(source.Alias), nullableString(source.Avatar), userJSON, now, now, target.ID)
	if err != nil {
		return nil, err
	}
	// 目标账号的旧会话与待用链接都基于旧身份，必须一起清掉。
	if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE wechat_account_id=?", target.ID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM account_links WHERE account_id IN (?,?)", target.ID, source.ID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sqlite_sequence SET seq=(SELECT COALESCE(MAX(id),0) FROM account_links) WHERE name='account_links'"); err != nil {
		return nil, err
	}

	merged, err := scanAccountRows(tx.QueryRowContext(ctx, selectAccountSQL+" WHERE id=?", target.ID))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return merged, nil
}

// MergePreviewNote 是预览时给运营者看的一句话，说明这次操作到底改了什么、没改什么。
const MergePreviewNote = "保留目标 ID 及其代理、备注等配置；使用来源账号的授权，并删除来源 ID。" +
	"系统无法自动证明两条记录属于同一个微信，请自行核对身份后再确认。"
