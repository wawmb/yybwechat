package store

import (
	"context"
	"database/sql"
	"time"
)

// AccountLink 是一条一次性授权链接。
//
// 设计要点：
//   - TokenHash 是查找键，链接的原始 token 不落库（另存一份加密副本供管理页展示）；
//   - ExpectedOpenID 决定「谁能用这条链接」：更新类链接必须由原账号扫码；
//   - TargetProduct 记录预期扫码渠道，跨渠道时要求 SwitchConfirmed；
//   - 一次性由 ConsumeAccountLink 的原子 UPDATE 保证，不靠应用层判断。
type AccountLink struct {
	ID              int64
	TokenHash       string
	Kind            string
	AccountID       int64
	OwnerUserID     *int64
	ExpectedOpenID  string
	TokenCiphertext string
	TargetProduct   string
	SwitchConfirmed bool
	ExpiresAt       int64
	UsedAt          *int64
	RevokedAt       *int64
	CreatedAt       int64
}

// AccountLinkRecord 在 AccountLink 之外附带一个由时间戳推导出的状态。
type AccountLinkRecord struct {
	AccountLink
	Status string `json:"status"`
}

func nullableInt64Ptr(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt64Value(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

// CreateAccountLinkForProduct 写入一条链接并返回落库后的记录。
func (db *DB) CreateAccountLinkForProduct(ctx context.Context, tokenHash, tokenCiphertext, kind string, accountID int64, ownerUserID *int64, expectedOpenID string, expiresAt int64, product string, switchConfirmed bool) (*AccountLink, error) {
	now := time.Now().Unix()
	result, err := db.sql.ExecContext(ctx, `INSERT INTO account_links
		(token_hash, token_ciphertext, kind, account_id, owner_user_id, expected_openid, expires_at, created_at, target_product, switch_confirmed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, tokenHash, tokenCiphertext, kind, accountID, nullableInt64Ptr(ownerUserID), expectedOpenID, expiresAt, now, product, switchConfirmed)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return db.GetAccountLink(ctx, id)
}

// GetAccountLink 按主键读取一条链接。
func (db *DB) GetAccountLink(ctx context.Context, id int64) (*AccountLink, error) {
	link := &AccountLink{}
	var owner, used, revoked sql.NullInt64
	err := db.sql.QueryRowContext(ctx, `SELECT id, token_hash, token_ciphertext, kind, account_id, owner_user_id,
		expected_openid, expires_at, used_at, revoked_at, created_at, target_product, switch_confirmed FROM account_links WHERE id=?`, id).Scan(
		&link.ID, &link.TokenHash, &link.TokenCiphertext, &link.Kind, &link.AccountID, &owner,
		&link.ExpectedOpenID, &link.ExpiresAt, &used, &revoked, &link.CreatedAt, &link.TargetProduct, &link.SwitchConfirmed)
	if err != nil {
		return nil, err
	}
	link.OwnerUserID = nullableInt64Value(owner)
	link.UsedAt = nullableInt64Value(used)
	link.RevokedAt = nullableInt64Value(revoked)
	return link, nil
}

// GetAccountLinkByHash 按 token 哈希查找链接。链接的原始 token 只出现在 URL 里，
// 服务端始终用哈希比对，避免日志或数据库快照泄露可用链接。
func (db *DB) GetAccountLinkByHash(ctx context.Context, tokenHash string) (*AccountLink, error) {
	var id int64
	if err := db.sql.QueryRowContext(ctx, "SELECT id FROM account_links WHERE token_hash=?", tokenHash).Scan(&id); err != nil {
		return nil, err
	}
	return db.GetAccountLink(ctx, id)
}

// ConsumeAccountLink 原子地占用一条链接。
//
// 返回 false 表示已经有人抢先用了、已作废或已过期。这里刻意用带条件的
// UPDATE 而不是「先查后改」：两条并发的 confirm 请求必须只有一条能拿到 true，
// 否则同一个一次性链接就能替换两次授权。
func (db *DB) ConsumeAccountLink(ctx context.Context, id int64) (bool, error) {
	now := time.Now().Unix()
	result, err := db.sql.ExecContext(ctx, `UPDATE account_links SET used_at=?
		WHERE id=? AND used_at IS NULL AND revoked_at IS NULL AND expires_at>?`, now, id, now)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// PurgeExpiredAccountLinks 清理过期、已消费与已作废的链接。
func (db *DB) PurgeExpiredAccountLinks(ctx context.Context) (int64, error) {
	now := time.Now().Unix()
	result, err := db.sql.ExecContext(ctx, `DELETE FROM account_links
		WHERE expires_at<=? OR used_at IS NOT NULL OR revoked_at IS NOT NULL`, now)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count > 0 {
		if err = db.reclaimAccountLinkSequence(ctx); err != nil {
			return count, err
		}
	}
	return count, nil
}

// reclaimAccountLinkSequence 让 SQLite 在一次性的链接被删除后复用编号。
//
// 链接 ID 只是管理页的句柄（真正的凭证是 token），没有任何理由保留一个只增不减
// 的序列；复用能让「链接 3」这种显示名在长期运行里始终保持在个位数量级。
func (db *DB) reclaimAccountLinkSequence(ctx context.Context) error {
	var maxID sql.NullInt64
	if err := db.sql.QueryRowContext(ctx, "SELECT MAX(id) FROM account_links").Scan(&maxID); err != nil {
		return err
	}
	if !maxID.Valid {
		_, err := db.sql.ExecContext(ctx, "DELETE FROM sqlite_sequence WHERE name='account_links'")
		return err
	}
	_, err := db.sql.ExecContext(ctx, "UPDATE sqlite_sequence SET seq=? WHERE name='account_links'", maxID.Int64)
	return err
}

// ListAccountLinks 列出链接，按创建时间倒序。
func (db *DB) ListAccountLinks(ctx context.Context, ownerUserID *int64) ([]AccountLinkRecord, error) {
	query := `SELECT id, token_hash, token_ciphertext, kind, account_id, owner_user_id, expected_openid, expires_at, used_at, revoked_at, created_at, target_product, switch_confirmed FROM account_links`
	args := []any{}
	if ownerUserID != nil {
		query += " WHERE owner_user_id=?"
		args = append(args, *ownerUserID)
	}
	query += " ORDER BY created_at DESC, id DESC"
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountLinkRecord
	now := time.Now().Unix()
	for rows.Next() {
		var item AccountLinkRecord
		var owner, used, revoked sql.NullInt64
		if err := rows.Scan(&item.ID, &item.TokenHash, &item.TokenCiphertext, &item.Kind, &item.AccountID, &owner, &item.ExpectedOpenID, &item.ExpiresAt, &used, &revoked, &item.CreatedAt, &item.TargetProduct, &item.SwitchConfirmed); err != nil {
			return nil, err
		}
		item.OwnerUserID, item.UsedAt, item.RevokedAt = nullableInt64Value(owner), nullableInt64Value(used), nullableInt64Value(revoked)
		switch {
		case item.RevokedAt != nil:
			item.Status = "revoked"
		case item.UsedAt != nil:
			item.Status = "used"
		case item.ExpiresAt <= now:
			item.Status = "expired"
		default:
			item.Status = "active"
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListActiveAccountLinksForTarget 找出同一目标上仍然有效的链接。
//
// 用于「同一账号只允许存在一条未消费链接」的互斥：否则运营者点两次就会生成
// 两条链接，哪一条先被扫到就替换成哪个身份，事后无从判断是哪次操作生效的。
// kind=add 的链接不与具体账号绑定，因此按 kind 整体判断。
func (db *DB) ListActiveAccountLinksForTarget(ctx context.Context, kind string, accountID int64, ownerUserID *int64) ([]AccountLinkRecord, error) {
	items, err := db.ListAccountLinks(ctx, ownerUserID)
	if err != nil {
		return nil, err
	}
	out := make([]AccountLinkRecord, 0)
	for _, item := range items {
		if item.Status == "active" && item.Kind == kind && (kind == "add" || item.AccountID == accountID) {
			out = append(out, item)
		}
	}
	return out, nil
}

// RevokeAccountLink 作废一条链接（直接从表里删掉）。
func (db *DB) RevokeAccountLink(ctx context.Context, id int64, ownerUserID *int64) error {
	query := "DELETE FROM account_links WHERE id=?"
	args := []any{id}
	if ownerUserID != nil {
		query += " AND owner_user_id=?"
		args = append(args, *ownerUserID)
	}
	result, err := db.sql.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return db.reclaimAccountLinkSequence(ctx)
}

// DeleteAccountLink 删除一条链接（已消费的链接用它回收）。
func (db *DB) DeleteAccountLink(ctx context.Context, id int64, ownerUserID *int64) error {
	query := "DELETE FROM account_links WHERE id=?"
	args := []any{id}
	if ownerUserID != nil {
		query += " AND owner_user_id=?"
		args = append(args, *ownerUserID)
	}
	result, err := db.sql.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return db.reclaimAccountLinkSequence(ctx)
}
