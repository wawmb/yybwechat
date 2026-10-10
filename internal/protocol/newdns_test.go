package protocol

import (
	"bytes"
	"testing"
)

// TestSplitHTTP 覆盖 HTTP 响应头与响应体的切分，这是 NewDNS 走 HTTP 通道时
// 取出响应体的第一步；切分错误会导致后续把响应头当成 protobuf 去解析。
func TestSplitHTTP(t *testing.T) {
	cases := []struct {
		name     string
		raw      []byte
		wantHead string
		wantBody string
	}{
		{"标准响应", []byte("HTTP/1.1 200 OK\r\nA: 1\r\n\r\nbody"), "HTTP/1.1 200 OK\r\nA: 1", "body"},
		{"响应体为空", []byte("HTTP/1.1 204\r\n\r\n"), "HTTP/1.1 204", ""},
		{"响应体含二进制", []byte("H\r\n\r\n\x00\xff"), "H", "\x00\xff"},
		{"没有分隔空行", []byte("no header separator"), "", "no header separator"},
		{"空输入", nil, "", ""},
	}
	for _, c := range cases {
		head, body := splitHTTP(c.raw)
		if head != c.wantHead {
			t.Fatalf("%s: 响应头期望 %q，实际 %q", c.name, c.wantHead, head)
		}
		if body != nil && !bytes.Equal(body, []byte(c.wantBody)) {
			t.Fatalf("%s: 响应体期望 %q，实际 %q", c.name, c.wantBody, body)
		}
	}
}

// TestSplitHTTPKeepsCRLFInBody 确认响应体里的空行不会被二次切分，
// 只认第一个分隔符。
func TestSplitHTTPKeepsCRLFInBody(t *testing.T) {
	raw := []byte("head\r\n\r\nfirst\r\n\r\nsecond")
	head, body := splitHTTP(raw)
	if head != "head" {
		t.Fatalf("响应头期望 head，实际 %q", head)
	}
	if string(body) != "first\r\n\r\nsecond" {
		t.Fatalf("响应体应保留后续空行，实际 %q", body)
	}
}

// TestServersFor 覆盖域名/协议匹配与缺失两种情况：
// 目标列表直接决定后续 TCP 连到哪里，取错会让握手连到无关端口。
func TestServersFor(t *testing.T) {
	parsed := map[string]dnsDomain{
		"longlink.weixin.qq.com": {
			IPs:       []string{"1.2.3.4", "5.6.7.8"},
			Protocols: map[string][]int{"longlink": {443, 8080}, "shortlink": {80}},
		},
	}

	got := serversFor(parsed, "longlink.weixin.qq.com", "longlink")
	// 2 个 IP × 2 个端口 = 4 个目标
	if len(got) != 4 {
		t.Fatalf("长连接目标数应为 4，实际 %d: %+v", len(got), got)
	}
	if got[0].IP != "1.2.3.4" || got[0].Port != 443 {
		t.Fatalf("首个目标期望 1.2.3.4:443，实际 %s:%d", got[0].IP, got[0].Port)
	}

	short := serversFor(parsed, "longlink.weixin.qq.com", "shortlink")
	if len(short) != 2 {
		t.Fatalf("短连接目标数应为 2（2 IP × 1 端口），实际 %d", len(short))
	}

	if unknown := serversFor(parsed, "no.such.domain", "longlink"); unknown != nil {
		t.Fatalf("未知域名应返回 nil，实际 %+v", unknown)
	}
	if missing := serversFor(parsed, "longlink.weixin.qq.com", "noproto"); missing != nil {
		t.Fatalf("未知协议应返回 nil，实际 %+v", missing)
	}
}
