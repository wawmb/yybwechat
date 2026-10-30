package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// configureDNS 用 YYB_DNS_SERVERS 里指定的服务器替换进程默认解析器。
//
// 什么时候需要它：部署环境的系统 DNS 解析不到微信/应用宝的域名，或者被劫持到
// 错误的 IP。只换解析服务器，不碰出网链路——代理仍然走各自的代理配置。
// 留空时什么都不做，沿用系统解析器。
func configureDNS(raw string) ([]string, error) {
	servers, err := parseDNSServers(raw)
	if err != nil || len(servers) == 0 {
		return servers, err
	}
	var next atomic.Uint32
	net.DefaultResolver = &net.Resolver{
		PreferGo:     true,
		StrictErrors: false,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// 多服务器轮询：单台超时不影响整轮解析。
			server := servers[int(next.Add(1)-1)%len(servers)]
			if network != "tcp" && network != "tcp4" && network != "tcp6" {
				network = "udp"
			}
			dialer := net.Dialer{Timeout: 4 * time.Second}
			return dialer.DialContext(ctx, network, server)
		},
	}
	return servers, nil
}

// parseDNSServers 只接受 IP 或 IP:port，端口缺失补 53。
// 不接受域名：解析器自己要用的服务器不能再依赖一次解析，否则是鸡生蛋。
func parseDNSServers(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' })
	servers := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		host, port, err := net.SplitHostPort(value)
		if err != nil {
			if net.ParseIP(value) == nil {
				return nil, fmt.Errorf("invalid DNS server %q; use IP or IP:port", value)
			}
			host, port = value, "53"
		}
		if net.ParseIP(host) == nil {
			return nil, fmt.Errorf("invalid DNS server IP %q", host)
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return nil, fmt.Errorf("invalid DNS server port %q", port)
		}
		servers = append(servers, net.JoinHostPort(host, port))
	}
	return servers, nil
}
