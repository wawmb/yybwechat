<p align="center">    
  <img src="https://img.shields.io/badge/Go-1.23-00ADD8?style=flat\\\&logo=go" alt="Go 1.23">    
  <img src="https://img.shields.io/badge/Docker-amd64-2496ED?style=flat\\\&logo=docker" alt="Docker amd64">    
  <img src="https://img.shields.io/badge/SQLite-纯_Go_驱动-003B57?style=flat\\\&logo=sqlite" alt="SQLite pure Go">    
  <img src="https://img.shields.io/badge/license-private-lightgrey" alt="private">    
</p>

<h1 align="center">yybwechat</h1>    
<p align="center">    
  <strong>自托管微信扫码登录服务</strong>  

纯 Go · MMTLS 直连 · 管理员认证 · 凭据保活 · 掉线通知 · 零外部依赖

</p>

---

## 目录

- [快速开始](#快速开始)
- [管理员初始化](#管理员初始化)
- [功能概览](#功能概览)
- [控制台导览](#控制台导览)
- [界面与主题](#界面与主题)
- [架构设计](#架构设计)
- [数据持久化](#数据持久化)
- [配置参数](#配置参数)
- [API 参考](#api-参考)
- [项目结构](#项目结构)
- [工作原理](#工作原理)
- [技术栈](#技术栈)
- [开发与测试](#开发与测试)
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

### 无人值守部署（预设管理员）

首次启动后需要创建管理员才能进入控制台。若希望容器启动即可用，通过环境变量预设：

```bash
docker run -d \
  --name yybwechat \
  --restart=always \
  -p 60006:8000 \
  -v /opt/yybwechat/data:/app/data \
  -e YYB_ADMIN_USER=admin \
  -e YYB_ADMIN_PASSWORD=your-strong-password \
  -e YYB_PROTOCOL_TOKEN=your-random-token \
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

## 管理员初始化

服务启用**单管理员认证**：系统只存在一个管理员账号，没有注册入口，也不做多用户隔离。

**首次启动**时访问任意页面会自动跳转到 `/setup`，填写用户名和密码即可创建（密码至少 8 位）。创建后自动登录，该页面此后不再显示。

三种初始化方式，任选其一：

| 方式                                           | 适用场景       |
| -------------------------------------------- | ---------- |
| 访问 `/setup` 页面                               | 常规部署，交互式创建 |
| 环境变量 `YYB_ADMIN_USER` / `YYB_ADMIN_PASSWORD` | 容器化、无人值守部署 |
| `POST /api/auth/setup` 接口                    | 脚本自动化      |

管理员凭据存放在**独立的 `auth.db`**（与业务库分离），密码使用 bcrypt 哈希存储，会话令牌只保存 SHA-256 摘要。

---

## 功能概览

| 能力         | 说明                                                             |
| ---------- | -------------------------------------------------------------- |
| 微信扫码登录     | Web 页面展示二维码，手机微信扫码即完成账号入库                                      |
| 多账号管理      | 支持同时维护多个微信账号，可设置备注、手动修正状态                                      |
| 管理员认证      | 单管理员账号 + 会话 Cookie + bcrypt，全站管理接口需登录                          |
| 登录态保持      | MMTLS 会话缓存 + 自动重连，账号存活状态实时检测                                   |
| 凭据保活       | 后台按凭据剩余有效期提前续期，失败自动退避重试                                        |
| 双轨生命周期展示   | 账号卡片同时显示「保活凭据」与「扫码 30 天窗口」两条进度条，最后 5 天提示重扫                    |
| wxapp 能力代理 | getCode / getPhoneNumber / operateWxData / getUserInfo / oauth |
| 协议令牌       | 可选的 Bearer 令牌，用于保护 `/wxapp/*`；留空则不校验，脚本零配置即可调用              |
| 掉线通知       | 账号状态变更时推送 Webhook，支持企业微信 markdown 卡片                           |
| 账号维护       | 备注管理、残缺账号清理、账号 ID 紧凑化                                          |
| OpenAPI 文档 | 内置 Swagger UI，可导出 JSON 导入 Postman                              |
| 账号级代理      | 每个账号可独立配置静态代理或动态提取代理，支持品赞 / 巨量供应商配置库与按省市选出口，代理页 `/proxies`     |
| 代理穿透       | 支持 SOCKS5 / HTTP CONNECT 代理，海外服务器也能用                           |
| 稳定设备标识     | 每个账号持久化唯一设备 MAC，避免微信风控                                         |
| 纯 Go 实现    | 无需微信客户端、无需外部数据库，一个二进制搞定                                        |
| 统一设计系统     | 四个页面共用一套 CSS 设计令牌，深浅色主题自动适配，无外部前端依赖                            |

---

## 控制台导览

### 状态三态

概览区顶部是账号计数与状态图例，三种状态含义见[账号状态取值](#账号管理)。「待确认」用琥珀色标记，它**不代表掉线**：服务保留着该账号的会话并在后台按递增退避重试，恢复后自动回到「可用」，期间不会推送告警。

### 双轨生命周期进度条

每张账号卡片下方是两条独立衰减的进度条：

- **保活凭据**：短效凭据的剩余时间（默认 2 小时量级），反映后台保活是否在正常续期；
- **扫码 30 天**：从扫码那一刻起算的 30 天授权窗口，剩余不足 5 天进度条转琥珀色，服务端会同时把 `rescan_recommended` 置为 true，卡片上出现「⚠ 建议重新扫码」。

两条曲线衰减速度差两个数量级，分开画才能同时看清「保活是否在跑」和「是否该重扫」。把鼠标悬停在卡片上，详情浮层里还有扫码时间与窗口剩余的精确数值。`expired` 账号显示「保活已停止 · 重新扫码后恢复」。

> **「扫码时间未记录」**：`refresh_token_observed_at` 是后来才加入的字段，在此之前的扫码不会留下时间戳。这类账号的 30 天窗口无法计算，卡片显示「未记录」，且不会触发「建议重新扫码」——在方便的时候重新扫码一次，时间戳就会开始记录。

### 调用面板按能力切换参数

「调用配置」里的参数区会随所选能力变化，避免填错字段：

| 能力                           | 需要的参数                                    |
| ---------------------------- | ---------------------------------------- |
| `getCode` / `getPhoneNumber` | 目标**小程序 AppID**                          |
| `operateWxData`              | 目标**小程序 AppID** + payload JSON           |
| `getUserInfo`                | 无需 AppID，选中账号即可（走 HTTPS 通道，不建立 MMTLS 会话） |
| `oauth`                      | **公众号 AppID** + 授权回调地址 + 作用域             |

选定能力后，下拉框下方会显示该能力的用途说明。`oauth` 生成链接成功后会自动复制到剪贴板——**code 必须由用户在微信内完成授权后由回调地址带回**，服务端不提供也不应伪造。

### 单账号操作

账号卡片右上角四个按钮：

| 按钮 | 作用                                     |
| -- | -------------------------------------- |
| ✎  | 设置/清除备注（纯本地字段，不参与微信侧同步）                |
| ⇄  | 手动修正状态为「可用」或「需重扫」。标记为需重扫时会同时清理该账号的登录会话 |
| 🌐 | 跳转到该账号的代理设置页 `/proxies?ref=<账号ID>`     |
| 🗑 | 删除账号                                   |

批量操作（刷新存活、同步资料、整理 ID、清理残留）在概览区右侧的快捷操作区。

---

## 界面与主题

### 单一设计基线

五个页面（控制台 `/`、代理设置 `/proxies`、扫码 `/scan`、登录 `/login`、初始化 `/setup`）共用一份 `resource/static/css/console.css`（约 330 行），页面各自的 `<style>` 只保留**布局与页面专属样式**。

收敛前四个模板各自内联一整套 token，合计约 680 行重复定义。收敛之后：

| 页面           | 内联 `<style>` 行数 |
| ------------ | --------------- |
| `index.html` | 225（控制台布局与卡片）   |
| `scan.html`  | 101（二维码与轮询区）    |
| `login.html` | 17（仅居中容器）       |
| `setup.html` | 24（仅居中容器）       |
| **合计**       | **367**         |

内联样式从约 680 行降到 367 行（净减约 313 行），多出来的 327 行是**只写一次**的共享基线。此前同样的 token 与组件在每个页面里各写一遍，且取值并不统一——控制台用 `--radius: 8px`、扫码页用 `10px`，同一个按钮在两个页面里圆角不一致。

收敛过程中还修掉一处悬空变量：账号卡片的 `.account-remark` 引用了 `--surface-2`，但这个变量在任何样式表里都没有定义过，深色主题下会渲染成透明。现在它由 `console.css` 在两套主题里统一声明，且已通过「引用集合 − 定义集合」的差集校验确认全仓库无其它悬空变量。

前端没有构建步骤、没有 npm 依赖、没有 CDN 外链，`console.css` 与模板一起由镜像提供。

### 设计令牌

令牌定义在 `:root`，深色主题通过 `[data-theme="dark"]` 覆盖**同名变量**——切换主题只改 `<html>` 上的 `data-theme` 属性，不重新加载样式表。

| 类别  | 变量                                                                                                                 |
| --- | ------------------------------------------------------------------------------------------------------------------ |
| 表面  | `--bg` `--bg-soft` `--surface` `--surface-2` `--surface-strong`                                                    |
| 文字  | `--ink`（正文） `--muted`（次要） `--faint`（弱化）                                                                            |
| 线条  | `--line` `--line-strong`                                                                                           |
| 语义色 | `--primary` / `-hover` / `-soft`、`--accent`(`-soft`)、`--success`(`-soft`)、`--warning`(`-soft`)、`--danger`(`-soft`) |
| 形状  | `--radius` `--radius-sm` `--radius-lg`                                                                             |
| 阴影  | `--shadow-sm` `--shadow-md` `--shadow-lg`                                                                          |
| 渐变  | `--grad-brand`（品牌标识） `--grad-panel`（概览区快捷操作面板）                                                                     |

颜色统一用 `oklch()` 表述，同一色相下浅色与深色主题的感知亮度保持一致。阴影只保留三级（卡片 / 浮起 / 弹窗），避免层层堆叠出过度立体的观感。

### 共用组件

`console.css` 提供以下跨页面组件，页面直接引用即可：

- **按钮** `.btn` — `primary` / `secondary` / `danger` / `ghost` / `loading` / `block`
- **表单** `.field` + `.input` / `.select` / `.textarea`，配套 `.field-hint`（能力说明气泡）
- **反馈** `.status-pill`、`.empty-state`、`.skeleton`（骨架屏 + shimmer）、`.toast`
- **浮层** `.modal-overlay` / `.modal`
- **其它** `.theme-btn`（主题开关）、`.mark` / `.brand`（品牌标识）

无障碍与动效约定：

- 所有可交互元素共用一套 `:focus-visible` 描边；输入框聚焦时额外加 `0 0 0 3px var(--accent-soft)` 光环
- 尊重系统的 `prefers-reduced-motion: reduce`，开启后所有过渡与动画压缩到 1ms

### 改动约定

1. 调整配色只改 `console.css` 里的 `:root` 与 `[data-theme="dark"]`，**不要在页面 `<style>` 里覆盖 token**
2. 新增变量必须**同时定义浅色与深色两套**，否则深色主题下会回退取到浅色值
3. 两个及以上页面会用到的组件放进 `console.css`；只有单页用到的布局留在页面自己的 `<style>`
4. 页面模板通过 `<link rel="stylesheet" href="/static/css/console.css">` 引入，不要内联复制

---

## 架构设计

```
┌───────────────────────────────────────────────────────┐
│                      浏览器 / 客户端                    │
│          Web 控制台 · Swagger · API 调用                │
└──────────────────────┬────────────────────────────────┘
                       │ HTTP :8000
┌──────────────────────▼────────────────────────────────┐
│                    yybwechat                          │
│  ┌────────────────────────────────────────────────┐   │
│  │              路由分组（安全边界）                │   │
│  │  公开组   /login /setup /api/auth/* /health     │   │
│  │  协议组   /wxapp/* /wx/oauth  ← 可选 Bearer 令牌│   │
│  │           GET /accounts       ← 只读·同受令牌门禁│   │
│  │  管理组   / /scan /accounts/refresh 等 ← 需登录 │   │
│  └────────────────────────────────────────────────┘   │
│  ┌──────────┐  ┌──────────┐  ┌────────────────────┐   │
│  │  httpapi │  │    qr    │  │      protocol      │   │
│  │  Gin 路由 │ │ OAuth 扫码│ │  MMTLS 直连微信服务器│   │
│  │  认证中间件│ │ 会话管理  │ │  ECDH+AES-GCM 加密  │   │
│  └────┬─────┘  └────┬─────┘ │  LongLink/ShortLink  │   │
│       │             │       │  NewDNS 域名解析      │   │
│  ┌────▼─────────────▼─────┐ │  SOCKS5/HTTP CONNECT │   │
│  │     store (SQLite)      │ └──────────┬───────────┘  │
│  │  账号 · 会话 · 设备标识   │             │              │
│  └─────────────────────────┘             │              │
│  ┌───────────────┐  ┌────────────────────▼──────────┐   │
│  │ auth (auth.db)│  │      后台 goroutine            │   │
│  │ 管理员 · 会话  │  │ 健康检查 · 凭据保活 · Webhook  │   │
│  └───────────────┘  └───────────────────────────────┘   │
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

**为什么认证库要独立成 auth.db？**  
业务库的 `settings` 表承载着 Webhook 等既有配置。认证相关的表独立成库，可以完全避免与业务表结构的迁移冲突，也让备份边界更清晰。

**为什么健康检查与凭据保活要并存？**  
两者依据不同：健康检查按「距上次检查的时长」兜底扫描，保证账号状态不会长期不更新；保活按「凭据剩余有效期」精确续期，在真正过期之前完成刷新。重复的刷新会被有效期判断自然过滤掉。

---

## 数据持久化

容器内目录分为两层：

| 路径               | 内容                    | 来源         |
| ---------------- | --------------------- | ---------- |
| `/app/resource/` | 模板、静态资源               | 镜像内置，随镜像更新 |
| `/app/data/`     | SQLite 数据库、头像缓存、二维码缓存 | 可挂载卷持久化    |

`-v` 挂载只需覆盖 `/app/data`，模板和前端文件永远不会被数据卷污染。

### 数据库文件

| 文件                | 内容               | 说明        |
| ----------------- | ---------------- | --------- |
| `data/db/yyb.db`  | 账号、MMTLS 会话、系统设置 | 业务主库      |
| `data/db/auth.db` | 管理员凭据、登录会话       | 认证库，与主库分离 |

### 三种持久化方式

| 方式      | 命令示例                          | 删容器数据还在？ | 适用场景         |
| ------- | ----------------------------- | :------: | ------------ |
| 默认（容器内） | 不加 `-v`                       |     ❌    | 测试、快速体验      |
| 具名卷     | `-v yybwechat-data:/app/data` |     ✅    | 生产、无需关心宿主机路径 |
| 宿主机目录   | `-v /opt/data:/app/data`      |     ✅    | 需要直接备份/迁移数据  |

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

> 恢复时请同时备份 `yyb.db` 与 `auth.db`，只恢复业务库会导致管理员需要重新初始化。

---

## 配置参数

### 命令行参数

| 参数                     | 默认值               | 环境变量                     | 说明                             |
| ---------------------- | ----------------- | ------------------------ | ------------------------------ |
| `--host`               | `0.0.0.0`         | `YYB_BIND_ADDRESS`       | HTTP 监听地址                      |
| `--port`               | `8000`            | `YYB_PORT`               | HTTP 监听端口                      |
| `--resource-root`      | `./resource`      | `YYB_RESOURCE_ROOT`      | 模板和静态资源根目录                     |
| `--data-root`          | (同 resource-root) | `YYB_DATA_ROOT`          | 持久化数据目录（db/avatars/qr）         |
| `--db`                 | `yyb.db`          | `YYB_DB`                 | 业务数据库文件名（位于 data-root/db 下）    |
| `--auth-db`            | `auth.db`         | `YYB_AUTH_DB`            | 认证数据库文件名（位于 data-root/db 下）    |
| `--tcp-proxy`          | (空)               | `YYB_TCP_PROXY`          | TCP 代理地址                       |
| `--check-interval`     | `1800`            | `YYB_CHECK_INTERVAL`     | 后台健康检查间隔（秒），设 0 禁用             |
| `--check-stale`        | `3600`            | `YYB_CHECK_STALE`        | 账号超过此秒数未检查则触发刷新                |
| `--keepalive-interval` | `60`              | `YYB_KEEPALIVE_INTERVAL` | 凭据保活扫描间隔（秒），设 0 禁用             |
| `--keepalive-ahead`    | `2700`            | `YYB_KEEPALIVE_AHEAD`    | 凭据剩余有效期低于此秒数时提前续期              |
| `--session-hours`      | `168`             | `YYB_SESSION_HOURS`      | 管理员会话有效期（小时）                   |
| `--protocol-token`     | (空)               | `YYB_PROTOCOL_TOKEN`     | `/wxapp/*`、`/wx/oauth` 与 `GET /accounts` 的 Bearer 令牌，空表示不校验 |
| `--cookie-secure`      | `false`           | `YYB_COOKIE_SECURE`      | 为会话 Cookie 加 Secure（HTTPS 时开启） |
| `--trust-proxy`        | `false`           | `YYB_TRUST_PROXY`        | 信任反向代理传来的 `X-Forwarded-For` / `X-Real-IP`（仅部署在可信代理后开启） |

> **优先级**：显式命令行参数 > 环境变量 > 默认值。
>
> **Docker 注意**：Dockerfile CMD 已将 `--resource-root` 设为 `/app/resource`、`--data-root` 设为 `/app/data`。Docker 部署时无需手动设置这些参数。

### 管理员预设环境变量

| 变量                                        | 说明              |
| ----------------------------------------- | --------------- |
| `YYB_ADMIN_USER` / `YYB_WEB_USER`         | 管理员用户名，启动时自动创建  |
| `YYB_ADMIN_PASSWORD` / `YYB_WEB_PASSWORD` | 管理员密码，需与用户名同时提供 |

### 后台健康检查

按「距上次检查的时长」兜底扫描所有账号——若某个账号超过 1 小时未被检查，自动刷新其存活状态。

**会跳过的账号**：没有凭据的、已判定为 `expired` 的（与凭据保活保持一致，彻底失效的账号不再尝试，只能重新扫码）、以及距上次检查还没超过 `--check-stale` 的。每轮扫描结束后还会清理 `sessions` 表中已过期的会话，避免这张表无限增长。

```bash
# 每 10 分钟检查一次，超过 30 分钟未检查的账号触发刷新
docker run -d --name yybwechat --restart=always -p 60006:8000 \
  wawmb/yybwechat:latest --check-interval 600 --check-stale 1800

# 完全禁用后台检查
docker run -d --name yybwechat --restart=always -p 60006:8000 \
  wawmb/yybwechat:latest --check-interval 0
```

### 凭据保活

按「凭据剩余有效期」精确续期：默认每 60 秒扫描一次，凭据剩余有效期不足 45 分钟时提前刷新。

判定逻辑是「剩余有效期大于 `--keepalive-ahead` 就跳过」，所以账号在三种情况下会被刷新：

| 剩余有效期      | 行为                                            |
| ---------- | --------------------------------------------- |
| > 45 分钟    | 跳过，不做任何请求                                     |
| ≤ 45 分钟    | 提前续期，成功后 `expires_at` 重置为「现在 + 7200 秒」，倒计时回到 2 小时 |
| 已过期（负数）    | **仍然尝试续期**，靠 refresh token 救回                  |

> **过期不等于账号报废。** 续期用的是 refresh token，它不检查 `expires_at`——只要 refresh token 在应用宝侧还没作废，即使 access token 已过期也能换出一对新令牌。服务停机几小时再启动也不会导致账号失效：启动后 60 秒内第一轮扫描就会发现并尝试续期。
>
> 真正会终止的只有一种情况：应用宝返回明确失效（如 `42007 refresh_token expired`）。此时状态置为 `expired`，凭据保活**与**健康检查都会跳过该账号，必须重新扫码。

```bash
# 每 5 分钟扫描，剩余有效期不足 30 分钟时续期
docker run -d --name yybwechat --restart=always -p 60006:8000 \
  wawmb/yybwechat:latest --keepalive-interval 300 --keepalive-ahead 1800

# 禁用保活（仅保留健康检查）
docker run -d --name yybwechat --restart=always -p 60006:8000 \
  wawmb/yybwechat:latest --keepalive-interval 0
```

**失败处理分三类**，既要避免把网络抖动误判成掉线，也要保证「真的续不上」一定通知到人：

| 判定                                            | 账号状态      | 后续行为                     | 推送 |
| --------------------------------------------- | --------- | ------------------------ | -- |
| 命中已知失效码（`42007` refresh token 失效、`40188` scope 无效） | `expired` | 停止自动续期，需重新扫码            | `status_changed` |
| 微信侧明确拒绝（业务码非 0）**且**缓存凭据已过期                    | `expired` | 同上。实测失效令牌常返回 `-109 RC_PARAMS_INVALID` 而非标准 42007 | `status_changed` |
| 其它刷新失败（网络、代理、超时）**且**缓存凭据已过期                   | `unknown` | 保留会话继续重试，网络/代理恢复后可自愈     | `renewal_failed` |
| 刷新失败但凭据尚未过期                                  | 维持原状态     | 保活还有时间窗口，继续尝试            | 不推送 |

关键在于第三行：**凭据一旦越过到期时刻又没能续上，账号此刻就是不可用的**，不管原因是账号失效还是代理配错，都必须告诉运营者。但这种情况不能直接判 `expired`——那是终态，会永久停止重试，一个临时性的代理故障就会把所有账号钉死。所以状态留在 `unknown` 继续自愈，同时单独推一条「无法续期」告警。

退避从 5 分钟起按 2 倍递增（5 → 10 → 20 → 40 分钟），封顶 1 小时；刷新成功后立即清零。只有收敛到终态（`alive` / `expired`）才会推送状态变更，`unknown` 不会制造告警噪音。

> **通知出口唯一**：手动刷新（`/accounts/refresh`）、wxapp 调用失败后的凭据补救、后台保活三条路径都收敛到同一个刷新实现，状态跃迁的通知在刷新内部发出。这修复了早期「后台先把账号置为 expired 就跳过、其它路径改状态却不推送」导致的掉线漏报。

### Webhook 掉线通知

服务会推送两类事件：

| 事件 | 触发条件 | 含义 |
| -- | -- | -- |
| `status_changed` | 状态收敛到终态（`alive` ↔ `expired`） | 账号掉线或恢复；`expired` 需重新扫码 |
| `renewal_failed` | 凭据已过期且本轮刷新失败，但原因不是明确失效 | 账号此刻**用不了**，但仍在重试自愈；先排查代理/网络 |

`unknown` 作为中间态本身不推送——除非它已经伴随「凭据过期 + 续不上」，此时走 `renewal_failed`。

**`status_changed` 示例：**

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

**`renewal_failed` 示例：**

```json
{
  "event": "renewal_failed",
  "timestamp": 1720153200,
  "account": { "id": 1, "openid": "oXXXX", "uin": 123456, "nickname": "张三" },
  "current": "unknown",
  "reason": "dial proxy failed: dial tcp 127.0.0.1:9: connect: connection refused"
}
```

`renewal_failed` 自带去重：同一故障周期只推一次，凭据续上后自动复位；若账号持续瘫痪，每 6 小时再提醒一次，避免响过一声就沉默。

**配置方式：**

- **控制台 UI**：在首页侧边栏「🔔 掉线通知」面板填入地址、保存、测试
- **API**：`PUT /settings/webhook` `{"url":"https://..."}`
- **读取**：`GET /settings/webhook` → `{"url":"..."}`

企业微信机器人地址会被自动识别，改用 `markdown_v2` 卡片格式推送。日志中的 Webhook URL 会自动脱敏 query string，避免把密钥写进日志。

**送达保证（不会漏报）：**

推送成功才记账；失败则保持旧标记不动，下一轮扫描时自动补推。三类事件各自记账：

| 事件 | 含义 | 记账列 | 重试方式 |
| -- | -- | -- | -- |
| `status_changed` | 状态跃迁（如 可用 → 需重扫） | `status_notified` | 每轮扫描比对 `status` 与 `status_notified`，不一致就重推 |
| `account_expired` | 账号**当前仍然是** `expired` | `expiry_alerted_at` | 不看跃迁，只看当前状态；超过 6 小时未重扫就再提醒一次 |
| `renewal_failed` | 凭据已过期但续不上（状态仍可自愈） | `expiry_alerted_at` | 未送达则不写；超过 6 小时仍未恢复会再次提醒 |

> **失效账号一定会被推到**：`expired` 是终态，保活与健康检查都会跳过它（刷不回来），手动刷新也不会产生新的状态跃迁。所以`account_expired` 刻意不依赖跃迁——只要账号还挂着 `expired`，每轮扫描都会检查并推送，6 小时重提醒一次，直到你重新扫码为止。三个事件共用 `expiry_alerted_at` 记账，因此同一条事实不会连发两份。

这一点很关键：账号一旦被判为 `expired`，保活与健康检查都会跳过它（终态，救不回来）。如果那次推送恰好失败又没人重试，状态已经落库、后续再没有任何路径会重算它，告警就永久丢失——运营者只会看到"账号莫名其妙不干活了"。补推让这件事变成"晚一点收到"而不是"永远收不到"。

| 场景 | 行为 |
| -- | -- |
| 推送成功 | 记账，同一状态不会重复打扰 |
| 推送失败（网络、Webhook 非 2xx、企业微信 errcode≠0） | 不记账，下一轮扫描补推；Webhook 恢复后自动送达 |
| 未配置 Webhook | 不推送也不补推，等运营者补齐地址后下一轮自动补发 |
| 服务重启 | 待补推信息已落库，重启后继续重试 |
| 升级旧版本 | 首次启动把现有状态回填为「已通知」，alive 账号不会补推刷屏 |
| 升级前账号已经失效 | 一次性修复迁移把这类账号重新标记为待推送：升级后第一轮扫描各补推**一次**「需重扫」告警（用 settings 标记保证不会随重启反复触发） |
| 账号持续瘫痪 | `renewal_failed` 每 6 小时重复提醒一次，直到续期成功或重新扫码 |
| 账号一直没重新扫码 | `account_expired` 每 6 小时重复提醒一次，直到重新扫码恢复 |
| 脚本调到失效账号 | `/wxapp/*` 命中 `expired` 时也会告警（同一 6 小时窗口去重） |
| 手动点「刷新存活」 | 失效账号在**发起刷新之前**先发一条 `account_expired`；刷新成功恢复则改发「已恢复可用」 |
| 手动刷新后状态变成 unknown | 网络失败不算证据，但失效告警照发（不能因为状态被降级就当成问题消失） |

> 补推没有次数上限，只要 Webhook 还挂着就一直重试（默认每 60 秒一次，开销极小）。这是刻意的取舍：掉线告警宁可不厌其烦，也不能悄悄消失。

### TCP 代理

当运行环境无法直连微信服务器（如海外、某些云环境），配置代理：

```bash
docker run -d --name yybwechat --restart=always -p 60006:8000 \
  wawmb/yybwechat:latest --tcp-proxy socks5://192.168.1.100:1080

# 也支持 HTTP CONNECT
docker run -d --name yybwechat --restart=always -p 60006:8000 \
  wawmb/yybwechat:latest --tcp-proxy http-connect://proxy.example.com:3128
```

`--tcp-proxy` 是**全局兜底**：没有单独配置代理的账号会使用它。

### 账号级代理与代理设置页

控制台顶栏的「代理设置」进入 `/proxies`，可以做两件事：

1. **代理配置库**：把品赞提取链接、巨量业务编号 + API Key 或一条静态代理地址存成可复用的配置。品赞支持按 6 位地区编码选省/市；巨量由服务端按账号省市动态签名。
2. **账号级出口**：为每个账号独立选择 直连 / 静态代理 / 自定义提取 API / 配置库，并可单独指定小程序调用的出口（跟随账号或直连）。

注意事项：

- **动态提取（品赞 / 巨量）是短效代理**，每次提取都会换出口，无法维持微信登录会话，系统会自动关闭这类账号的后台保活并给出提示；静态代理不受影响。
- 保存代理会**作废该账号已缓存的 MMTLS 会话**，下一次调用会重新登录。
- 「测试代理」会立即提取一次并探测实际出口 IP 与归属地；「按位置推荐地区」优先用浏览器定位，拿不到时退回 IP 归属地。
- 全局兜底 `--tcp-proxy` 与账号级代理同时存在时，账号级配置优先。

---

## API 参考

所有接口返回统一信封格式：

```json
{
  "code": 0,
  "msg": "success",
  "data": {}
}
```

| code | 含义                                                                                            |
| ---- | --------------------------------------------------------------------------------------------- |
| `0`  | 成功                                                                                            |
| 其他   | HTTP 状态码（400 参数错误 / 401 未登录 / 404 未找到 / 409 冲突 / 429 限流 / 502 网关错误 / 500 内部错误），`msg` 字段包含具体描述 |

### 认证说明

| 路由分组                                                                            | 认证方式                                     |
| ------------------------------------------------------------------------------- | ---------------------------------------- |
| `/health`、`/login`、`/setup`、`/api/auth/*`、`/openapi.json`、`/static/*`              | 公开                                       |
| `GET /accounts`（**只读**）                                                          | 免登录，但**受协议令牌保护**（配置后需带 Bearer）             |
| `/wxapp/*`、`/wx/oauth`                                                          | 可选 Bearer 令牌（配置 `YYB_PROTOCOL_TOKEN` 后启用）   |

> **令牌门禁也认管理员会话**：配置 `YYB_PROTOCOL_TOKEN` 后，上面两组接口同时接受「正确的 Bearer 令牌」或「已登录的管理员会话 Cookie」。因为控制台页面本身就要调 `/accounts`（账号列表）和 `/wxapp/*`（调用面板），而浏览器只会带 Cookie、不会自己加令牌；如果门禁只认令牌，一开令牌控制台就全线 401，前端又会把 401 当成会话过期踢回登录页。
| 其余全部路由（`/accounts/refresh`、`/accounts/resync`、`/accounts/status`、`/settings*`、`/qr*`、`/`、`/scan`、`/docs` 等） | 管理员会话 Cookie，或 `Authorization: Bearer <会话令牌>` |

未登录时，机器调用类路径（`/api/*`、`/accounts*`）返回 `401 JSON`，页面路径返回 `303` 跳转到 `/login` 或 `/setup`。

### 脚本如何调用：不需要任何凭据

自动化脚本（青龙面板定时任务等）的调用链只需要两步，且**全程免认证、不需要配置任何令牌**：

```python
import httpx

BASE = "http://192.168.85.235:60006"

# 第一步：拉取账号列表。GET /accounts 免登录；若服务端配置了 protocol-token，
# 记得带上 Authorization: Bearer <令牌>，未配置时裸请求即可。
accounts = httpx.get(f"{BASE}/accounts", timeout=10).json()["data"]
alive = [a for a in accounts if a.get("status") == "alive"]

# 第二步：用 ref 指定账号调用协议接口。/wxapp/* 默认不校验令牌。
r = httpx.post(f"{BASE}/wxapp/getCode", json={"ref": alive[0]["openid"], "app_id": "wx................"})
```

`GET /accounts` 返回的是**脱敏视图** `AccountPublic`，字段如下，不含 `login_buffer` 与 `credentials`：

| 字段                      | 类型      | 说明                             |
| ----------------------- | ------- | ------------------------------ |
| `id`                    | int     | 账号主键，可直接作为 `ref` 使用         |
| `openid`                | string  | 应用宝下发的 openid，也可作为 `ref` 使用  |
| `uin` / `alias`         | ?int / ?string | 微信 UIN / 别名，可能为空         |
| `nickname` / `avatar`   | ?string | 昵称 / 头像路径                     |
| `status`                | ?string | `alive` / `expired` / `unknown` |
| `device_id`             | ?string | 登录设备标识                        |
| `remark`                | ?string | 备注                            |
| `last_checked_at`       | ?int    | 上次健康检查时间戳                     |
| `credential_expires_at` | int     | 凭据到期时间戳，0 表示未知                |
| `created_at` / `updated_at` | int | 创建 / 更新时间戳                    |

> **为什么 `GET /accounts` 免登录？** 它是脚本调用链的第一步，用来探测服务是否存活并挑选在线账号。如果要求脚本先做交互式登录，无人值守的定时任务就直接断链了。放开的是**只读且脱敏**的那一个方法：`DELETE /accounts` 与其它写操作仍在处理器内部二次鉴权，只认管理员会话，不接受任何静态令牌。
>
> 但免登录 ≠ 不需要任何防护。它和 `/wxapp/*` 一样挂在**协议令牌门禁**下：一旦配置了 `--protocol-token`，脚本就要在请求头带上 `Authorization: Bearer <令牌>`，否则返回 401。这样在把服务暴露到公网时，至少有一道统一的口令挡在「拉账号列表」和「消费一次性 code」前面。

`ref` 支持三种标识符，服务端会依次尝试匹配：账号 `id`、`openid`、`nickname`。

### 系统接口

| 方法    | 路径                 | 说明                          |
| ----- | ------------------ | --------------------------- |
| `GET` | `/`                | Web 控制台首页（需登录）              |
| `GET` | `/health`          | 健康检查                        |
| `GET` | `/docs/index.html` | Swagger UI 接口文档（需登录）        |
| `GET` | `/openapi.json`    | OpenAPI 3.0 规范（可导入 Postman） |
| `GET` | `/api/version`     | 服务构建元数据（需登录）                |

### 管理员认证

| 方法     | 路径                   | 说明                                                           |
| ------ | -------------------- | ------------------------------------------------------------ |
| `GET`  | `/setup`             | 初始化页面（仅当无管理员时可用）                                             |
| `POST` | `/api/auth/setup`    | 创建管理员 `{"username":"...","password":"..."}`，密码至少 8 位         |
| `GET`  | `/login`             | 登录页面                                                         |
| `POST` | `/api/auth/login`    | 登录并下发会话 Cookie                                               |
| `POST` | `/api/auth/logout`   | 注销当前会话                                                       |
| `GET`  | `/api/auth/status`   | 认证状态（公开）                                                     |
| `GET`  | `/api/auth/me`       | 当前管理员信息                                                      |
| `PUT`  | `/api/auth/password` | 修改密码 `{"old_password":"...","new_password":"..."}`，成功后其它设备下线 |

> 同一 IP 连续登录失败 8 次后，将在 15 分钟内被限流（返回 429）。

### 扫码登录

| 方法     | 路径                         | 说明                                               |
| ------ | -------------------------- | ------------------------------------------------ |
| `GET`  | `/scan`                    | 扫码页面（HTML，需登录）                                   |
| `POST` | `/qr`                      | 创建扫码会话 → 返回二维码（支持 `?as_base64=true` 返回 data URI） |
| `GET`  | `/qr/{session_id}/image`   | 获取二维码图片                                          |
| `GET`  | `/qr/{session_id}/poll`    | 轮询扫码状态                                           |
| `POST` | `/qr/{session_id}/confirm` | 确认授权，账号入库                                        |

**扫码流程：**

```
POST /qr → 展示二维码 → 微信扫码 → 轮询 /qr/{id}/poll → 用户确认 → POST /qr/{id}/confirm → 入库完成
```

微信报告授权成功到 login buffer 真正可读之间存在短暂竞态，`/confirm` 内置了 8 秒窗口的退避重试来吸收这段延迟，不会再把瞬时未就绪当成失败抛给用户。

### 账号管理

| 方法       | 路径                           | 说明                                               |
| -------- | ---------------------------- | ------------------------------------------------ |
| `GET`    | `/accounts`                  | 获取全部账号列表                                         |
| `DELETE` | `/accounts?ref={ref}`        | 删除指定账号                                           |
| `GET`    | `/accounts/avatar?ref={ref}` | 获取账号头像                                           |
| `POST`   | `/accounts/refresh`          | 刷新账号登录态（`{"ref":"..."}`，不传 ref 则批量刷新全部）          |
| `POST`   | `/accounts/resync`           | 重同步账号信息（`{"ref":"..."}`，不传 ref 则批量同步全部）          |
| `POST`   | `/accounts/remark`           | 设置/清除备注 `{"ref":"...","remark":"..."}`（最多 128 字） |
| `POST`   | `/accounts/status`           | 手动设置状态 `{"ref":"...","status":"alive\|expired"}` |
| `POST`   | `/accounts/repair`           | 清理残缺账号，`{"confirm":false}` 预览、`true` 执行          |
| `POST`   | `/accounts/compact`          | 账号 ID 重排为 1..N，`{"confirm":false}` 预览、`true` 执行  |


> `{ref}` 支持账号 ID（纯数字）、UIN（纯数字）或 openid。

> ⚠️ **整理 ID 后**，脚本中硬编码的账号 ID（如 `@1`、`@2`）需要同步调整。

**账号状态取值：**

| 状态        | 含义                        | 控制台显示 |
| --------- | ------------------------- | ----- |
| `alive`   | 凭据有效，可正常调用 wxapp 接口       | 可用    |
| `expired` | 凭据已彻底失效，必须重新扫码            | 需重扫   |
| `unknown` | 本轮无法判定（网络抖动、超时、尚无凭据），等待重试 | 待确认   |

**账号列表额外字段**（`GET /accounts` 返回的 `AccountPublic`）：

| 字段                          | 说明                                                    |
| --------------------------- | ----------------------------------------------------- |
| `credential_expires_at`     | 凭据到期时间戳（Unix 秒），反映保活续期效果                              |
| `credential_expires_in`     | 本次凭据发放时的有效期秒数（剩余比例的分母）                                |
| `refresh_token_observed_at` | refresh token 首次被观察到的时间戳，用于推算 30 天扫码授权窗口；仅扫码成功那一刻写入   |
| `rescan_recommended`        | 扫码窗口进入最后 5 天（或缓存凭据已越过到期时刻且状态不明）时为 true，控制台据此显示「建议重新扫码」 |

### 代理管理

| 方法       | 路径                                  | 说明                                        |
| -------- | ----------------------------------- | ----------------------------------------- |
| `GET`    | `/accounts/proxy?ref={ref}`         | 读取账号代理配置（含 `keepalive_supported` 与 `proxy_warning`） |
| `PUT`    | `/accounts/proxy`                   | 保存账号代理配置，并作废该账号已缓存的 MMTLS 会话              |
| `POST`   | `/accounts/proxy/test`              | 立即提取一次代理并探测出口 IP 与归属地（只传 ref 表示测已保存配置）    |
| `GET`    | `/api/proxy-profiles`               | 列出代理供应商配置                                 |
| `POST`   | `/api/proxy-profiles`               | 新增供应商配置（品赞提取链接 / 巨量业务编号 / 静态代理地址）         |
| `PUT`    | `/api/proxy-profiles/{id}`          | 更新供应商配置                                   |
| `DELETE` | `/api/proxy-profiles/{id}`          | 删除供应商配置，引用它的账号退回直连                        |
| `GET`    | `/api/proxy-profiles/areas/provinces` | 品赞省份地区编码列表                                |
| `GET`    | `/api/proxy-profiles/areas/cities?province={code}` | 品赞城市地区编码列表（6 位省份编码）        |
| `POST`   | `/api/proxy-location/recommend`     | 按浏览器经纬度（缺省退回 IP 归属地）推荐代理省市编码              |

**保存账号代理（`PUT /accounts/proxy`）请求体：**

```json
{
  "ref": "1",
  "mode": "direct | static | api",
  "protocol_proxy_mode": "account | direct",
  "proxy_type": "http | socks5",
  "static_proxy": "user:pass@1.2.3.4:8080",
  "api_url": "https://供应商提取接口",
  "provider_profile_id": 1,
  "region_code": "440300",
  "region_province": "广东省",
  "region_city": "深圳市",
  "refresh_ahead_minutes": 15
}
```

引用配置库时只需 `ref` + `provider_profile_id` + 地区字段，`mode` 会被服务端按配置类型改写（静态预设 → `static`，动态供应商 → `api`）。响应中 `lifetime_class` 为 `short` 且 `keepalive_supported` 为 `false` 时表示动态短效代理，后台保活已自动关闭。

### wxapp 能力调用

| 方法           | 路径                      | 说明                               |
| ------------ | ----------------------- | -------------------------------- |
| `POST`       | `/wxapp/getCode`        | 获取小程序登录 code                     |
| `POST`       | `/wxapp/getPhoneNumber` | 获取手机号                            |
| `POST`       | `/wxapp/operateWxData`  | 调用 operateWxData 接口（微信小程序数据操作）   |
| `GET`/`POST` | `/wxapp/getUserInfo`    | 获取微信用户信息（走 HTTPS 通道，无需 MMTLS 会话） |
| `POST`       | `/wxapp/oauth`          | 构造公众号网页授权 URL                    |
| `POST`       | `/wx/oauth`             | 同上，早期脚本使用的兼容别名（行为完全一致）           |

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

| 字段        |  必填 | 说明                                                |
| --------- | :-: | ------------------------------------------------- |
| `ref`     |  ✅  | 账号 ID、UIN 或 openid                                |
| `app_id`  |  ✅  | 目标小程序 AppID                                       |
| `payload` |  ❌  | `operateWxData` 接口需要，其他 wxapp 接口忽略。具体结构取决于小程序 API |

**公众号授权（`/wxapp/oauth`）：**

```json
{
  "app_id": "wx141bfb9b73c970a9",
  "redirect_uri": "https://example.com/callback",
  "scope": "snsapi_base",
  "state": "optional-state"
}
```

返回 `url` 与 `full_url`（带 `#wechat_redirect`）。**该接口只生成授权链接**，code 必须由用户在微信客户端内完成授权后由回调地址携带返回，服务端无法也不应伪造。

`app_id` 也可以用旧字段名 `appid` 传（两者等价，同时存在时以 `app_id` 为准）。传了 `ref` 时响应会回填 `account_id` 与 `openid`。

因为 code 必须等用户操作，响应里 `code` 恒为 `null`，并附三个提示字段，方便脚本判断「这一步该等回调，而不是重试」：

| 字段 | 值 |
| --- | --- |
| `authorization_required` | `true` — 必须由用户在微信内完成授权 |
| `callback_code_source` | `redirect_uri` — code 只会出现在回调参数里 |
| `next_step` / `callback_params` | 自然语言说明；回调参数固定为 `["code","state"]` |

### 系统设置

| 方法     | 路径                       | 说明                                                   |
| ------ | ------------------------ | ---------------------------------------------------- |
| `GET`  | `/settings/webhook`      | 读取当前 Webhook 地址                                      |
| `PUT`  | `/settings/webhook`      | 设置 Webhook 地址 `{"url":"https://..."}`                |
| `POST` | `/settings/webhook/test` | 测试 Webhook 推送 `{"url":"https://..."}`（服务端代理，避免 CORS） |

---

## 项目结构

```
yybwechat/
├── cmd/yybwechat/main.go       # 程序入口、CLI/环境变量、优雅关闭
├── internal/
│   ├── httpapi/                # HTTP 层
│   │   ├── app.go              # 路由分组、配置、请求处理
│   │   ├── auth.go             # 管理员认证中间件与接口
│   │   ├── wxapp_extra.go      # getUserInfo / oauth 扩展接口
│   │   ├── qr_confirm.go       # 扫码确认的凭据重试
│   │   ├── keepalive.go        # 凭据保活后台任务（刷新/状态判定/通知唯一出口）
│   │   ├── account_maintenance.go # 备注/清理/整理/状态
│   │   ├── account_proxy.go    # 账号级代理读写/测试/出口探测
│   │   ├── proxy_profiles.go   # 代理供应商配置库（品赞/巨量/静态）
│   │   ├── proxy_location.go   # 按位置推荐代理地区
│   │   ├── openapi.go          # OpenAPI 规范生成
│   │   ├── fallback_html.go    # 内嵌后备 HTML
│   │   ├── resources.go        # 资源目录管理
│   │   ├── dbfile.go           # 数据库路径
│   │   ├── app_test.go         # 路由分组、认证门禁、脚本只读契约测试
│   │   └── keepalive_test.go   # 失效判定与退避策略测试
│   ├── auth/                   # 管理员认证（独立 auth.db）
│   │   ├── store.go            # bcrypt 口令、会话令牌（只存 SHA-256 摘要）
│   │   └── store_test.go       # 认证与会话测试（7 项）
│   ├── oauth/                  # 公众号网页授权 URL 构造
│   │   ├── authorize.go
│   │   └── authorize_test.go   # 授权 URL 与入参校验测试（5 项）
│   ├── version/                # 构建元数据
│   │   ├── version.go
│   │   └── version_test.go     # 版本/提交号回退测试（3 项）
│   ├── protocol/               # 协议层
│   │   ├── pool.go             # MMTLS 连接池（含 wx.login code 串行锁）
│   │   ├── mmtls_client.go     # 客户端握手
│   │   ├── mmtls_crypto.go     # AES-GCM / ECDH / HKDF
│   │   ├── loginbuffer.go      # 凭据获取与刷新（含结构化错误）
│   │   ├── ilink.go            # login_buffer 解析与握手请求构造
│   │   ├── shortlink.go        # 0-RTT 短连接
│   │   ├── newdns.go           # DNS 解析缓存
│   │   ├── bytes.go            # protobuf varint / LZ4
│   │   ├── transport.go        # TCP / SOCKS5 / HTTP CONNECT 代理
│   │   ├── bytes_test.go       # 编解码测试（9 项）
│   │   ├── mmtls_crypto_test.go # 加密层测试（11 项）
│   │   ├── ilink_test.go       # login_buffer 与设备标识测试（6 项）
│   │   ├── newdns_test.go      # DNS 纯函数测试（3 项）
│   │   └── ...
│   ├── proxysource/            # 代理提取与响应解析（txt/json/json2）
│   │   └── proxy.go
│   ├── qr/                     # OAuth 扫码流程
│   │   ├── qr.go               # 含代理客户端与二维码图片校验
│   │   └── qr_test.go          # 状态解析与凭据提取测试（9 项）
│   └── store/                  # 持久化层
│       ├── store.go            # 账号、会话、设置（含双轨生命周期推导）
│       ├── proxy.go            # 账号代理设置
│       ├── proxy_profiles.go   # 代理供应商配置
│       ├── maintenance.go      # 备注、清理、ID 紧凑化
│       └── store_test.go       # 持久化层测试
├── resource/
│   ├── templates/              # HTML 模板（各自只留布局样式）
│   │   ├── index.html          # Web 控制台（含双轨生命周期进度条）
│   │   ├── proxies.html        # 代理设置页
│   │   ├── scan.html           # 扫码页面
│   │   ├── login.html          # 登录页面
│   │   └── setup.html          # 管理员初始化页面
│   ├── static/
│   │   ├── css/console.css     # 统一设计系统：token + 共用组件（五页共用）
│   │   ├── js/                 # 预留前端脚本目录
│   │   └── favicon.png
│   ├── db/                     # 数据库目录（运行时生成 yyb.db / auth.db）
│   ├── avatars/                # 头像缓存
│   └── qr/                     # 二维码缓存
├── .github/workflows/
│   └── docker-image.yml        # GitHub Actions 自动构建并推送镜像
├── Dockerfile                  # 多阶段构建（AMD64）
├── .dockerignore               # 构建上下文排除清单
├── .gitignore                  # 含 data/ 与 *.db，防止数据库误提交
├── go.mod / go.sum             # Go 模块
└── README.md
```

> `resource/db`、`resource/avatars`、`resource/qr`、`resource/static/js`、`resource/static/css` 由 `.gitkeep` 占位，实际运行时数据写在 `data-root`（容器内为 `/app/data`）。
>
> `Dockerfile` 会把整个 `resource/templates/` 与 `resource/static/` 拷进镜像，因此 `console.css` 随镜像更新，不会被数据卷覆盖——**挂载 `-v` 时只需覆盖 `/app/data`**。

---

## 工作原理

### 凭据：扫码拿到的是什么

扫码成功后入库的**不是微信登录态**，而是应用宝登录通道下发的一组令牌。它们以 JSON 形式存放在 `wechat_accounts.credentials` 列：

| 字段                         | 含义                             |
| -------------------------- | ------------------------------ |
| `openid`                   | 该微信账号在应用宝通道下的唯一标识              |
| `accesstoken`              | 访问令牌，用来换取 login_buffer         |
| `refreshtoken`             | 刷新令牌，用来在过期前换一对新令牌              |
| `expires_at` / `expires_in` | 过期时间戳与有效期，默认 7200 秒（2 小时）      |
| `refresh_token_observed_at` | refresh token 首次被观察到的时间戳（Unix 秒），**只在扫码成功那一刻写入**，保活刷新不改动它 |
| `refresh_refreshed_at`      | 最近一次凭据续期的时间戳，用于在缺少 `expires_at` 时兜底推算到期时刻 |
| `nickname` / `logintype`   | 昵称与登录类型（默认 `WX`），仅用于展示         |

> **这组令牌不是微信开放平台 OAuth 签发的。** 它们来自应用宝的登录通道（`yybadaccess.3g.qq.com`，业务 ID `pc_yyb_auth`）：扫码时微信授权给的是应用宝，应用宝再把令牌下发给本服务。因此凭据的续期与失效规则取决于应用宝侧，与微信开放平台的 access_token 不是同一套体系。

凭据有**两条生命周期**，控制台分开显示：

- **保活凭据**（`expires_at` / `expires_in`，2 小时量级）：后台保活在到期前自动续期，反映保活是否正常；
- **扫码授权窗口**（`refresh_token_observed_at` 起算 30 天）：refresh token 的授权窗口推算值，进入最后 5 天时控制台提示重新扫码。

**凭据本身不能直接调微信接口**，它是两级兑换链的第一环：

```
凭据 --(1) 应用宝 HTTPS --> login_buffer --(2) MMTLS ManualAuth 握手 --> 会话密钥 --(3) 0-RTT --> 接口调用
```

| 环节    | 实现                                                                        | 说明                        |
| ----- | ------------------------------------------------------------------------- | ------------------------- |
| (1)   | `FetchLoginBuffer`                                                        | 用 access token 换取一段 login_buffer |
| (2)   | `parseLoginBuffer` → `buildManualAuthRequest` → `sendApp(cmdManualAuth)`   | 用 login_buffer 与微信完成登录握手  |
| (3)   | 0-RTT ShortLink                                                           | 握手得到的会话被缓存复用，后续请求不再重复握手  |

唯一的旁路是 `getUserInfo`：它直接用 access token 请求应用宝的 `/pc_yyb/pcyyb_get_user_info`，不换 login_buffer、不建立 MMTLS 会话——这就是控制台上选中它时不需要填 AppID 的原因。

### 扫码登录流程

```
  用户浏览器              yybwechat                微信服务器
      │                      │                         │
      │  GET /scan           │                         │
      │─────────────────────►│                         │
      │                      │  POST OAuth 获取 UUID    │
      │                      │───────────────────────► │
      │                      │  ◄── UUID + 二维码图片   │
      │  渲染二维码           │                         │
      │◄─────────────────────│                         │
      │                      │                         │
      │  (用户微信扫码确认)     │  GET 长轮询状态         │
      │                      │───────────────────────► │
      │                      │  ◄── 授权码 + 重定向      │
      │                      │                         │
      │                      │  GET 回调 → Cookie 凭证   │
      │                      │───────────────────────► │
      │                      │  ◄── openid + ticket     │
      │                      │                         │
      │                      │  请求 login_buffer       │
      │                      │  （失败时短窗口重试）      │
      │                      │───────────────────────► │
      │                      │  ◄── login_buffer        │
      │                      │                         │
      │  扫码成功             │  SQLite 存储账号         │
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

每个微信账号入库时，服务自动生成一个**稳定的设备 MAC 地址**，持久化在 SQLite 的 `wechat_accounts.device_id` 列。

- **格式**：`XX-XX-XX-XX-XX-XX` 六段十六进制。**首字节是随机的**，只保证两个位——最低位为 0（单播）、次低位为 1（本地管理），因此首字节可能落在 `02`、`4A`、`B2`……直到 `FE` 之间的任意偶数，并非固定 `02` 开头
- **生成时机**：账号首次发起小程序请求（如 `GetCode`、`GetPhoneNumber`、`OperateWXData`）时，通过 `EnsureAccountDeviceID()` 在会话层触发；该账号此前未生成过 device_id 时才会真正写入
- **生命周期**：与账号绑定，跨重启不变化
- **作用**：MMTLS 登录握手时将设备 MAC 嵌入认证请求，避免因设备信息频繁变动触发微信风控

---

## 技术栈

| 层面      | 选择                           | 原因                               |
| ------- | ---------------------------- | -------------------------------- |
| HTTP 框架 | Gin v1.10                    | 高性能、轻量、中间件生态                     |
| 数据库     | SQLite (modernc.org/sqlite)  | **纯 Go SQLite 驱动**，运行时无 C 依赖，零运维 |
| 口令哈希    | bcrypt (golang.org/x/crypto) | cost 12，抗 GPU 暴力破解               |
| 微信协议    | 自实现 MMTLS                    | 无需微信客户端，完全掌控会话生命周期               |
| 加密      | ECDH P-256 + AES-128-GCM     | 与微信客户端密码套件兼容                     |
| 编解码     | Protobuf varint / LZ4        | 微信协议数据格式                         |
| API 文档  | Swagger (OpenAPI 3.0)        | 可视化交互、可导入 Postman                |
| 控制台前端   | 原生 HTML/CSS/JS + 统一设计令牌      | 零构建步骤、零 npm 依赖、零 CDN 外链，随镜像发布    |
| 容器化     | 多阶段 Docker 构建                | 镜像精简 (~25MB)、非 root 运行           |

---

## 开发与测试

### 本地运行

```bash
go run ./cmd/yybwechat \
  --resource-root ./resource \
  --data-root ./data
```

默认会在 `./data/db` 下生成 `yyb.db` 与 `auth.db`。这两个文件含敏感数据，已被 `.gitignore` 排除，**请勿提交到仓库**。

### 单元测试

```bash
go test ./...
```

共 **63 个测试函数**（含 31 个表驱动子用例，合计 94 个用例），一次 `go test ./...` 全部跑完，无需网络。

| 包                  | 测试数 | 覆盖内容                                                                                                          |
| ------------------ | :-: | ------------------------------------------------------------------------------------------------------------- |
| `internal/protocol` | 29  | varint/protobuf 往返、LZ4 往返与截断拒绝、**ECDH 双方共享密钥一致**、AES-GCM 往返与「密文/AAD/序列号/密钥」四类篡改拒绝、nonce 构造、AAD 字段布局、HKDF 确定性、login_buffer 解析、NewDNS 目标筛选 |
| `internal/qr`      |  9  | 扫码状态正则提取（8 类响应）、二维码图片校验（HTML 错误页/截断 PNG）、data URI MIME 降级、Cookie 凭据提取与非法 `expires_in` 回退、随机串唯一性       |
| `internal/httpapi` |  9  | 路由分组与 Swagger、管理员认证门禁、协议 Bearer 令牌、凭据失效判定（9 子用例）、状态变更告警判定、退避递增策略、**脚本只读契约**（匿名 `GET /accounts` 必须 200 且不泄露 `login_buffer`/`credentials`，写操作 `DELETE` 仍 401） |
| `internal/auth`    |  7  | 建号/认证/改密、重复与弱口令拒绝、会话生命周期、只下线其它会话、过期会话清理、认证库路径、**数据库不得出现明文会话令牌**                                               |
| `internal/oauth`   |  5  | 授权 URL 构造、state 随机性、12 类非法入参（含回调带 userinfo 的钓鱼风险、相对路径、fragment）、第三方平台 AppID                                   |
| `internal/version` |  3  | 版本号/commit/构建时间非空、commit 截断为 7 位、空版本回退                                                                        |
| `internal/store`   |  1  | 数据库 schema 迁移（旧 `wmpf_sessions` → `sessions`）                                                                 |

两条最关键的断言：

- **`internal/protocol` 的 ECDH 对称性**：双方各自用「自己的私钥 + 对方公钥」计算共享密钥，结果必须完全相同。一旦不对称，MMTLS 握手必然失败，而失败点会远离真正的根因——这是整个协议栈正确性的基石。
- **`internal/auth` 的明文令牌断言**：创建会话后直接读取 `auth.db` 文件字节，若其中出现明文令牌则测试失败——确保库里只落 SHA-256 摘要。
- **`internal/httpapi` 的脚本只读契约**：写入一条带哨兵值（`SUPER-SECRET-...`）的账号后，用完全匿名的裸请求访问 `GET /accounts`，断言状态码为 200、脚本依赖的 `id`/`openid`/`nickname`/`status` 字段存在，且响应体里搜不到任何哨兵值。这条断言同时锁住两件事：脚本的调用链不会被认证打断，公开视图也不会漏出敏感字段。

> 测试全部使用纯函数与内存构造，**不发起任何真实网络请求**，因此可以在 CI 里稳定跑。

### 前端验证方式

模板里的内联 JS 不便用浏览器自动化逐条回归，仓库内采用两段轻量校验：

```bash
# 1) 语法检查：抽出 <script> 内容后交给 Node 解析
node --check <抽取出的 .js>

# 2) 行为检查：用最小 DOM stub + node:vm 执行内联脚本，断言关键函数输出
```

真实二进制冒烟覆盖：`/static/css/console.css` 以 `text/css` 返回、四个页面均引入共享样式、未登录时页面路径返回 `303` 引导到 `/login` 或 `/setup`、`/accounts` 字段完整。

### 构建元数据

`/api/auth/status` 与 `GET /api/version` 都会返回版本号、Git 提交号与构建时间。正式发布时用 `-ldflags` 注入：

```bash
go build -trimpath -ldflags "-s -w \
  -X yybwechat/internal/version.Version=1.0.0" \
  ./cmd/yybwechat/
```

未注入时保留源码默认值，并回退读取 Go 内嵌的 VCS 信息（`vcs.revision` / `vcs.time`），因此从源码目录直接构建也能识别出提交号。

---

## 安全建议

### 网络安全

- **不要在公网直接暴露服务**。yybwechat 设计为内网/本地使用，建议通过 VPN 或 SSH 隧道访问
- 如果必须公网访问，务必在前面加一层反向代理（Nginx / Caddy）并配置 HTTPS + 基础认证
- 公网部署时必须设置 `YYB_COOKIE_SECURE=true` 和随机的 `YYB_PROTOCOL_TOKEN`
- `GET /accounts` 免登录，会暴露账号的 `openid`、昵称、头像与状态；它不含 `login_buffer` 与 `credentials`，但公网暴露仍会泄露「有哪些账号在线」这一信息，建议只在内网开放。它与 `/wxapp/*` 共用**同一道协议令牌门禁**，配置 `YYB_PROTOCOL_TOKEN` 后同样需要 Bearer
- `/wxapp/*` 与 `/wx/oauth` 默认不校验令牌（保证内网脚本易用性）。配置 `YYB_PROTOCOL_TOKEN` 后需携带 `Authorization: Bearer <token>`
- 启动日志会在「未配置令牌且监听非回环地址」时打印两条 `[security]` 提示，部署时不要忽略
- **`--trust-proxy` 默认关闭**：`X-Forwarded-For` 在直连场景下可任意伪造，默认信任会让攻击者轮换假 IP 绕过登录失败限流（每 IP 8 次）。只有在反向代理确实会覆盖该头时才开启；开启后请确认代理会**丢弃客户端自带的** `X-Forwarded-For`


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
        # 注意用 $remote_addr 而不是 $proxy_add_x_forwarded_for：
        # 服务端取 X-Forwarded-For 的第一个地址，而 $proxy_add_x_forwarded_for
        # 会把客户端自带的头原样保留在前面，等于让来访者自己决定日志里的 IP。
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

> 这份配置只有配合 `--trust-proxy` 才会生效；不开启时服务端一律使用 TCP 连接的对端地址。

### 数据安全

- `yyb.db` 包含 `login_buffer` 和 `credentials`（openid / access_token / refresh_token），这是敏感数据
- `auth.db` 包含管理员口令哈希与会话令牌摘要
- 挂载宿主机目录时，确保目录权限仅对运行用户可读（容器内已是 `appuser`）
- 备份数据时注意加密或存放在安全位置，并同时备份两个数据库文件
- Webhook URL 在日志输出时会自动脱敏 query string（企业微信的 `?key=xxx` 就是完整凭证）

### 认证安全

- 会话令牌为 32 字节随机数，数据库只保存 SHA-256 摘要，数据库泄露也无法直接冒用会话
- 会话 Cookie 设置 `HttpOnly` + `SameSite=Lax`，防止脚本读取与跨站携带
- 修改密码后其它设备的会话会被自动下线
- 登录失败按 IP 限流，防止暴力破解

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

### 登录与认证

**Q: 打开页面一直跳转到 `/setup`**

说明认证库中还没有管理员账号。访问 `/setup` 创建，或用 `YYB_ADMIN_USER` / `YYB_ADMIN_PASSWORD` 环境变量预设后重启。

**Q: 脚本调用 `/accounts` 返回 401 Unauthorized**

`GET /accounts` 已经是**免认证**接口，正常情况下匿名裸请求就返回 200。如果仍然出现 401，按下面顺序排查：

| 现象                                       | 原因                          | 处理                                          |
| ---------------------------------------- | --------------------------- | ------------------------------------------- |
| `GET /accounts` 401                       | 运行的还是旧版本二进制/镜像            | 重新构建镜像或替换二进制后重启；`GET /accounts` 免认证是本次修复的内容 |
| `GET /accounts` 正常，但 `/accounts/refresh` 401 | 该接口属于管理组，本就要求登录            | 脚本不需要它：存活状态由服务端后台凭据保活与健康检查自动维护           |
| `DELETE /accounts` 401                    | 写操作要求管理员登录（设计如此）           | 在控制台操作，或先 `POST /api/auth/login` 拿会话 Cookie |
| 所有接口都 401                                | 配置了 `YYB_PROTOCOL_TOKEN`，脚本没带 | 要么脚本带上 `Authorization: Bearer <token>`，要么去掉该环境变量重启 |

脚本侧**不需要**任何令牌，也不需要把管理员密码硬编码进去换会话。完整的调用示例见[脚本如何调用：不需要任何凭据](#脚本如何调用不需要任何凭据)。

**Q: 忘记管理员密码**

无法在线找回（系统不存明文密码，也没有邮箱验证）。处理方式：

```bash
# 删除认证库后重新初始化（不影响已扫码的账号数据）
docker exec yybwechat rm -f /app/data/db/auth.db
docker restart yybwechat
```

**Q: 登录一直提示失败，后来返回 429**

同一 IP 连续失败 8 次会限流 15 分钟。可重启服务清空限流表，或等待窗口过期。

### 容器运行异常

**Q: 无法连接微信服务器**

1. 确认容器能出站到 `*.weixin.qq.com` 的 80/443/8080 端口
2. 如果在内网环境，配置 `--tcp-proxy`
3. 检查 DNS 解析：`docker exec yybwechat nslookup aedns.weixin.qq.com`

**Q: 账号突然全部失效**

通常是 login_buffer 过期导致。login_buffer 由应用宝 API 签发，有效期有限。可尝试：

1. 重新扫码添加账号
2. 确认凭据保活已启用（默认启用，检查日志中的 `[keepalive]` 行）
3. 检查 `data/db/yyb.db` 文件大小是否正常（异常小可能数据库损坏）

**Q: 账号显示「待确认」但还能正常调用接口**

「待确认」对应状态 `unknown`，表示最近一次刷新没能判定成功或失败，常见于网络抖动、超时或代理不稳定。此时服务**不会**清理 MMTLS 会话，账号通常仍可正常调用；后台会按递增退避自动重试，恢复后自动回到「可用」，期间不推送告警。若长时间停留在此状态，检查容器到微信服务器的连通性（见上一条）。

### 扫码登录失败

**Q: 二维码刷不出来**

检查日志：`docker logs yybwechat`，常见原因：

- 微信 OAuth 接口返回异常（可能是 IP 被临时限制）
- 代理配置错误

服务会对二维码响应做图片校验，若被代理拦截会明确提示「不是有效图片」，而不是展示一张坏图。

**Q: 扫码后一直不确认**

确认使用的是**手机微信扫一扫**，不是其他扫码工具。手机端需要点击"确认登录"。

---

## 常见问题

**Q: 为什么用 SQLite？**  
单用户 / 小团队场景下，SQLite 零运维、零配置，一个文件就是完整数据库。备份只需复制 .db 文件。如果后续需要多实例部署，可以迁移到 PostgreSQL。

**Q: 需要微信客户端吗？**  
不需要。服务通过自实现的 MMTLS 协议直连微信服务器，不依赖微信客户端或 WeChat SDK。

**Q: 可以多人使用吗？**  
不可以。系统设计为**单管理员**模式：只有一个管理员账号，没有注册入口，也不做多用户与账号归属隔离。如需多人协作，请共享管理员凭据或在前置反向代理层做访问控制。

**Q: 支持 ARM（Apple Silicon / 树莓派）吗？**  
当前 Dockerfile 仅构建 amd64。如需 ARM，修改 `GOARCH` 为 `arm64` 重新构建。

**Q: 能同时登录多个微信号吗？**  
可以。扫码页面支持反复添加，所有账号在控制台统一管理，互不干扰。

**Q: 数据会丢吗？**  
取决于挂卷方式：

- 不挂卷：容器不删数据不丢；删容器会丢
- 挂宿主机目录 / 具名卷：随便删容器，数据永久保留

**Q: 脚本调用 `/wxapp/*` 会被登录拦截吗？**  
不会。`/wxapp/*` 属于协议组，不要求浏览器登录，脚本裸请求即可调用。只有在你主动配置了 `YYB_PROTOCOL_TOKEN` 时才需要在请求头带上 `Authorization: Bearer <token>`——**这是可选项，不是必选项**。

**Q: 想改控制台配色 / 换 logo / 调圆角怎么办？**  
改 `resource/static/css/console.css` 即可：配色与圆角都在 `:root` 与 `[data-theme="dark"]` 的令牌里，四个页面同时生效，无需逐页修改。改完重新构建镜像（模板和 CSS 由镜像提供，不在数据卷里）。详见[界面与主题](#界面与主题)。

**Q: 支持 Windows / macOS 直接运行吗？**  
项目主要面向 Docker 部署。如需本地开发调试，安装 Go 1.23+ 后直接 `go run ./cmd/yybwechat` 即可。

**Q: 整理账号 ID 后脚本报错怎么办？**  
`/accounts/compact` 会重排账号主键，脚本中硬编码的账号 ID（如 `@3`）需要同步改为新的 ID。接口执行后会返回 old→new 的映射表，可据此批量替换。预防起见，建议脚本使用 openid 而非数字 ID 作为引用。
