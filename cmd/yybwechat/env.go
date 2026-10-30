package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// envFileFlagName 是 .env 路径参数的两种写法（Go 的 flag 同时接受单/双横线）。
const envFileName = "env-file"

// prescanEnvFile 在注册 flag 之前，从命令行里找出 .env 的路径。
//
// 为什么必须提前：这个程序所有 flag 的默认值都来自环境变量（envOr），
// 而 flag 一旦注册，默认值就固定了。所以 .env 必须在注册之前读进进程环境，
// 否则文件里的配置对 flag 默认值完全无效——那正是「配了 .env 却不生效」的典型坑。
func prescanEnvFile(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name := strings.TrimLeft(arg, "-")
		if value, ok := strings.CutPrefix(name, envFileName+"="); ok {
			return value
		}
		if name == envFileName && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// resolveEnvFile 决定要加载哪个 .env：显式指定优先，否则依次尝试
// 可执行文件同目录、当前工作目录。两者都不存在时返回空串（不是错误）。
func resolveEnvFile(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), ".env")
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	if info, err := os.Stat(".env"); err == nil && info.Mode().IsRegular() {
		return ".env"
	}
	return ""
}

// loadEnvFile 把 .env 里的键值写进进程环境。
//
// 规则刻意保守：
//   - 已存在的环境变量**不覆盖**——命令行/容器注入的配置优先级最高，
//     否则一个陈旧的 .env 会悄悄盖掉你在 docker run -e 里写的值；
//   - 只接受 KEY=VALUE，忽略空行与 # 注释，容忍行首 export；
//   - 值两侧的成对引号会被去掉，其余字符原样保留（口令里可能有空格或 #）。
func loadEnvFile(path string, report io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开 %s 失败: %w", path, err)
	}
	defer file.Close()

	loaded, kept := 0, 0
	scanner := bufio.NewScanner(file)
	// 单行可能很长（例如长 token），放宽到 1MB
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if _, exists := os.LookupEnv(key); exists {
			kept++
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("设置 %s 失败: %w", key, err)
		}
		loaded++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	// 只报告数量，不回显任何值：.env 里通常有口令和 token
	fmt.Fprintf(report, "[env] 已加载 %s：新增 %d 项，跳过 %d 项（已存在的环境变量优先）\n", path, loaded, kept)
	return nil
}
