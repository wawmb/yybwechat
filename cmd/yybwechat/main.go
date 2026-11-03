// Package main 是 yybwechat 服务的启动入口。
// yybwechat 是一个微信扫码登录服务，提供扫码登录、账号管理、
// wxapp 协议接口代理调用、凭据保活以及掉线 Webhook 通知等功能。
//
// 运行方式：
//
//	yybwechat --host 0.0.0.0 --port 8000 \
//	          --resource-root ./resource \
//	          --data-root ./data \
//	          --tcp-proxy socks5://proxy:1080
//
// 大部分参数都可以通过环境变量覆盖，便于容器化部署。
// 优先级：显式命令行参数 > 环境变量 > 默认值。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"yybwechat/internal/httpapi"
	"yybwechat/internal/protocol"
)

// envOr 读取环境变量，不存在时返回默认值。
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// envInt 读取整型环境变量，解析失败时返回默认值。
func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("[config] 环境变量 %s 的值 %q 不是合法整数，使用默认值 %d", key, v, def)
		return def
	}
	return n
}

// defaultCodeMinGap 是同一账号两次上游请求的最小间隔。
//
// 这是个保守起点，不是测出来的安全值：唯一一次「仅靠成功取码就把授权打到
// 失效」的现场大约是每秒 1.6 个请求。熔断只对失败反应、登录预算只对 OAuth
// 调用反应，所以**一串成功的 code 能同时穿过两者**——间隔是唯一拦得住它的东西。
const defaultCodeMinGap = 3 * time.Second

// envDuration 读取 Go 时长（如 "3s" / "10m"）。
//
// "off" 与 "0" 都表示关闭该功能；空值或解析失败时保留默认值并明说一句，
// 不静默换成一个用户没预期的数。
func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	if strings.EqualFold(v, "off") {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		log.Printf("[config] 环境变量 %s 的值 %q 不是合法时长，使用默认值 %s", key, v, def)
		return def
	}
	return d
}

// envBool 读取布尔环境变量，仅 "true"/"1"/"yes"/"on" 视为真。
func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	switch v {
	case "true", "1", "yes", "on", "TRUE", "True":
		return true
	case "false", "0", "no", "off", "FALSE", "False":
		return false
	}
	return def
}

// envWithFallback 依次尝试多个环境变量名，返回第一个有值的。
// 用于兼容同一配置的不同历史名称。
func envWithFallback(keys ...string) string {
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			return v
		}
	}
	return ""
}

