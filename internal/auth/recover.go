package auth

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// ErrAuthDBNotFound 表示认证数据库文件不存在。
//
// 本地恢复口令时不新建空库：新建出来的库会让「重置成功」变成假象——
// 服务实际读的是另一个文件，用户重置完仍然登不上。
var ErrAuthDBNotFound = errors.New("认证数据库文件不存在")

// ValidatePassword 校验口令是否满足最低要求。
//
// 抽出来是为了让「创建管理员」「改口令」「本地恢复」三处共用同一套规则：
// 三处各写一份长度判断，迟早会出现恢复出 6 位口令却能登录的情况。
func ValidatePassword(password string) error {
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	return nil
}

// OpenExisting 打开已存在的认证数据库，不新建文件、不初始化表。
//
// 与 Open 的区别只在于「文件必须已存在」，建表语句仍是 IF NOT EXISTS，
// 因此对已有库没有任何副作用。
func OpenExisting(path string) (*Store, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrAuthDBNotFound
	}
	return Open(path)
}

// RecoverAdminPassword 在本地重置管理员口令，并注销该管理员的全部会话。
//
// 与 ChangePassword 的唯一区别是「不需要原口令」——它专门用于口令彻底遗忘的场景，
// 因此只能在拥有认证库文件访问权的主机上通过 CLI 执行，绝不能暴露成 HTTP 接口：
// 一个不需要原口令的改密接口等于把服务直接交出去。
func (s *Store) RecoverAdminPassword(ctx context.Context, username, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	username = normalizeUsername(username)
	if username == "" {
		return errors.New("username is required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return err
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var id int64
	if err = tx.QueryRowContext(ctx, "SELECT id FROM admin_users WHERE username=?", username).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("未找到该管理员；请核对用户名，或用 -auth-db / YYB_DATA_ROOT 指向正确的认证库")
		}
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE admin_users SET password_hash=? WHERE id=?", string(hash), id); err != nil {
		return err
	}
	// 旧会话必须一起作废。否则拿到旧 Cookie 的人仍然能继续操作，
	// 「重置口令」就没有真正收回控制权。
	if _, err = tx.ExecContext(ctx, "DELETE FROM admin_sessions WHERE user_id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

// normalizeUsername 统一用户名的大小写与首尾空白，避免「Admin」与「admin」被当成两个账号。
func normalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}
