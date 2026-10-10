package version

import "testing"

// TestInfoReturnsNonEmptyVersion 校验未注入 ldflags 时仍有可用版本号，
// 避免 /api/version 暴露空字符串给前端。
func TestInfoReturnsNonEmptyVersion(t *testing.T) {
	ver, commit, buildDate := Info()
	if ver == "" {
		t.Fatal("版本号不应为空")
	}
	if commit == "" {
		t.Fatal("提交号不应为空（无 VCS 信息时应回落为 unknown）")
	}
	if buildDate == "" {
		t.Fatal("构建时间不应为空（无 VCS 信息时应回落为 unknown）")
	}
}

// TestCommitIsTruncated 校验提交号被截断为 7 位，与常见 Git short hash 一致。
func TestCommitIsTruncated(t *testing.T) {
	original := Commit
	defer func() { Commit = original }()

	Commit = "0123456789abcdef0123456789abcdef01234567"
	if _, commit, _ := Info(); commit != "0123456" {
		t.Fatalf("提交号 = %q，期望截断为前 7 位", commit)
	}

	// 短提交号不应被破坏
	Commit = "abc"
	if _, commit, _ := Info(); commit != "abc" {
		t.Fatalf("短提交号 = %q，期望原样保留", commit)
	}
}

// TestEmptyVersionFallsBack 校验版本号被意外置空时能回落到默认值。
func TestEmptyVersionFallsBack(t *testing.T) {
	original := Version
	defer func() { Version = original }()

	Version = "   "
	if ver, _, _ := Info(); ver == "" {
		t.Fatal("版本号为空时应回落到默认值，而不是返回空字符串")
	}
}
