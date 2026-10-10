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
	"syscall"
	"time"

	"yybwechat/internal/httpapi"
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
	checkInterval := flag.Int("check-interval", envInt("YYB_CHECK_INTERVAL", 1800),
		"后台健康检查间隔(秒)，设 0 禁用")
	checkStale := flag.Int("check-stale", envInt("YYB_CHECK_STALE", 3600),
		"账号未检查超过此时长(秒)视为过期并触发刷新")
	keepAliveInterval := flag.Int("keepalive-interval", envInt("YYB_KEEPALIVE_INTERVAL", 60),
		"凭据保活扫描间隔(秒)，设 0 禁用")
	keepAliveAhead := flag.Int("keepalive-ahead", envInt("YYB_KEEPALIVE_AHEAD", 2700),
		"凭据剩余有效期低于此秒数时提前续期")
	sessionHours := flag.Int("session-hours", envInt("YYB_SESSION_HOURS", 168),
		"管理员登录会话有效期(小时)")
	protocolToken := flag.String("protocol-token", envOr("YYB_PROTOCOL_TOKEN", ""),
		"/wxapp/* 的 Bearer 令牌，留空表示不校验")
	cookieSecure := flag.Bool("cookie-secure", envBool("YYB_COOKIE_SECURE", false),
		"为会话 Cookie 加 Secure 标记（HTTPS 部署时开启）")
	flag.Parse()

	// 管理员可通过环境变量预设，避免首次启动必须人工访问 /setup
	adminUser := envWithFallback("YYB_ADMIN_USER", "YYB_WEB_USER")
	adminPassword := envWithFallback("YYB_ADMIN_PASSWORD", "YYB_WEB_PASSWORD")

	// 构建应用配置
	cfg := httpapi.Config{
		ResourceRoot:      *resourceRoot,
		DataRoot:          *dataRoot,
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
		SessionDuration:   time.Duration(*sessionHours) * time.Hour,
		CookieSecure:      *cookieSecure,
		ProtocolToken:     *protocolToken,
		AdminUser:         adminUser,
		AdminPassword:     adminPassword,
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
			log.Printf("[security] 未配置 YYB_PROTOCOL_TOKEN，/wxapp/* 接口不做令牌校验，请勿直接暴露到公网")
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
