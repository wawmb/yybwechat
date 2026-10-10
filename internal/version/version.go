// Package version 暴露构建元数据，使 HTTP 层无需耦合具体的构建工具或发布流程。
//
// 版本号、提交号、构建时间三个变量在正式构建时由 -ldflags 注入：
//
//	go build -ldflags "-X yybwechat/internal/version.Version=1.0.0 ..."
//
// 未注入时保留有意义的默认值，并回退读取 Go 内嵌的 VCS 信息，
// 保证从源码直接构建也能识别出提交号。
package version

import (
	"runtime/debug"
	"strings"
)

// 以下三个变量由发布构建的 -ldflags 覆盖。
var (
	Version   = "1.0.0"   // 语义化版本号
	Commit    = "unknown" // Git 提交号
	BuildDate = "unknown" // 构建时间戳
)

// Info 返回构建元数据 (version, commit, buildDate)。
// 当二进制直接从源码检出目录构建（未传 ldflags）时，
// 回退使用 debug.ReadBuildInfo 中的 vcs.revision 与 vcs.time。
func Info() (string, string, string) {
	version, commit, buildDate := Version, Commit, BuildDate
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				if commit == "unknown" && setting.Value != "" {
					commit = setting.Value
				}
			case "vcs.time":
				if buildDate == "unknown" && setting.Value != "" {
					buildDate = setting.Value
				}
			}
		}
	}
	if strings.TrimSpace(version) == "" {
		version = "1.0.0"
	}
	// 提交号取前 7 位，与常见 Git short hash 展示习惯一致
	if len(commit) > 7 {
		commit = commit[:7]
	}
	return version, commit, buildDate
}
