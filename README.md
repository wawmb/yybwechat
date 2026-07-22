<p align="center">
  <img src="https://img.shields.io/badge/Go-1.23-00ADD8?style=flat&logo=go" alt="Go 1.23">
  <img src="https://img.shields.io/badge/Docker-amd64-2496ED?style=flat&logo=docker" alt="Docker amd64">
  <img src="https://img.shields.io/badge/SQLite-纯_Go_驱动-003B57?style=flat&logo=sqlite" alt="SQLite pure Go">
  <img src="https://img.shields.io/badge/license-private-lightgrey" alt="private">
</p>

<h1 align="center">yybwechat</h1>
<p align="center">
  <strong>自托管微信扫码登录服务</strong><br>
  纯 Go · MMTLS 直连 · Docker 一键部署 · 零外部依赖
</p>

---

## 目录

- [快速开始](#快速开始)
- [功能概览](#功能概览)
- [架构设计](#架构设计)
- [数据持久化](#数据持久化)
- [配置参数](#配置参数)
- [API 参考](#api-参考)
- [项目结构](#项目结构)
- [工作原理](#工作原理)
- [技术栈](#技术栈)
- [安全建议](#安全建议)
- [故障排查](#故障排查)
- [常见问题](#常见问题)

---

## 快速开始

### 前置条件

- Docker 20.10+

### 构建镜像

```bash
cd yybwechat

# 推荐：指定平台 + 纯净构建
docker buildx build --platform linux/amd64 \
  -t wawmb/yybwechat:latest \
  --no-cache --progress=plain .
```

### 启动服务

```bash
# 最简启动 — 数据内置在容器中，重启不丢
docker run -d \
  --name yybwechat \
  --restart=always \
  -p 60006:8000 \
  wawmb/yybwechat:latest

# 生产环境 — 挂载宿主机目录持久化
docker run -d \
  --name yybwechat \
  --restart=always \
  -p 60006:8000 \
  -v /opt/yybwechat/data:/app/data \
  wawmb/yybwechat:latest
```

### 更新镜像

```bash
# 1. 停止并删除旧容器
docker rm -f yybwechat

# 2. 重新构建
docker buildx build --platform linux/amd64 -t wawmb/yybwechat:latest --no-cache --progress=plain .

# 3. 重新启动
docker run -d --name yybwechat --restart=always -p 60006:8000 wawmb/yybwechat:latest
```

> 如果使用 `-v` 挂载了宿主机目录，更新镜像后只需 `docker rm -f yybwechat` 再 `docker run` 即可，模板/静态文件由镜像提供，不会受旧数据卷影响。

启动后访问 `http://localhost:60006` 进入 Web 控制台。

---

## 功能概览

| 能力 | 说明 |
|------|------|
| 微信扫码登录 | Web 页面展示二维码，手机微信扫码即完成账号入库 |
| 多账号管理 | 支持同时维护多个微信账号，分别调用不同小程序 |
| 登录态保持 | MMTLS 会话缓存 + 自动重连，账号存活状态实时检测 |
| wxapp 能力代理 | getCode / getPhoneNumber / operateWxData 等接口直接调用 |
| OpenAPI 文档 | 内置 Swagger UI，可导出 JSON 导入 Postman |
| 代理穿透 | 支持 SOCKS5 / HTTP CONNECT 代理，海外服务器也能用 |
| 稳定设备标识 | 每个账号持久化唯一设备 MAC，避免微信风控 |
| 纯 Go 实现 | 无需微信客户端、无需外部数据库，一个二进制搞定 |

---

## 架构设计

```
┌───────────────────────────────────────────────────────┐
│                      浏览器 / 客户端                   │
│               Web 控制台 · API 调用 · Swagger          │
└──────────────────────┬────────────────────────────────┘
                       │ HTTP :8000
┌──────────────────────▼────────────────────────────────┐
│                    yybwechat                          │
│  ┌──────────┐  ┌──────────┐  ┌────────────────────┐   │
│  │  httpapi │  │    qr    │  │      protocol      │   │
│  │  Gin 路由 │ │ OAuth 扫码│ │  MMTLS 直连微信服务器│  │
│  │  中间件   │ │ 会话管理  │  │  ECDH+AES-GCM 加密 │   │
│  └────┬─────┘  └────┬─────┘  │  LongLink/ShortLink    │
│       │             │        │  NewDNS 域名解析    │   │
│       └──────┬──────┘        │  SOCKS5/HTTP CONNECT │ │
│              │               └──────────┬───────────┘ │
│  ┌──────────▼──────────────────────────┐              │
│  │              store                  │              │
│  │    SQLite · 账号 · 会话 · 设备标识   │              │
│  └─────────────────────────────────────┘              │
└──────────────────────┬────────────────────────────────┘
                       │ MMTLS (TCP :443/:8080)
┌──────────────────────▼────────────────────────────────┐
│                  微信服务器集群                        │
│     LongLink (登录握手) · ShortLink (0-RTT 请求)       │
│     OAuth 扫码 · login_buffer 签发                     │
└───────────────────────────────────────────────────────┘
```

### 关键设计决策

**为什么用 MMTLS 而不是微信 SDK？**
微信 SDK 需要客户端环境、有复杂的 token 刷新逻辑，且不支持多账号。自实现 MMTLS 协议栈可以直接与微信服务器通信，完全掌控会话生命周期。

**为什么 0-RTT ShortLink？**
LongLink 握手需要多次往返（~600ms），频繁调用时延迟累积。ShortLink 使用 PSK 票据实现 0-RTT，后续请求可直接发送加密数据，延迟降低到 ~200ms。

---

## 数据持久化

容器内目录分为两层：

| 路径 | 内容 | 来源 |
|------|------|------|
| `/app/resource/` | 模板、静态资源 | 镜像内置，随镜像更新 |
| `/app/data/` | SQLite 数据库、头像缓存、二维码缓存 | 可挂载卷持久化 |

`-v` 挂载只需覆盖 `/app/data`，模板和前端文件永远不会被数据卷污染。

### 三种持久化方式

| 方式 | 命令示例 | 删容器数据还在？ | 适用场景 |
|------|----------|:---:|------|
| 默认（容器内） | 不加 `-v` | ❌ | 测试、快速体验 |
| 具名卷 | `-v yybwechat-data:/app/data` | ✅ | 生产、无需关心宿主机路径 |
| 宿主机目录 | `-v /opt/data:/app/data` | ✅ | 需要直接备份/迁移数据 |

### 推荐配置

```bash
# 创建具名卷（只需一次）
docker volume create yybwechat-data

# 启动容器
docker run -d \
  --name yybwechat \
  --restart=always \
  -p 60006:8000 \
  -v yybwechat-data:/app/data \
  wawmb/yybwechat:latest
```

### 备份与恢复

数据只需要备份 `/app/data`，模板由镜像提供无需备份。

```bash
# 备份
docker cp yybwechat:/app/data ./backup-yybwechat

# 恢复
docker cp ./backup-yybwechat/. yybwechat:/app/data/
```

---

## 配置参数

### 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--host` | `0.0.0.0` | HTTP 监听地址 |
| `--port` | `8000` | HTTP 监听端口 |
| `--resource-root` | `./resource` | 模板和静态资源根目录 |
| `--data-root` | (同 resource-root) | 持久化数据目录（db/avatars/qr） |
| `--db` | `yyb.db` | SQLite 数据库文件名（位于 data-root/db/ 下） |
| `--tcp-proxy` | (空) | TCP 代理地址 |
| `--check-interval` | `1800` | 后台健康检查间隔（秒），设 0 禁用 |
| `--check-stale` | `3600` | 账号超过此秒数未检查则触发刷新 |

> **Docker 注意**：Dockerfile CMD 已将 `--resource-root` 设为 `/app/resource`、`--data-root` 设为 `/app/data`。Docker 部署时无需手动设置这些参数。

### 后台健康检查

yybwechat 内置了健康检查 goroutine，默认每 30 分钟扫描一次所有账号——若某个账号超过 1 小时未被检查，自动刷新其存活状态。

```bash
# 每 10 分钟检查一次，超过 30 分钟未检查的账号触发刷新
docker run -d \
  --name yybwechat \
  --restart=always \
  -p 60006:8000 \
  wawmb/yybwechat:latest \
  --check-interval 600 --check-stale 1800

# 完全禁用后台检查（账号状态不再自动更新，需手动调用 /accounts/refresh）
docker run -d \
  --name yybwechat \
  --restart=always \
  -p 60006:8000 \
  wawmb/yybwechat:latest \
  --check-interval 0
```

### Webhook 掉线通知

当健康检查发现账号状态发生变化（alive→expired 或 recovered→alive），可自动向配置的 Webhook 地址推送 JSON 通知：

```json
{
  "event": "status_changed",
  "timestamp": 1720153200,
  "account": {
    "id": 1,
    "openid": "oXXXX",
    "uin": 123456,
    "nickname": "张三"
  },
  "previous": "alive",
  "current": "expired"
}
```

**配置方式：**

- **控制台 UI**：在首页侧边栏「🔔 掉线通知」面板填入地址、保存、测试
- **API**：`PUT /settings/webhook` `{"url":"https://..."}`
- **读取**：`GET /settings/webhook` → `{"url":"..."}`

### TCP 代理

当运行环境无法直连微信服务器（如海外、某些云环境），配置代理：

```bash
docker run -d \
  --name yybwechat \
  --restart=always \
  -p 60006:8000 \
  wawmb/yybwechat:latest \
  --tcp-proxy socks5://192.168.1.100:1080

# 也支持 HTTP CONNECT
docker run -d \
  --name yybwechat \
  --restart=always \
  -p 60006:8000 \
  wawmb/yybwechat:latest \
  --tcp-proxy http-connect://proxy.example.com:3128
```

> 代理失败时会自动回退直连，不会导致服务中断。

## API 参考

所有接口返回统一信封格式：

```json
{
  "code": 0,
  "msg": "success",
  "data": {}
}
```

| code | 含义 |
|------|------|
| `0` | 成功 |
| 其他 | HTTP 状态码（400 参数错误 / 404 未找到 / 409 冲突 / 502 网关错误 / 500 内部错误），`msg` 字段包含具体描述 |

### 系统接口

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/` | Web 控制台首页 |
| `GET` | `/health` | 健康检查 |
| `GET` | `/docs/index.html` | Swagger UI 接口文档 |
| `GET` | `/openapi.json` | OpenAPI 3.0 规范（可导入 Postman） |

### 系统设置

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/settings/webhook` | 读取当前 Webhook 地址 |
| `PUT` | `/settings/webhook` | 设置 Webhook 地址 `{"url":"https://..."}` |
| `POST` | `/settings/webhook/test` | 测试 Webhook 推送 `{"url":"https://..."}`（服务端代理，避免 CORS） |

### 扫码登录

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/scan` | 扫码页面（HTML） |
| `POST` | `/qr` | 创建扫码会话 → 返回二维码（支持 `?as_base64=true` 返回 data URI） |
| `GET` | `/qr/{session_id}/image` | 获取二维码图片 |
| `GET` | `/qr/{session_id}/poll` | 轮询扫码状态 |
| `POST` | `/qr/{session_id}/confirm` | 确认授权，账号入库 |

**扫码流程：**
```
POST /qr → 展示二维码 → 微信扫码 → 轮询 /qr/{id}/poll → 用户确认 → POST /qr/{id}/confirm → 入库完成
```

### 账号管理

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/accounts` | 获取全部账号列表 |
| `DELETE` | `/accounts?ref={ref}` | 删除指定账号 |
| `GET` | `/accounts/avatar?ref={ref}` | 获取账号头像 |
| `POST` | `/accounts/refresh` | 刷新账号登录态（JSON body `{"ref":"..."}`，不传 ref 则批量刷新全部） |
| `POST` | `/accounts/resync` | 重同步账号信息（JSON body `{"ref":"..."}`，不传 ref 则批量同步全部） |

> `{ref}` 支持账号 ID（纯数字）、UIN（纯数字）或 openid。refresh/resync 的 ref 通过请求 body 传入。

### wxapp 能力调用

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/wxapp/getCode` | 获取小程序登录 code |
| `POST` | `/wxapp/getPhoneNumber` | 获取手机号 |
| `POST` | `/wxapp/operateWxData` | 调用 operateWxData 接口（微信小程序数据操作） |

**请求体：**

```json
{
  "ref": "oXXXX-abc123",
  "app_id": "wx141bfb9b73c970a9",
  "payload": {
    "api_name": "getUserInfo",
    "data": {},
    "env": 1
  }
}
```

| 字段 | 必填 | 说明 |
|------|:--:|------|
| `ref` | ✅ | 账号 ID、UIN 或 openid |
| `app_id` | ✅ | 目标小程序 AppID |
| `payload` | ❌ | `operateWxData` 接口需要，其他 wxapp 接口忽略。具体结构取决于小程序 API |

---

## 项目结构

```
yybwechat/
├── cmd/yybwechat/main.go       # 程序入口、CLI、优雅关闭
├── internal/
│   ├── httpapi/                # HTTP 层
│   │   ├── app.go              # 路由注册、请求处理
│   │   ├── openapi.go          # OpenAPI 规范生成
│   │   ├── fallback_html.go    # 内嵌后备 HTML
│   │   ├── resources.go        # 资源目录管理
│   │   └── dbfile.go           # 数据库路径
│   ├── protocol/               # 协议层
│   │   ├── pool.go             # MMTLS 连接池
│   │   ├── mmtls_client.go     # 客户端握手
│   │   ├── mmtls_crypto.go     # AES-GCM / ECDH
│   │   ├── mmtls_protocol.go   # 协议常量
│   │   ├── loginbuffer.go      # 凭证获取刷新
│   │   ├── ilink.go            # iLink 通道
│   │   ├── shortlink.go        # 0-RTT 短连接
│   │   ├── newdns.go           # DNS 解析缓存
│   │   ├── transport.go        # TCP / 代理
│   │   └── bytes.go            # 编解码工具
│   ├── qr/qr.go                # OAuth 扫码流程
│   └── store/store.go          # SQLite 持久化
├── resource/templates/         # HTML 模板
│   ├── index.html              # Web 控制台
│   └── scan.html               # 扫码页面
├── Dockerfile                  # 多阶段构建
├── go.mod / go.sum             # Go 模块
└── README.md
```

---

## 工作原理

### 扫码登录流程

```
  用户浏览器              yybwechat                微信服务器
      │                      │                         │
      │  GET /scan           │                         │
      │─────────────────────►│                         │
      │                      │  POST OAuth 获取 UUID   │
      │                      │───────────────────────► │
      │                      │  ◄── UUID + 二维码图片   │
      │  渲染二维码           │                         │
      │◄─────────────────────│                         │
      │                      │                         │
      │  (用户微信扫码确认)     │  GET 长轮询状态        │
      │                      │───────────────────────► │
      │                      │  ◄── 授权码 + 重定向     │
      │                      │                         │
      │                      │  GET 回调 → Cookie 凭证  │
      │                      │───────────────────────► │
      │                      │  ◄── openid + ticket    │
      │                      │                         │
      │                      │  请求 login_buffer      │
      │                      │───────────────────────► │
      │                      │  ◄── login_buffer       │
      │                      │                         │
      │  扫码成功             │  SQLite 存储账号        │
      │◄─────────────────────│                         │
```

### MMTLS 连接与 API 调用

```
  yybwechat                               微信服务器
      │                                        │
      │  NewDNS: 解析 LongLink IP              │
      │──────────────────────────────────────► │
      │  ◄── IP:Port 列表                      │
      │                                        │
      │  TCP 连接 → ClientHello (ECDH g₁,g₂)   │
      │──────────────────────────────────────► │
      │  ◄── ServerHello + 证书 + 加密扩展      │
      │                                        │
      │  ManualAuth (login_buffer 签名)        │
      │──────────────────────────────────────► │
      │  ◄── 会话密钥 + PSK 票据                │
      │                                        │
      │  后续请求：0-RTT ShortLink              │
      │  PSK ClientHello + 加密业务数据         │
      │──────────────────────────────────────► │
      │  ◄── 加密响应                           │
```

### 设备标识策略

每个微信账号入库时，服务自动生成一个**稳定的设备 MAC 地址**（格式 `02-XX-XX-XX-XX-XX`），持久化在 SQLite 的 `wechat_accounts.device_id` 列。

- **生成时机**：账号首次发起小程序请求（如 `GetCode`、`GetPhoneNumber`、`OperateWXData`）时，通过 `EnsureAccountDeviceID()` 在会话层触发；该账号此前未生成过 device_id 时才会真正写入
- **生命周期**：与账号绑定，跨重启不变化
- **作用**：MMTLS 登录握手时将设备 MAC 嵌入认证请求，避免因设备信息频繁变动触发微信风控

---

## 技术栈

| 层面 | 选择 | 原因 |
|------|------|------|
| HTTP 框架 | Gin v1.10 | 高性能、轻量、中间件生态 |
| 数据库 | SQLite (modernc.org/sqlite) | **纯 Go SQLite 驱动**，运行时无 C 依赖，零运维 |
| 微信协议 | 自实现 MMTLS | 无需微信客户端，完全掌控会话生命周期 |
| 加密 | ECDH P-256 + AES-128-GCM | 与微信客户端密码套件兼容 |
| 编解码 | Protobuf varint / LZ4 | 微信协议数据格式 |
| API 文档 | Swagger (OpenAPI 3.0) | 可视化交互、可导入 Postman |
| 容器化 | 多阶段 Docker 构建 | 镜像精简 (~25MB)、非 root 运行 |

---

## 安全建议

### 网络安全

- **不要在公网直接暴露服务**。yybwechat 设计为内网/本地使用，建议通过 VPN 或 SSH 隧道访问
- 如果必须公网访问，务必在前面加一层反向代理（Nginx / Caddy）并配置 HTTPS + 基础认证

### 反向代理示例（Nginx）

```nginx
server {
    listen 443 ssl;
    server_name yyb.internal;

    ssl_certificate     /etc/ssl/yyb.crt;
    ssl_certificate_key /etc/ssl/yyb.key;

    auth_basic "yybwechat";
    auth_basic_user_file /etc/nginx/.htpasswd;

    location / {
        proxy_pass http://127.0.0.1:8000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}
```

### 数据安全

- SQLite 数据库包含 `login_buffer` 和 `credentials`（openid / access_token / refresh_token），这是敏感数据
- 挂载宿主机目录时，确保目录权限仅对运行用户可读（容器内已是 `appuser`）
- 备份数据时注意加密或存放在安全位置

---

## 故障排查

### 镜像构建失败

**Q: `go mod download` 超时**

```bash
# 确认 Dockerfile 中已设置国内代理
ENV GOPROXY=https://goproxy.cn,direct
```

**Q: `cgo: C compiler "gcc" not found`**

yybwechat 使用纯 Go SQLite 驱动（modernc.org/sqlite），构建时 `CGO_ENABLED=0`，不需要 C 编译器。如果遇到此错误，请确认 Dockerfile 未被修改。

### 容器运行异常

**Q: 无法连接微信服务器**

1. 确认容器能出站到 `*.weixin.qq.com` 的 80/443/8080 端口
2. 如果在内网环境，配置 `--tcp-proxy`
3. 检查 DNS 解析：`docker exec yybwechat nslookup aedns.weixin.qq.com`

**Q: 账号突然全部失效**

通常是 login_buffer 过期导致。login_buffer 由应用宝 API 签发，有效期有限。可尝试：
1. 重新扫码添加账号
2. 检查 `resource/db/yyb.db` 文件大小是否正常（异常小可能数据库损坏）

### 扫码登录失败

**Q: 二维码刷不出来**

检查日志：`docker logs yybwechat`，常见原因：
- 微信 OAuth 接口返回异常（可能是 IP 被临时限制）
- 代理配置错误

**Q: 扫码后一直不确认**

确认使用的是**手机微信扫一扫**，不是其他扫码工具。手机端需要点击"确认登录"。

---

## 常见问题

**Q: 为什么用 SQLite？**
单用户 / 小团队场景下，SQLite 零运维、零配置，一个文件就是完整数据库。备份只需复制 .db 文件。如果后续需要多实例部署，可以迁移到 PostgreSQL。

**Q: 需要微信客户端吗？**
不需要。服务通过自实现的 MMTLS 协议直连微信服务器，不依赖微信客户端或 WeChat SDK。

**Q: 支持 ARM（Apple Silicon / 树莓派）吗？**
当前 Dockerfile 仅构建 amd64。如需 ARM，修改 `GOARCH` 为 `arm64` 重新构建。

**Q: 能同时登录多个微信号吗？**
可以。扫码页面支持反复添加，所有账号在控制台统一管理，互不干扰。

**Q: 数据会丢吗？**
取决于挂卷方式：
- 不挂卷：容器不删数据不丢；删容器会丢
- 挂宿主机目录 / 具名卷：随便删容器，数据永久保留

**Q: 支持 Windows / macOS 直接运行吗？**
项目主要面向 Docker 部署。如需本地开发调试，安装 Go 1.23+ 后直接 `go run ./cmd/yybwechat` 即可。
