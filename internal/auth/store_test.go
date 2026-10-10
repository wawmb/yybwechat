package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStore 在临时目录里打开一个独立的认证库，并返回其文件路径。
// 返回路径是为了让测试能直接检查数据库文件内容（例如确认没有明文令牌落库）。
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// TestCreateAdminAndAuthenticate 覆盖管理员创建与口令校验的主链路。
func TestCreateAdminAndAuthenticate(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	if has, err := s.HasAdmin(ctx); err != nil || has {
		t.Fatalf("初始状态应无管理员: has=%v err=%v", has, err)
	}

	u, err := s.CreateAdmin(ctx, "admin", "StrongPass123")
	if err != nil {
		t.Fatalf("CreateAdmin() error = %v", err)
	}
	if u.ID == 0 || u.Username != "admin" {
		t.Fatalf("创建结果异常: %+v", u)
	}

	has, err := s.HasAdmin(ctx)
	if err != nil || !has {
		t.Fatalf("创建后应有管理员: has=%v err=%v", has, err)
	}

	// 正确口令应通过；错误口令必须返回哨兵错误，便于 HTTP 层区分 401 与 500
	ok, err := s.Authenticate(ctx, "admin", "StrongPass123")
	if err != nil || ok == nil || ok.ID != u.ID {
		t.Fatalf("正确口令认证失败: user=%v err=%v", ok, err)
	}
	if _, err := s.Authenticate(ctx, "admin", "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("错误口令应返回 ErrInvalidCredentials，实际: %v", err)
	}
	if _, err := s.Authenticate(ctx, "nobody", "StrongPass123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("不存在的用户名应返回 ErrInvalidCredentials，实际: %v", err)
	}
}

// TestCreateAdminRejectsDuplicateAndWeakPassword 覆盖重复创建与弱口令拒绝。
func TestCreateAdminRejectsDuplicateAndWeakPassword(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	if _, err := s.CreateAdmin(ctx, "admin", "StrongPass123"); err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	if _, err := s.CreateAdmin(ctx, "admin2", "AnotherPass123"); !errors.Is(err, ErrAdminExists) {
		t.Fatalf("重复创建应返回 ErrAdminExists，实际: %v", err)
	}
	if _, err := s.CreateAdmin(ctx, "short", "1234567"); err == nil {
		t.Fatal("7 位口令应被拒绝（要求至少 8 位）")
	}
}

// TestSessionLifecycle 覆盖会话令牌的签发、解析、注销，
// 并验证原始令牌不会以明文形式落库（库里只保存 SHA-256 摘要）。
func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	s, dbPath := newTestStore(t)

	u, err := s.CreateAdmin(ctx, "admin", "StrongPass123")
	if err != nil {
		t.Fatalf("CreateAdmin() error = %v", err)
	}

	token, err := s.CreateSession(ctx, u.ID, "test-agent", "127.0.0.1", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if len(token) < 32 {
		t.Fatalf("会话令牌过短(%d)，应为 32 字节随机数的 URL 安全 base64", len(token))
	}

	bySession, err := s.UserBySession(ctx, token)
	if err != nil || bySession == nil || bySession.ID != u.ID {
		t.Fatalf("凭令牌取用户失败: %v err=%v", bySession, err)
	}
	if got, _ := s.UserBySession(ctx, "not-a-real-token"); got != nil {
		t.Fatal("伪造令牌不应解析出用户")
	}

	// 关键安全断言：数据库文件里不应出现明文令牌
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("读取数据库文件失败: %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("数据库里出现了明文会话令牌，应只保存 SHA-256 摘要")
	}

	// 注销后令牌立即失效
	if err := s.DeleteSession(ctx, token); err != nil {
		t.Fatalf("DeleteSession() error = %v", err)
	}
	if got, _ := s.UserBySession(ctx, token); got != nil {
		t.Fatal("注销后令牌仍可用")
	}
}

