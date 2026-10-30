package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadEnvFileParsingAndPrecedence 覆盖 .env 的解析规则与优先级。
//
// 两条规则尤其要守住：
//   - 已存在的环境变量不能被文件覆盖（容器 -e 注入优先）；
//   - 只输出条数、不回显值（.env 里通常是口令与 token）。
func TestLoadEnvFileParsingAndPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := strings.Join([]string{
		"# 注释行",
		"",
		"YYB_TEST_NEW=from-file",
		"export YYB_TEST_EXPORT=exported",
		`YYB_TEST_QUOTED="quoted value"`,
		"YYB_TEST_SINGLE='single value'",
		"YYB_TEST_WITH_HASH=abc#def",
		"YYB_TEST_KEEP=from-file",
		"not-a-pair-line",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	// 已存在的值必须赢
	t.Setenv("YYB_TEST_KEEP", "from-env")
	// 清掉可能被上一次运行污染的键
	for _, key := range []string{"YYB_TEST_NEW", "YYB_TEST_EXPORT", "YYB_TEST_QUOTED", "YYB_TEST_SINGLE", "YYB_TEST_WITH_HASH"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}

	var report bytes.Buffer
	if err := loadEnvFile(path, &report); err != nil {
		t.Fatalf("loadEnvFile() error = %v", err)
	}

	cases := map[string]string{
		"YYB_TEST_NEW":       "from-file",
		"YYB_TEST_EXPORT":    "exported",
		"YYB_TEST_QUOTED":    "quoted value",
		"YYB_TEST_SINGLE":    "single value",
		"YYB_TEST_WITH_HASH": "abc#def", // # 只在行首才是注释
		"YYB_TEST_KEEP":      "from-env",
	}
	for key, want := range cases {
		if got := os.Getenv(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}

	log := report.String()
	if !strings.Contains(log, "新增 5 项") || !strings.Contains(log, "跳过 1 项") {
		t.Fatalf("加载统计不对: %q", log)
	}
	// 不能回显任何值
	for _, secret := range []string{"from-file", "exported", "quoted value", "single value"} {
		if strings.Contains(log, secret) {
			t.Fatalf("日志不应包含具体值 %q: %q", secret, log)
		}
	}
}

// TestPrescanEnvFile 覆盖 .env 路径的四种写法。
func TestPrescanEnvFile(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-env-file", "/tmp/a.env"}, "/tmp/a.env"},
		{[]string{"--env-file=/tmp/b.env"}, "/tmp/b.env"},
		{[]string{"-port", "8000", "-env-file", "/tmp/c.env"}, "/tmp/c.env"},
		{[]string{"-port", "8000"}, ""},
		{[]string{"-env-file"}, ""},
	}
	for _, c := range cases {
		if got := prescanEnvFile(c.args); got != c.want {
			t.Fatalf("prescanEnvFile(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}

// TestResolveEnvFileFallsBackToWorkingDirectory 验证未显式指定时会去找当前目录的 .env。
func TestResolveEnvFileFallsBackToWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	if got := resolveEnvFile(""); got != "" {
		t.Fatalf("没有 .env 时应返回空串，got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if got := resolveEnvFile(""); got != ".env" {
		t.Fatalf("应发现当前目录的 .env，got %q", got)
	}
	if got := resolveEnvFile("/explicit/path.env"); got != "/explicit/path.env" {
		t.Fatalf("显式路径应优先，got %q", got)
	}
}
