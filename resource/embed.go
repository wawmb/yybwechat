// Package resource 把控制台的静态资源与 HTML 模板编译进二进制。
//
// 用途：独立二进制（不用 Docker、没有挂载 resource 目录）启动时，
// 服务端能自己把这套文件释放到运行时目录，不必再外带一个 resource 目录。
// 显式指定 --resource-root 的部署仍然用自己的文件，内嵌内容只作兜底——
// 见 httpapi.ensureResources 的 preferEmbedded 分支。
package resource

import "embed"

// WebAssets 是控制台运行所需的静态文件与 HTML 模板。
//
//go:embed static templates
var WebAssets embed.FS
