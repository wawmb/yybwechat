package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 微信 NewDNS 服务相关常量
const (
	newdnsHost      = "aedns.weixin.qq.com"
	newdnsBackupIP  = "180.153.202.85"
	newdnsPath      = "/cgi-bin/default/getdns"
	longlinkDomain  = "longcloud.weixin.com"
	shortlinkDomain = "shortcloud.weixin.com"
	protoMMTLS      = "mmtlsovertcp"
	protoHTTP       = "http"
	defaultUA       = "MicroMessenger Client"
)

// shortlinkFallbackTargets 是 HTTPDNS 不可达时的短链兜底出口。
//
// 旧值 120.241.131.173 已被微信下线（实测不通）：微信近期把短链迁到
// aewebproxy.weixin.qq.com，旧 IP 只能让取码/保活直接失败。这里换成一组
// 实测可达的官方出口，最后再补一条官方域名，交给 DNS 或代理去解析。
var shortlinkFallbackTargets = []Target{
	{IP: "116.128.171.253", Port: 80},
	{IP: "140.207.120.146", Port: 80},
	{IP: "182.50.15.223", Port: 80},
	{IP: "182.50.15.171", Port: 80},
}

// Target 表示一个 MMTLS 服务的目标地址。
type Target struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// dnsDomain 表示 DNS 解析返回的域名信息。
type dnsDomain struct {
	IPs       []string
	Protocols map[string][]int
	Timeout   int
}

// dnsCacheEntry 是 DNS 缓存的条目，包含过期时间和解析结果。
type dnsCacheEntry struct {
	ExpiresAt time.Time
	Parsed    map[string]dnsDomain
}

// dnsCache 是全局 DNS 缓存，键为 "clientversion|devicetype" 格式。
var dnsCache = struct {
	sync.Mutex
	entries map[string]dnsCacheEntry
}{entries: map[string]dnsCacheEntry{}}

func buildDNSQuery(clientVersion int, deviceType string, uin int) string {
	// 早期调用方传 0 / 空串，微信侧会因此拿不到域名列表。
	// 补上与 ilink 客户端一致的版本号与设备类型，提升解析成功率。
	if clientVersion == 0 {
		clientVersion = clientVersionDefault
	}
	if deviceType == "" {
		deviceType = "Windows"
	}
	v := url.Values{}
	v.Set("clientversion", strconv.Itoa(clientVersion))
	v.Set("devicetype", deviceType)
	v.Set("uin", strconv.Itoa(uin))
	v.Set("format", "json")
	return v.Encode()
}

func requestNewDNS(ctx context.Context, connectTo string, timeout time.Duration) (int, map[string]any, string, error) {
	host := connectTo
	if host == "" {
		host = newdnsHost
	}
	conn, err := dialDirect(ctx, host, 80, timeout)
	if err != nil {
		return 0, nil, "", err
	}
	defer conn.Close()
	// 截止时间必须取自 ctx：上层给 HTTPDNS 的预算只是整体登录预算的一部分，
	// 用 now+timeout 会让 ctx 到期后 io.ReadAll 仍然阻塞到 timeout，
	// 从而击穿调用方的整体时间预算。
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	_ = conn.SetDeadline(ioDeadline(ctx, timeout))
	path := newdnsPath + "?" + buildDNSQuery(0, "Windows", 0)
	req := fmt.Sprintf("GET %s HTTP/1.0\r\nHost: %s\r\nUser-Agent: %s\r\nAccept: */*\r\nConnection: close\r\n\r\n", path, newdnsHost, defaultUA)
	if _, err = conn.Write([]byte(req)); err != nil {
		return 0, nil, "", err
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		return 0, nil, "", err
	}
	head, body := splitHTTP(raw)
	status := 0
	if lines := strings.Split(head, "\n"); len(lines) > 0 {
		parts := strings.Fields(lines[0])
		if len(parts) >= 2 {
			status, _ = strconv.Atoi(parts[1])
		}
	}
	text := string(body)
	var obj map[string]any
	_ = json.Unmarshal(body, &obj)
	return status, obj, text, nil
}

func getDNSParsed(ctx context.Context, timeout, cacheTTL time.Duration, force bool) (map[string]dnsDomain, error) {
	key := "0|Windows"
	now := time.Now()
	if !force && cacheTTL > 0 {
		dnsCache.Lock()
		if ent, ok := dnsCache.entries[key]; ok && ent.ExpiresAt.After(now) {
			dnsCache.Unlock()
			return ent.Parsed, nil
		}
		dnsCache.Unlock()
	}
	var last error
	for _, connectTo := range []string{"", newdnsBackupIP} {
		status, obj, text, err := requestNewDNS(ctx, connectTo, timeout)
		if err != nil {
			last = err
			continue
		}
		if status != 200 || obj == nil {
			last = fmt.Errorf("newdns HTTP %d body=%q", status, text[:min(len(text), 120)])
			continue
		}
		parsed, err := parseDomainList(obj)
		if err != nil {
			last = err
			continue
		}
		if cacheTTL > 0 {
			dnsCache.Lock()
			dnsCache.entries[key] = dnsCacheEntry{ExpiresAt: now.Add(cacheTTL), Parsed: parsed}
			dnsCache.Unlock()
		}
		return parsed, nil
	}
	if last == nil {
		last = fmt.Errorf("newdns request failed")
	}
	return nil, last
}

