// Package httpapi 提供 yybwechat 服务的 HTTP API 层实现。
// 包含路由注册、请求处理、资源管理、OpenAPI 规范等。
package httpapi

import (
	"os"
	"path/filepath"
)

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
func ensureResources(resourceRoot, dataRoot string) (resources, error) {
	res := resources{
		DB:        filepath.Join(dataRoot, "db"),
		Avatars:   filepath.Join(dataRoot, "avatars"),
		QR:        filepath.Join(dataRoot, "qr"),
		Templates: filepath.Join(resourceRoot, "templates"),
		Static:    filepath.Join(resourceRoot, "static"),
	}
	for _, p := range []string{res.DB, res.Avatars, res.QR, res.Templates, res.Static} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return res, err
		}
	}
	return res, nil
}

// avatarPath 根据 openid 生成头像文件的本地存储路径。
func (r resources) avatarPath(openid string) string {
	return filepath.Join(r.Avatars, safeName(openid)+".jpg")
}

// qrPath 根据会话 ID 生成二维码图片的本地存储路径。
func (r resources) qrPath(sessionID string) string {
	return filepath.Join(r.QR, sessionID+".jpg")
}
