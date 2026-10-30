package httpapi

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// 内嵌资源必须在没有外部 resource 目录时也能撑起整个控制台。
// 释放逻辑是「补齐不覆盖」：已有同名文件一律保留，所以部署方改过的模板
// 不会因为升级被悄悄冲掉。
func TestEnsureResourcesRestoresEmbeddedWebAssetsWithoutOverwriting(t *testing.T) {
	root := t.TempDir()
	res, err := ensureResources(root, root, false)
	if err != nil {
		t.Fatalf("ensure resources: %v", err)
	}

	loginPath := filepath.Join(res.Templates, "login.html")
	login, err := os.ReadFile(loginPath)
	if err != nil {
		t.Fatalf("read restored login template: %v", err)
	}
	if !bytes.Contains(login, []byte("<!doctype html>")) {
		t.Fatalf("restored login template does not contain HTML: %q", login)
	}
	// auth.css 已随统一设计基线删除，现在的设计系统只有 console.css
	if _, err := os.Stat(filepath.Join(res.Static, "css", "console.css")); err != nil {
		t.Fatalf("restored console stylesheet: %v", err)
	}

	custom := []byte("custom login template")
	if err := os.WriteFile(loginPath, custom, 0o644); err != nil {
		t.Fatalf("write custom login template: %v", err)
	}
	if _, err := ensureResources(root, root, false); err != nil {
		t.Fatalf("ensure resources again: %v", err)
	}
	after, err := os.ReadFile(loginPath)
	if err != nil {
		t.Fatalf("read custom login template: %v", err)
	}
	if !bytes.Equal(after, custom) {
		t.Fatalf("custom login template was overwritten: %q", after)
	}
}

// 版本变了必须换释放目录，否则新版本会读到上一版留下的旧页面。
func TestStandaloneWebAssetsUseVersionedDirectory(t *testing.T) {
	root := t.TempDir()
	old, err := ensureResourcesVersion(root, root, true, "0.2.16")
	if err != nil {
		t.Fatalf("restore old version: %v", err)
	}
	if err := os.WriteFile(filepath.Join(old.Templates, "login.html"), []byte("stale page"), 0o644); err != nil {
		t.Fatalf("seed old page: %v", err)
	}

	next, err := ensureResourcesVersion(root, root, true, "0.2.17")
	if err != nil {
		t.Fatalf("restore next version: %v", err)
	}
	if next.Templates == old.Templates || next.Static == old.Static {
		t.Fatalf("web roots were reused: old=%#v next=%#v", old, next)
	}
	login, err := os.ReadFile(filepath.Join(next.Templates, "login.html"))
	if err != nil {
		t.Fatalf("read next login page: %v", err)
	}
	if bytes.Equal(login, []byte("stale page")) || !bytes.Contains(login, []byte("<!doctype html>")) {
		t.Fatalf("next version restored stale page: %q", login)
	}
	// 数据目录不能跟着版本目录一起搬家，否则数据库会被当成新库
	nextDB := filepath.Join(root, "db")
	if _, err := os.Stat(nextDB); err != nil {
		t.Fatalf("runtime database directory moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "avatars")); err != nil {
		t.Fatalf("runtime avatars directory moved: %v", err)
	}
}
