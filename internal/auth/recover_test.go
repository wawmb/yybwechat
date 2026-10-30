package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRecoverAdminPasswordResetsAndRevokesSessions 覆盖本地口令恢复的两件事：
// 新口令生效、旧口令失效，并且**旧会话必须一起作废**。
//
// 第三条最容易漏：只改哈希不删会话，等于「口令改了但攻击者手上的 Cookie 还能用」，
// 恢复动作就成了心理安慰。
func TestRecoverAdminPasswordResetsAndRevokesSessions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "auth.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.CreateAdmin(ctx, "Admin", "oldpassword1"); err != nil {
		t.Fatalf("CreateAdmin() error = %v", err)
	}
	token, err := store.CreateSession(ctx, 1, "test-agent", "127.0.0.1", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if _, err := store.UserBySession(ctx, token); err != nil {
		t.Fatalf("会话应当有效: %v", err)
	}

	// 用户名大小写应被归一化：命令行里输入 Admin / admin 都能命中
	if err := store.RecoverAdminPassword(ctx, "  ADMIN ", "newpassword9"); err != nil {
		t.Fatalf("RecoverAdminPassword() error = %v", err)
	}

	if _, err := store.Authenticate(ctx, "admin", "oldpassword1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("旧口令应当失效，got %v", err)
	}
	if _, err := store.Authenticate(ctx, "admin", "newpassword9"); err != nil {
		t.Fatalf("新口令应当可用: %v", err)
	}
	if _, err := store.UserBySession(ctx, token); err == nil {
		t.Fatal("重置口令后旧会话必须失效")
	}
}

// TestRecoverAdminPasswordGuards 覆盖恢复口令的前置校验。
func TestRecoverAdminPasswordGuards(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "auth.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.CreateAdmin(ctx, "admin", "oldpassword1"); err != nil {
		t.Fatalf("CreateAdmin() error = %v", err)
	}

	// 口令过短：规则与创建管理员一致，避免恢复出比创建时更弱的口令
	if err := store.RecoverAdminPassword(ctx, "admin", "short"); err == nil {
		t.Fatal("短口令应被拒绝")
	}
	// 用户名不存在：要报错，而不是悄悄什么都不做
	if err := store.RecoverAdminPassword(ctx, "ghost", "newpassword9"); err == nil {
		t.Fatal("不存在的管理员应报错")
	}
}

// TestOpenExistingDoesNotCreateDatabase 验证恢复命令不会凭空建库。
//
// 如果这里退化成 Open()，一次「-data-root 写错」的重置就会新建一个空库，
// 命令输出「重置成功」而服务仍读旧库——这是最难排查的一种假成功。
func TestOpenExistingDoesNotCreateDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if _, err := OpenExisting(path); !errors.Is(err, ErrAuthDBNotFound) {
		t.Fatalf("期望 ErrAuthDBNotFound，got %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("OpenExisting 不应创建数据库文件")
	}

	// 已存在的库应当能正常打开并读到管理员
	real := filepath.Join(t.TempDir(), "real.db")
	store, err := Open(real)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := store.CreateAdmin(context.Background(), "admin", "oldpassword1"); err != nil {
		t.Fatalf("CreateAdmin() error = %v", err)
	}
	_ = store.Close()

	opened, err := OpenExisting(real)
	if err != nil {
		t.Fatalf("OpenExisting() error = %v", err)
	}
	defer opened.Close()
	if has, err := opened.HasAdmin(context.Background()); err != nil || !has {
		t.Fatalf("应读到已存在的管理员，got has=%v err=%v", has, err)
	}
}
