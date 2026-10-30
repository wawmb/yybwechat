// Package httpapi 提供 yybwechat 服务的 HTTP API 层实现。
// 包含路由注册、请求处理、资源管理、OpenAPI 规范等。
package httpapi

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"yybwechat/internal/auth"
	"yybwechat/internal/version"
	embeddedresource "yybwechat/resource"
)

// webAssetVersion 只认 x.y.z 三段版本号：用它给释放目录命名，
// 版本一变就换目录，不会让旧版本留下的文件继续被新版本读到。
// 开发构建（dev / 带后缀）一律归到 vdev。
var webAssetVersion = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$`)

// resources 定义运行时资源目录结构。
// 模板和静态资源存放在 resourceRoot 下，持久化数据存放在 dataRoot 下，
// 二者可独立挂载，避免模板被数据卷覆盖。
type resources struct {
	DB        string // SQLite 数据库目录
	Avatars   string // 用户头像缓存目录
	QR        string // 二维码图片缓存目录
	Templates string // HTML 模板目录
	Static    string // 静态资源目录（css/js 等）
}

// ensureResources 根据指定的资源根和数据根创建子目录并返回 resources 结构体。
//
// preferEmbedded 为真时，模板与静态资源改用「资源根/.web-assets/v<版本>」，
// 并先把二进制里内嵌的那一份释放进去。这样独立二进制不必外带 resource 目录，
// 也不会去覆盖部署方自己挂载的 resource/templates。
func ensureResources(resourceRoot, dataRoot string, preferEmbedded bool) (resources, error) {
	buildVersion, _, _ := version.Info()
	return ensureResourcesVersion(resourceRoot, dataRoot, preferEmbedded, buildVersion)
}

func ensureResourcesVersion(resourceRoot, dataRoot string, preferEmbedded bool, buildVersion string) (resources, error) {
	webRoot := resourceRoot
	if preferEmbedded {
		if !webAssetVersion.MatchString(buildVersion) {
			buildVersion = "dev"
		}
		webRoot = filepath.Join(resourceRoot, ".web-assets", "v"+buildVersion)
	}
	res := resources{
		DB:        filepath.Join(dataRoot, "db"),
		Avatars:   filepath.Join(dataRoot, "avatars"),
		QR:        filepath.Join(dataRoot, "qr"),
		Templates: filepath.Join(webRoot, "templates"),
		Static:    filepath.Join(webRoot, "static"),
	}
	for _, p := range []string{res.DB, res.Avatars, res.QR, res.Templates,
		filepath.Join(res.Static, "css"), filepath.Join(res.Static, "js")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return res, err
		}
	}
	if err := restoreEmbeddedWebAssets(webRoot); err != nil {
		return res, err
	}
	return res, nil
}

// restoreEmbeddedWebAssets 把内嵌的模板与静态文件写到 root 下。
// 已存在的同名文件一律跳过：部署方自己改过的模板不能被升级悄悄覆盖掉。
func restoreEmbeddedWebAssets(root string) error {
	return fs.WalkDir(embeddedresource.WebAssets, ".", func(assetPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if assetPath == "." {
			return nil
		}

		target := filepath.Join(root, filepath.FromSlash(assetPath))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if _, err := os.Stat(target); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}

		content, err := fs.ReadFile(embeddedresource.WebAssets, assetPath)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
}

// avatarPath 根据 openid 生成头像文件的本地存储路径。
func (r resources) avatarPath(openid string) string {
	return filepath.Join(r.Avatars, safeName(openid)+".jpg")
}

// qrPath 根据会话 ID 生成二维码图片的本地存储路径。
func (r resources) qrPath(sessionID string) string {
	return filepath.Join(r.QR, sessionID+".jpg")
}

// isSafeFileToken 判断一个来自外部的标识符能否安全地拼进文件名。
//
// 会话 ID 与 openid 都来自微信响应，正常情况下是字母数字，
// 但落盘前仍要挡住路径分隔符与 ..，否则一个被篡改的响应就能把文件写到目录之外。
func isSafeFileToken(token string) bool {
	if token == "" || token == "." || token == ".." {
		return false
	}
	if strings.ContainsAny(token, `/\`) || strings.Contains(token, "..") {
		return false
	}
	return true
}

// AuthDBPathFor 返回与管理端完全一致的认证数据库路径。
//
// CLI 的本地口令恢复必须打到同一个文件上，否则会出现「命令说重置成功、
// 服务端却仍用旧口令」的假成功。这里刻意不做 MkdirAll：
// 恢复操作不应该顺带创建目录，路径不存在就该直接报错。
func AuthDBPathFor(cfg Config) (string, error) {
	resourceRoot, dataRoot := cfg.ResourceRoot, cfg.DataRoot
	if resourceRoot == "" {
		resourceRoot = filepath.Join(".", "resource")
	}
	if dataRoot == "" {
		dataRoot = resourceRoot
	}
	filename := cfg.AuthDBFilename
	if filename == "" {
		filename = auth.DefaultAuthDBFilename
	}
	return auth.AuthDBPath(filepath.Join(dataRoot, "db"), filename), nil
}
