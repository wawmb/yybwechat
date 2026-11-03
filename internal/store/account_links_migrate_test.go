package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestAccountLinksColumnsMigrateLegacyDatabase 守住老库升级：
//
// account_links 的建表语句后来加了跨渠道相关列，而 CREATE TABLE IF NOT EXISTS
// 对已存在的表什么都不做。不做迁移的话，旧库升级后短链的列表 / 签发 / 公网页面
// 会一起报 no such column。
func TestAccountLinksColumnsMigrateLegacyDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// 造一张「旧版本」的表：只有最早那几列。
	if _, err = db.sql.ExecContext(ctx, "DROP TABLE account_links"); err != nil {
		t.Fatal(err)
	}
	const legacy = `CREATE TABLE account_links (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		token_hash      TEXT    NOT NULL UNIQUE,
		kind            TEXT    NOT NULL,
		account_id      INTEGER NOT NULL,
		expected_openid TEXT    NOT NULL DEFAULT '',
		expires_at      INTEGER NOT NULL,
		used_at         INTEGER,
		created_at      INTEGER NOT NULL
	)`
	if _, err = db.sql.ExecContext(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	// 重新打开：迁移应该把缺的列补上。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("老库重新打开失败: %v", err)
	}
	defer reopened.Close()

	for _, column := range []string{"token_ciphertext", "owner_user_id", "revoked_at", "target_product", "switch_confirmed"} {
		var n int
		if err = reopened.sql.QueryRowContext(ctx,
			"SELECT count(*) FROM pragma_table_info('account_links') WHERE name=?", column).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatalf("迁移后仍缺列 %s", column)
		}
	}

	// 真正能查：这条 SQL 用到了 target_product 与 switch_confirmed。
	if _, err = reopened.ListAccountLinks(ctx, nil); err != nil {
		t.Fatalf("迁移后查询短链失败: %v", err)
	}

	// 幂等：再开一次不应报错。
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("第二次打开失败（迁移不幂等）: %v", err)
	}
	again.Close()
}
