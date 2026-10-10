package store

import (
	"context"
	"database/sql"
	"time"
)

// 本文件实现账号维护相关的持久化方法：备注修改、残缺账号清理与 ID 紧凑化。

// migrateAddRemark 为 wechat_accounts 表补充 remark 列（账号备注）。
// 兼容旧数据库：列不存在时执行 ALTER TABLE ADD COLUMN，已存在则跳过。
func migrateAddRemark(ctx context.Context, db *sql.DB) error {
	exists, err := sqliteTableExists(ctx, db, "wechat_accounts")
	if err != nil || !exists {
		return err
	}
	var n int
	err = db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('wechat_accounts') WHERE name='remark'").Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		_, err = db.ExecContext(ctx, "ALTER TABLE wechat_accounts ADD COLUMN remark TEXT")
	}
	return err
}

// SetAccountRemark 更新指定账号的备注，返回更新后的账号。
// 传入空字符串表示清除备注，数据库中以 NULL 存储。
func (db *DB) SetAccountRemark(ctx context.Context, id int64, remark string) (*WechatAccount, error) {
	if _, err := db.sql.ExecContext(ctx,
		"UPDATE wechat_accounts SET remark=?, updated_at=? WHERE id=?",
		nullableText(remark), time.Now().Unix(), id); err != nil {
		return nil, err
	}
	return db.GetAccount(ctx, id)
}

// nullableText 把字符串转为可空列值，空字符串存为 NULL。
func nullableText(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// IncompleteAccount 表示一条残缺的账号记录（扫码未完成或凭据缺失）。
type IncompleteAccount struct {
	ID     int64   `json:"id"`
	OpenID string  `json:"openid"`
	Status *string `json:"status,omitempty"`
	Reason string  `json:"reason"`
}

// ListIncompleteAccounts 列出残缺账号。
// 判定标准：缺少 openid、缺少 login_buffer 或缺少 credentials 之一的记录。
// 这类记录通常是扫码流程被中断留下的，正常业务无法使用。
func (db *DB) ListIncompleteAccounts(ctx context.Context) ([]IncompleteAccount, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT id, openid, status,
       CASE
           WHEN openid IS NULL OR openid = '' THEN 'missing_openid'
           WHEN credentials IS NULL OR credentials = '' THEN 'missing_credentials'
           ELSE 'missing_login_buffer'
       END AS reason
FROM wechat_accounts
WHERE (openid IS NULL OR openid = '')
   OR (credentials IS NULL OR credentials = '')
   OR (login_buffer IS NULL OR login_buffer = '')
ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IncompleteAccount
	for rows.Next() {
		var a IncompleteAccount
		var status sql.NullString
		if err := rows.Scan(&a.ID, &a.OpenID, &status, &a.Reason); err != nil {
			return nil, err
		}
		a.Status = stringPtrFromNull(status)
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteIncompleteAccounts 删除指定 ID 集合中的残缺账号。
// 删除前会在同一条 SQL 中二次复核残缺条件，防止预览与确认之间账号被补全后误删。
func (db *DB) DeleteIncompleteAccounts(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
DELETE FROM wechat_accounts
WHERE id = ?
  AND ((openid IS NULL OR openid = '')
    OR (credentials IS NULL OR credentials = '')
    OR (login_buffer IS NULL OR login_buffer = ''))`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	var total int64
	for _, id := range ids {
		res, err := stmt.ExecContext(ctx, id)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

// CompactAccountIDs 把账号主键重排为连续的 1..N，消除删除留下的空洞。
//
// 返回 oldID → newID 的映射；若无空洞则返回空映射，调用方应跳过后续处理。
// 实现上先把 ID 临时置为负值（避免主键冲突），再统一翻正，
// 同时同步更新 sessions 表的外键并重置 AUTOINCREMENT 序列。
func (db *DB) CompactAccountIDs(ctx context.Context) (map[int64]int64, error) {
	rows, err := db.sql.QueryContext(ctx, "SELECT id FROM wechat_accounts ORDER BY id")
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	mapping := make(map[int64]int64, len(ids))
	for i, id := range ids {
		newID := int64(i + 1)
		if id != newID {
			mapping[id] = newID
		}
	}
	if len(mapping) == 0 {
		return map[int64]int64{}, nil
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// 允许事务提交前存在外键悬空，否则中间态会被立即拒绝
	if _, err = tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
		return nil, err
	}

	parentStmt, err := tx.PrepareContext(ctx, "UPDATE wechat_accounts SET id=? WHERE id=?")
	if err != nil {
		return nil, err
	}
	defer parentStmt.Close()
	childStmt, err := tx.PrepareContext(ctx, "UPDATE sessions SET wechat_account_id=? WHERE wechat_account_id=?")
	if err != nil {
		return nil, err
	}
	defer childStmt.Close()

	// 第一步：全部迁移到负 ID，彻底避开与现有正 ID 的冲突
	for oldID, newID := range mapping {
		if _, err = parentStmt.ExecContext(ctx, -newID, oldID); err != nil {
			return nil, err
		}
		if _, err = childStmt.ExecContext(ctx, -newID, oldID); err != nil {
			return nil, err
		}
	}
	// 第二步：负值翻正，得到最终连续 ID
	if _, err = tx.ExecContext(ctx, "UPDATE wechat_accounts SET id=-id WHERE id<0"); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sessions SET wechat_account_id=-wechat_account_id WHERE wechat_account_id<0"); err != nil {
		return nil, err
	}
	// 重置自增序列，避免新账号拿到旧的最大 ID 造成空洞重现
	if _, err = tx.ExecContext(ctx,
		"UPDATE sqlite_sequence SET seq=(SELECT COALESCE(MAX(id),0) FROM wechat_accounts) WHERE name='wechat_accounts'"); err != nil {
		return nil, err
	}

	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return mapping, nil
}