// TestDeleteOtherSessionsKeepsCurrent 校验改密后其它设备的会话被踢下线，当前会话保留。
func TestDeleteOtherSessionsKeepsCurrent(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	u, err := s.CreateAdmin(ctx, "admin", "StrongPass123")
	if err != nil {
		t.Fatalf("CreateAdmin() error = %v", err)
	}
	current, err := s.CreateSession(ctx, u.ID, "current-agent", "127.0.0.1", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession(current) error = %v", err)
	}
	other, err := s.CreateSession(ctx, u.ID, "other-agent", "10.0.0.1", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession(other) error = %v", err)
	}

	n, err := s.DeleteOtherSessions(ctx, u.ID, current)
	if err != nil || n != 1 {
		t.Fatalf("DeleteOtherSessions() = %d, %v; 期望删除 1 条", n, err)
	}
	if got, _ := s.UserBySession(ctx, other); got != nil {
		t.Fatal("其它设备的会话应已失效")
	}
	if got, _ := s.UserBySession(ctx, current); got == nil {
		t.Fatal("当前会话应保留")
	}
}

// TestPurgeExpiredSessions 校验过期会话被清理、未过期的保留。
func TestPurgeExpiredSessions(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	u, err := s.CreateAdmin(ctx, "admin", "StrongPass123")
	if err != nil {
		t.Fatalf("CreateAdmin() error = %v", err)
	}
	live, err := s.CreateSession(ctx, u.ID, "agent", "127.0.0.1", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession(live) error = %v", err)
	}
	// 负 TTL 立即过期
	if _, err := s.CreateSession(ctx, u.ID, "agent", "127.0.0.1", -time.Second); err != nil {
		t.Fatalf("CreateSession(expired) error = %v", err)
	}

	n, err := s.PurgeExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("PurgeExpiredSessions() error = %v", err)
	}
	if n != 1 {
		t.Fatalf("清理数量 = %d，期望 1（只清过期的）", n)
	}
	if got, _ := s.UserBySession(ctx, live); got == nil {
		t.Fatal("未过期的会话不应被清理")
	}
}

// TestChangePasswordRequiresCorrectOldPassword 校验改密必须验证原口令，
// 且新口令同样受 8 位长度约束。
func TestChangePasswordRequiresCorrectOldPassword(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	u, err := s.CreateAdmin(ctx, "admin", "StrongPass123")
	if err != nil {
		t.Fatalf("CreateAdmin() error = %v", err)
	}
	if err := s.ChangePassword(ctx, u.ID, "wrong-old", "NewStrongPass1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("原口令错误应返回 ErrInvalidCredentials，实际: %v", err)
	}
	if err := s.ChangePassword(ctx, u.ID, "StrongPass123", "short"); err == nil {
		t.Fatal("新口令少于 8 位应被拒绝")
	}
	if err := s.ChangePassword(ctx, u.ID, "StrongPass123", "NewStrongPass1"); err != nil {
		t.Fatalf("合法改密应成功: %v", err)
	}
	if _, err := s.Authenticate(ctx, "admin", "NewStrongPass1"); err != nil {
		t.Fatalf("改密后应用新口令可登录: %v", err)
	}
	if _, err := s.Authenticate(ctx, "admin", "StrongPass123"); err == nil {
		t.Fatal("改密后旧口令不应再可用")
	}
}

// TestAuthDBPath 校验认证库路径拼接：空文件名回落到 auth.db。
func TestAuthDBPath(t *testing.T) {
	if got := AuthDBPath("/data/db", ""); got != filepath.Join("/data/db", "auth.db") {
		t.Fatalf("AuthDBPath(空文件名) = %q", got)
	}
	if got := AuthDBPath("/data/db", "custom.db"); got != filepath.Join("/data/db", "custom.db") {
		t.Fatalf("AuthDBPath(自定义) = %q", got)
	}
}
