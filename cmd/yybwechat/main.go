// Package main 是 yybwechat 服务的启动入口。
// yybwechat 是一个微信扫码登录服务，提供扫码登录、账号管理
// 以及 wxapp 接口代理调用等功能。
//
// 运行方式：
//
//	yybwechat --host 0.0.0.0 --port 8000 \
//	          --resource-root ./resource \
//	          --tcp-proxy socks5://proxy:1080
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
	"syscall"
	"time"

	"yybwechat/internal/httpapi"
)

// main 是程序入口，负责解析命令行参数、初始化 App 实例、
// 启动 HTTP 服务器，并监听系统信号以实现优雅关闭。
func main() {
	// 解析命令行参数
	host := flag.String("host", "0.0.0.0", "监听地址")
	port := flag.Int("port", 8000, "监听端口")
	resourceRoot := flag.String("resource-root", filepath.Join(".", "resource"), "模板和静态资源目录")
	dataRoot := flag.String("data-root", "", "持久化数据目录（db/avatars/qr），默认同 resource-root")
	dbFilename := flag.String("db", httpapi.DefaultDBFilename, "SQLite 数据库文件名，位于 data-root/db 下")
	tcpProxy := flag.String("tcp-proxy", "", "可选 TCP 代理: socks5://host:port 或 http-connect://host:port")
	checkInterval := flag.Int("check-interval", 1800, "后台健康检查间隔(秒)，设 0 禁用")
	checkStale := flag.Int("check-stale", 3600, "账号未检查超过此时长(秒)视为过期并触发刷新")
	flag.Parse()

	// 构建应用配置
	cfg := httpapi.Config{
		ResourceRoot:   *resourceRoot,
		DataRoot:       *dataRoot,
		DBFilename:     *dbFilename,
		TCPProxy:       *tcpProxy,
		SessionTTL:     30 * time.Minute,  // 登录会话有效期
		RequestTimeout: 8 * time.Second,   // 短连接请求超时
		AvatarTimeout:  10 * time.Second,  // 下载头像超时
		QRSessionTTL:   5 * time.Minute,   // 二维码会话有效期
		CheckInterval:  time.Duration(*checkInterval) * time.Second,
		CheckStale:     time.Duration(*checkStale) * time.Second,
	}

	// 初始化 App（数据库、资源目录、连接池等）
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
}