// main 是程序入口，负责解析命令行参数与环境变量、初始化 App 实例、
// 启动 HTTP 服务器，并监听系统信号以实现优雅关闭。
func main() {
	// .env 必须在任何 flag 注册之前加载：本程序所有 flag 的默认值都取自环境变量，
	// 注册之后再读文件，文件里的配置就对默认值无能为力了。
	explicitEnvFile := prescanEnvFile(os.Args[1:])
	if path := resolveEnvFile(explicitEnvFile); path != "" {
		if err := loadEnvFile(path, os.Stderr); err != nil {
			// 显式指定却读不了要报错退出；自动发现失败只提示，不阻止启动
			if explicitEnvFile != "" {
				fmt.Fprintln(os.Stderr, "加载 .env 失败："+err.Error())
				os.Exit(1)
			}
			fmt.Fprintln(os.Stderr, "提示："+err.Error())
		}
	} else if explicitEnvFile != "" {
		fmt.Fprintln(os.Stderr, "加载 .env 失败：指定的文件不存在："+explicitEnvFile)
		os.Exit(1)
	}

	// 解析命令行参数；默认值优先取环境变量，保证容器场景零参数启动
	host := flag.String("host", envOr("YYB_BIND_ADDRESS", "0.0.0.0"), "监听地址")
	port := flag.Int("port", envInt("YYB_PORT", 8000), "监听端口")
	resourceRoot := flag.String("resource-root",
		envOr("YYB_RESOURCE_ROOT", filepath.Join(".", "resource")), "模板和静态资源目录")
	dataRoot := flag.String("data-root", envOr("YYB_DATA_ROOT", ""),
		"持久化数据目录（db/avatars/qr/auth），默认同 resource-root")
	dbFilename := flag.String("db", envOr("YYB_DB", httpapi.DefaultDBFilename),
		"SQLite 数据库文件名，位于 data-root/db 下")
	authDBFilename := flag.String("auth-db", envOr("YYB_AUTH_DB", "auth.db"),
		"管理员认证数据库文件名，位于 data-root/db 下")
	tcpProxy := flag.String("tcp-proxy", envOr("YYB_TCP_PROXY", ""),
		"可选 TCP 代理: socks5://host:port 或 http-connect://host:port")
	// 默认值对齐官方客户端：每 2 分钟检查一次、提前 4 分钟续期。
	// 提前量从 45 分钟收到 4 分钟是有意的——续期越贴近真实到期点，
	// 单位时间内发起的刷新次数越少，被上游当成异常行为的概率也越低。
	checkInterval := flag.Int("check-interval", envInt("YYB_CHECK_INTERVAL", 0),
		"后台健康检查间隔(秒)，默认 0 禁用；保活已覆盖到期检查，除非需要额外兜底再打开")
	checkStale := flag.Int("check-stale", envInt("YYB_CHECK_STALE", 3600),
		"账号未检查超过此时长(秒)视为过期并触发刷新（仅 check-interval>0 时生效）")
	keepAliveInterval := flag.Int("keepalive-interval", envInt("YYB_KEEPALIVE_INTERVAL", 120),
		"凭据保活扫描间隔(秒)，设 0 禁用")
	keepAliveAhead := flag.Int("keepalive-ahead", envInt("YYB_KEEPALIVE_AHEAD", 240),
		"凭据剩余有效期低于此秒数时提前续期")
	enablePCLogin := flag.Bool("enable-pc-login", envBool("YYB_ENABLE_PC_LOGIN", false),
		"开启本机微信授权（quick-login）；仅 Windows 客户端场景有意义")
	sessionHours := flag.Int("session-hours", envInt("YYB_SESSION_HOURS", 168),
		"管理员登录会话有效期(小时)")
	protocolToken := flag.String("protocol-token", envOr("YYB_PROTOCOL_TOKEN", ""),
		"/wxapp/* 的 Bearer 令牌，留空表示不校验")
	deviceIdentity := flag.String("device-identity", envOr("YYB_DEVICE_IDENTITY", protocol.DeviceIdentityStable),
		"设备标识策略：stable=每个账号固定一套设备（默认），random=每次登录换新设备（历史行为）")
	asRefreshMode := flag.String("appstore-refresh-mode", envOr("YYB_APPSTORE_REFRESH_MODE", httpapi.ASRefreshLegacy),
		"应用宝凭据续期入口：legacy_auth=历史 _auth 入口（默认），source=按来源的现有入口")
	circuitBreaker := flag.Bool("circuit-breaker", envBool("YYB_CIRCUIT_BREAKER", true),
		"取码熔断（默认开）：已确认被拒的目标/会话在本地直接返回 429，不再重复请求上游")
	codeMinGap := flag.Duration("code-min-gap", envDuration("YYB_CODE_MIN_GAP", defaultCodeMinGap),
		"同一账号两次发往上游的请求最小间隔（默认 3s）；0 或 off 关闭限速")
	codeMaxWait := flag.Duration("code-max-wait", envDuration("YYB_CODE_MAX_WAIT", 20*time.Second),
		"取码请求为排档期最多等待多久（默认 20s），超时本地返回 429")
	loginBudget := flag.Int("login-budget", envInt("YYB_LOGIN_BUDGET", protocol.DefaultLoginBudget),
		"同一账号在窗口内允许因业务请求触发的重新登录次数（默认 3）；0 或 off 关闭预算")
	loginWindow := flag.Duration("login-window", envDuration("YYB_LOGIN_WINDOW", protocol.DefaultLoginWindow),
		"登录预算的滑动窗口长度（默认 10m）")
	notifyChannel := flag.String("notify-channel", envOr("YYB_NOTIFY_CHANNEL", ""),
		"额外推送通道，可选 serverchan / pushplus；企业微信仍用控制台里的 Webhook 地址")
	notifyToken := flag.String("notify-token", envOr("YYB_NOTIFY_TOKEN", ""),
		"额外推送通道的令牌（Server酱 SendKey / PushPlus token）")
	notifyTopic := flag.String("notify-topic", envOr("YYB_NOTIFY_TOPIC", ""),
		"PushPlus 群组编码（可选，仅 pushplus 使用）")
	cookieSecure := flag.Bool("cookie-secure", envBool("YYB_COOKIE_SECURE", false),
		"为会话 Cookie 加 Secure 标记（HTTPS 部署时开启）")
	trustProxy := flag.Bool("trust-proxy", envBool("YYB_TRUST_PROXY", false),
		"信任反向代理传来的 X-Forwarded-For/X-Real-IP（仅部署在可信代理后面时开启）")
	// 本地口令恢复：给出用户名后读完口令即退出，不启动服务。
	// 这是「口令彻底忘了」时唯一的自救通道，只改认证库，不碰业务库。
	flag.String(envFileName, envOr("YYB_ENV_FILE", ""),
		"dotenv 文件路径；留空则依次尝试可执行文件同目录与当前目录下的 .env（先于其它 flag 生效）")
	resetAdminUser := flag.String("reset-admin-password", envOr("YYB_RESET_ADMIN_PASSWORD", ""),
		"本地重置指定管理员的口令后退出（口令从标准输入读取；不会新建数据库）")
	flag.Parse()

	// 是否显式指定了资源根：命令行 --resource-root，或环境变量 / .env 里的
	// YYB_RESOURCE_ROOT。任一为真都说明部署方自己准备了一份 resource 目录，
	// 那就以他的文件为准；否则用二进制内嵌的那一份（独立二进制场景）。
	// 上游只看 flag.Visit，环境变量指定的部署反而会退回去用内嵌资源，这里补上。
	resourceRootExplicit := strings.TrimSpace(envOr("YYB_RESOURCE_ROOT", "")) != ""
	flag.Visit(func(current *flag.Flag) {
		if current.Name == "resource-root" {
			resourceRootExplicit = true
		}
	})

	// 自定义 DNS：必须在任何解析发生之前装好，所以紧跟 flag.Parse()。
	// 部署环境的系统 DNS 解析不到微信/应用宝域名时才需要配，留空则什么都不做。
	if dnsServers, err := configureDNS(envOr("YYB_DNS_SERVERS", "")); err != nil {
		log.Fatalf("配置 DNS 失败: %v", err)
	} else if len(dnsServers) > 0 {
		log.Printf("[dns] 已使用指定 DNS 服务器: %s", strings.Join(dnsServers, ", "))
	}

	// 管理员可通过环境变量预设，避免首次启动必须人工访问 /setup
	adminUser := envWithFallback("YYB_ADMIN_USER", "YYB_WEB_USER")
	adminPassword := envWithFallback("YYB_ADMIN_PASSWORD", "YYB_WEB_PASSWORD")

	// 构建应用配置
	cfg := httpapi.Config{
		ResourceRoot:      *resourceRoot,
		DataRoot:          *dataRoot,
		EmbeddedWebAssets: !resourceRootExplicit,
		DBFilename:        *dbFilename,
		AuthDBFilename:    *authDBFilename,
		TCPProxy:          *tcpProxy,
		SessionTTL:        30 * time.Minute, // MMTLS 登录会话有效期
		RequestTimeout:    8 * time.Second,  // 短连接请求超时
		AvatarTimeout:     10 * time.Second, // 下载头像超时
		QRSessionTTL:      5 * time.Minute,  // 二维码会话有效期
		CheckInterval:     time.Duration(*checkInterval) * time.Second,
		CheckStale:        time.Duration(*checkStale) * time.Second,
		KeepAliveInterval: time.Duration(*keepAliveInterval) * time.Second,
		KeepAliveAhead:    time.Duration(*keepAliveAhead) * time.Second,
		EnablePCLogin:     *enablePCLogin,
		SessionDuration:   time.Duration(*sessionHours) * time.Hour,
		CookieSecure:      *cookieSecure,
		ProtocolToken:     *protocolToken,
		DeviceIdentity:    *deviceIdentity,
		ASRefreshMode:     *asRefreshMode,

		DisableCircuitBreaker: !*circuitBreaker,
		CodeMinGap:            *codeMinGap,
		CodeMaxWait:           *codeMaxWait,
		LoginBudget:           *loginBudget,
		LoginWindow:           *loginWindow,

		NotifyChannel: *notifyChannel,
		NotifyToken:   *notifyToken,
		NotifyTopic:   *notifyTopic,

		TrustProxy:    *trustProxy,
		AdminUser:     adminUser,
		AdminPassword: adminPassword,
	}

	// 口令恢复模式：不初始化 App、不打开业务库、不监听端口。
	// 走这条路时任何有副作用的初始化都可能把事情弄得更糟
	// （例如误建一个空数据库，让服务下次启动读到一个没有账号的新库）。
	if *resetAdminUser != "" {
		if err := resetAdminPassword(cfg, *resetAdminUser, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "重置失败："+err.Error())
			os.Exit(1)
		}
		return
	}

	// 初始化 App（数据库、资源目录、连接池、认证库等）
	app, err := httpapi.NewApp(cfg)
	if err != nil {
		log.Fatalf("初始化 App 失败: %v", err)
	}
	defer app.Close()

	// 配置 HTTP 服务器
	addr := fmt.Sprintf("%s:%d", *host, *port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 在 goroutine 中启动监听
	go func() {
		log.Printf("yybwechat 服务已启动，监听 http://%s", addr)
		if cfg.ProtocolToken == "" {
			log.Printf("[security] 未配置 YYB_PROTOCOL_TOKEN，/accounts 与 /wxapp/* 不做令牌校验，请勿直接暴露到公网")
			if *host != "127.0.0.1" && *host != "localhost" && *host != "::1" {
				log.Printf("[security] 当前监听 %s，建议改为 --host 127.0.0.1 由反向代理转发，或设置 --protocol-token", *host)
			}
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务器异常退出: %v", err)
		}
	}()

	// 等待中断信号，实现优雅关闭
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	// 给予 5 秒时间完成现有请求后关闭
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("服务器关闭异常: %v", err)
	}
	log.Println("yybwechat 已停止")
}