func parseDomainList(obj map[string]any) (map[string]dnsDomain, error) {
	dnsObj, ok := obj["dns"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("dns missing in response")
	}
	if rc, ok := dnsObj["retcode"].(float64); ok && int(rc) != 0 {
		return nil, fmt.Errorf("newdns retcode=%d", int(rc))
	}
	list, ok := dnsObj["domainlist"].([]any)
	if !ok {
		return nil, fmt.Errorf("domainlist missing in response")
	}
	out := map[string]dnsDomain{}
	for _, item := range list {
		d, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := d["name"].(string)
		if name == "" {
			continue
		}
		dom := dnsDomain{Protocols: map[string][]int{}}
		if t, ok := d["timeout"].(float64); ok {
			dom.Timeout = int(t)
		}
		if ips, ok := d["iplist"].([]any); ok {
			for _, it := range ips {
				if m, ok := it.(map[string]any); ok {
					if ip, ok := m["ip"].(string); ok && net.ParseIP(ip) != nil {
						dom.IPs = append(dom.IPs, ip)
					}
				}
			}
		}
		if plist, ok := d["protocollist"].([]any); ok {
			for _, it := range plist {
				p, ok := it.(map[string]any)
				if !ok {
					continue
				}
				pname, _ := p["name"].(string)
				if pname == "" {
					continue
				}
				if ports, ok := p["portlist"].([]any); ok {
					for _, pv := range ports {
						if f, ok := pv.(float64); ok {
							dom.Protocols[pname] = append(dom.Protocols[pname], int(f))
						}
					}
				}
			}
		}
		out[name] = dom
	}
	return out, nil
}

func serversFor(parsed map[string]dnsDomain, domain, proto string) []Target {
	info, ok := parsed[domain]
	if !ok {
		return nil
	}
	ports := info.Protocols[proto]
	var out []Target
	for _, ip := range info.IPs {
		for _, p := range ports {
			out = append(out, Target{IP: ip, Port: p})
		}
	}
	return out
}

// getLonglinkTargets 返回 LongLink 候选端点，永不返回空列表。
//
// HTTPDNS 只是一项优化：它失败、或返回了 200 却没有 longlink 条目、
// 或条目里没有可用 IP，都不代表网络不可用——很多网络会拦掉微信 HTTPDNS，
// 但普通 DNS 与 LongLink 的 443 端口仍然可达。这些情况一律退化为官方域名，
// 交给 net.Dialer（或已配置的 TCP 代理）去解析，至少还有一次尝试机会。
func getLonglinkTargets(ctx context.Context, timeout, cacheTTL time.Duration) ([]Target, error) {
	parsed, err := getDNSParsed(ctx, timeout, cacheTTL, false)
	if err == nil {
		if targets := serversFor(parsed, longlinkDomain, protoMMTLS); len(targets) > 0 {
			return targets, nil
		}
	}
	return []Target{{IP: longlinkDomain, Port: 443}}, nil
}

func getShortlinkTargets(ctx context.Context, timeout, cacheTTL time.Duration) []Target {
	if parsed, err := getDNSParsed(ctx, timeout, cacheTTL, false); err == nil {
		// 当前后端下发的是 mmtlsoverhttp；回落 http 以兼容旧数据。
		for _, proto := range []string{"mmtlsoverhttp", protoHTTP} {
			seen := map[string]bool{}
			var out []Target
			for _, t := range serversFor(parsed, shortlinkDomain, proto) {
				if t.Port == 80 && !seen[t.IP] {
					seen[t.IP] = true
					out = append(out, t)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	// HTTPDNS 在部分网络下不可用：退到官方出口，最后补域名让
	// DNS / 代理自行解析（该域名现在 CNAME 到 aewebproxy.weixin.qq.com）。
	out := append([]Target(nil), shortlinkFallbackTargets...)
	return append(out, Target{IP: shortlinkDomain, Port: 80})
}

// orderLonglinkTargets 按端口优先级排列候选，并为未列入优先级的端口兜底。
//
// 两个容易踩的点：
//   - 去重必须按 IP+Port，不能只按 IP。同一个 IP 常同时通告 8080 和 443，
//     只按 IP 去重会把 443 丢掉；而不少 CONNECT 代理只放行 443，
//     结果就是所有候选全灭。
//   - 443 排在最前，同样是因为代理放行端口的限制。同端口内保持解析顺序，
//     不再按 IP 字典序重排——上游给的顺序本身带有倾向性。
func orderLonglinkTargets(targets []Target, max int) []Target {
	pref := []int{443, 8080, 80, 5000}
	seen := map[Target]bool{}
	var out []Target
	add := func(t Target) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, p := range pref {
		for _, t := range targets {
			if t.Port == p {
				add(t)
			}
		}
	}
	for _, t := range targets {
		add(t)
	}
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// longlinkLoginTargets 在候选列表末尾永远预留一个官方域名回退位。
//
// HTTPDNS 返回成功并不代表它的端点可达；如果候选已经占满名额，
// 就挤掉最后一个，保证回退位一定在——否则一旦 HTTPDNS 给的 IP 全不通，
// 这次登录就没有任何退路。
func longlinkLoginTargets(targets []Target, max int) []Target {
	fallback := Target{IP: longlinkDomain, Port: 443}
	ordered := orderLonglinkTargets(targets, 0)
	out := make([]Target, 0, len(ordered)+1)
	for _, target := range ordered {
		if target != fallback {
			out = append(out, target)
		}
	}
	if max > 0 && len(out) >= max {
		out = out[:max-1]
	}
	return append(out, fallback)
}

func splitHTTP(raw []byte) (string, []byte) {
	idx := strings.Index(string(raw), "\r\n\r\n")
	if idx < 0 {
		return "", raw
	}
	return string(raw[:idx]), raw[idx+4:]
}
