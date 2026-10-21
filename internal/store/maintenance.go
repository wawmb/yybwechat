package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// 本文件实现账号维护相关的持久化方法：备注修改、残缺账号清理与 ID 紧凑化。

// migrateAddStatusNotified 为 wechat_accounts 表补充 status_notified 列。
//
// 该列记录「最近一次成功推送出去的状态」。有了它，推送失败就不必只留一行日志：
// 状态已落库为 expired 而 status_notified 仍停在旧值，下一轮扫描就能识别出
// 这是一条尚未送达的变更并重推。
//
// 首次升级时会把现有状态回填为「已通知」，避免升级后把所有历史账号补推一遍。
func migrateAddStatusNotified(ctx context.Context, db *sql.DB) error {
	exists, err := sqliteTableExists(ctx, db, "wechat_accounts")
	if err != nil || !exists {
		return err
	}
	var n int
	if err = db.QueryRowContext(ctx,
		"SELECT count(*) FROM pragma_table_info('wechat_accounts') WHERE name='status_notified'").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err = db.ExecContext(ctx,
			"ALTER TABLE wechat_accounts ADD COLUMN status_notified TEXT"); err != nil {
			return err
		}
	}
	// 只回填 NULL 行：推送失败留下的旧值（如 alive）不会被这里覆盖，
	// 因此「已失败待重试」的记录可以跨重启保留。
	_, err = db.ExecContext(ctx,
		"UPDATE wechat_accounts SET status_notified=status WHERE status_notified IS NULL AND status IS NOT NULL")
	return err
}

// PendingNotification 描述一次尚未成功送达的状态变更。
type PendingNotification struct {
	AccountID      int64
	OpenID         string
	Nickname       *string
	UIN            *int64
	Status         string
	NotifiedStatus string
}

// MarkStatusNotified 记录某个状态已成功推送。
//
// 刻意不更新 updated_at：推送成功不代表账号数据发生变化，
// 否则「最近更新时间」会被通知动作污染。
func (db *DB) MarkStatusNotified(ctx context.Context, id int64, status string) error {
	_, err := db.sql.ExecContext(ctx,
		"UPDATE wechat_accounts SET status_notified=? WHERE id=?", status, id)
	return err
}

// ListPendingNotifications 列出状态已收敛但尚未成功推送的账号。
//
// 只关心 alive 与 expired 两个终态：unknown 表示本轮判不出来，
// 推送它只会制造告警噪音，等下一轮收敛到终态再说。
func (db *DB) ListPendingNotifications(ctx context.Context) ([]PendingNotification, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT id, openid, nickname, uin, status, COALESCE(status_notified,'')
FROM wechat_accounts
WHERE status IN ('alive','expired')
  AND (status_notified IS NULL OR status_notified <> status)
ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pending := make([]PendingNotification, 0)
	for rows.Next() {
		var item PendingNotification
		if err := rows.Scan(&item.AccountID, &item.OpenID, &item.Nickname, &item.UIN, &item.Status, &item.NotifiedStatus); err != nil {
			return nil, err
		}
		pending = append(pending, item)
	}
	return pending, rows.Err()
}

// migrateAddExpiryAlerted 为 wechat_accounts 表补充 expiry_alerted_at 列。
//
// 该列记录「最近一次成功发出『凭据已过期且续不上』告警的时间」。
// 它只用于去重：成功推送才写入，推送失败保持原值，因此下一轮扫描会自然重试；
// 超过重提醒窗口仍未恢复时再提醒一次，避免账号长期瘫痪却只响过一声。
func migrateAddExpiryAlerted(ctx context.Context, db *sql.DB) error {
	exists, err := sqliteTableExists(ctx, db, "wechat_accounts")
	if err != nil || !exists {
		return err
	}
	var n int
	if err = db.QueryRowContext(ctx,
		"SELECT count(*) FROM pragma_table_info('wechat_accounts') WHERE name='expiry_alerted_at'").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		_, err = db.ExecContext(ctx,
			"ALTER TABLE wechat_accounts ADD COLUMN expiry_alerted_at INTEGER NOT NULL DEFAULT 0")
	}
	return err
}

// expiredNotifyRepairFlag 是「历史失效账号补推」一次性修复在 settings 表里的标记键。
const expiredNotifyRepairFlag = "expired_notify_repair_v1"

// migrateRepairExpiredNotifications 把升级前就已失效、但从未推送出去的账号
// 重新标记为待推送。
//
// 背景：status_notified 列首次加入时，迁移把存量账号回填成 status_notified=status，
// 这对 alive 账号是对的，却把「升级前就已 expired」的账号当成了已推送——
// expired 是终态，之后再没有任何状态跃迁，这些账号的掉线告警就永远沉默了。
//
// 修复方式：把 expired 账号的 status_notified 清空，下一轮扫描的补推逻辑
// （retryPendingNotifications）会把它们各推一次，成功后重新记账。
// 用 settings 标记保证整个部署生命周期只执行一次，避免每次重启都重推；
// 推送出去之后 status_notified 记为 expired，同样不会再重复。
func migrateRepairExpiredNotifications(ctx context.Context, db *sql.DB) error {
	for _, table := range []string{"wechat_accounts", "settings"} {
		exists, err := sqliteTableExists(ctx, db, table)
		if err != nil || !exists {
			return err
		}
	}
	var done string
	err := db.QueryRowContext(ctx,
		"SELECT value FROM settings WHERE key=?", expiredNotifyRepairFlag).Scan(&done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if done == "1" {
		return nil
	}
	if _, err = db.ExecContext(ctx,
		"UPDATE wechat_accounts SET status_notified='' WHERE status='expired'"); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		"INSERT INTO settings(key, value, updated_at) VALUES(?,?,?) "+
			"ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at",
		expiredNotifyRepairFlag, "1", time.Now().Unix())
	return err
}

// ExpiryAlertedAt 返回上次成功发出「续期失败」告警的时间戳，0 表示从未发出。
func (db *DB) ExpiryAlertedAt(ctx context.Context, id int64) (int64, error) {
	var ts sql.NullInt64
	err := db.sql.QueryRowContext(ctx,
		"SELECT expiry_alerted_at FROM wechat_accounts WHERE id=?", id).Scan(&ts)
	if err != nil {
		return 0, err
	}
	return ts.Int64, nil
}

// MarkExpiryAlerted 记录一次成功的「续期失败」告警。
func (db *DB) MarkExpiryAlerted(ctx context.Context, id int64, ts int64) error {
	_, err := db.sql.ExecContext(ctx,
		"UPDATE wechat_accounts SET expiry_alerted_at=? WHERE id=?", ts, id)
	return err
}

// ClearExpiryAlerted 结束当前故障周期（凭据续上了，或账号被重新扫码）。
func (db *DB) ClearExpiryAlerted(ctx context.Context, id int64) error {
	_, err := db.sql.ExecContext(ctx,
		"UPDATE wechat_accounts SET expiry_alerted_at=0 WHERE id=?", id)
	return err
}

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
