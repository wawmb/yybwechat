package protocol

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
)

// buildTestLoginBuffer 按 login_buffer 的实际布局拼一段测试数据：
// 字段 1 是 ticket，字段 2 是设备标识，字段 3 是可选的宿主 AppID。
func buildTestLoginBuffer(ticket, device, host []byte) string {
	raw := pbLen(1, ticket)
	raw = append(raw, pbLen(2, device)...)
	if host != nil {
		raw = append(raw, pbLen(3, host)...)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestParseLoginBufferRoundTrip(t *testing.T) {
	ticket := []byte("ticket-bytes-001")
	device := []byte("02-11-22-33-44-55")
	host := []byte("wx141bfb9b73c970a9")

	meta, err := parseLoginBuffer(buildTestLoginBuffer(ticket, device, host))
	if err != nil {
		t.Fatalf("解析合法 login_buffer 失败: %v", err)
	}
	if !bytes.Equal(meta.Ticket, ticket) {
		t.Fatalf("ticket 解析错误: %q", meta.Ticket)
	}
	if !bytes.Equal(meta.DeviceID, device) {
		t.Fatalf("设备标识解析错误: %q", meta.DeviceID)
	}
	if !bytes.Equal(meta.HostAppID, host) {
		t.Fatalf("宿主 AppID 解析错误: %q", meta.HostAppID)
	}
	raw, _ := base64.StdEncoding.DecodeString(buildTestLoginBuffer(ticket, device, host))
	if !bytes.Equal(meta.Raw, raw) {
		t.Fatal("Raw 应保留解码后的完整字节，后续构造握手请求时要用到")
	}
}

// TestParseLoginBufferHostAppIDOptional 确认字段 3 缺失时不报错，
// 只是 HostAppID 为空——真实环境下并非所有 login_buffer 都带宿主 AppID。
func TestParseLoginBufferHostAppIDOptional(t *testing.T) {
	meta, err := parseLoginBuffer(buildTestLoginBuffer([]byte("tk"), []byte("dv"), nil))
	if err != nil {
		t.Fatalf("缺少宿主 AppID 不应报错: %v", err)
	}
	if len(meta.HostAppID) != 0 {
		t.Fatalf("缺少字段 3 时 HostAppID 应为空，实际 %q", meta.HostAppID)
	}
}

func TestParseLoginBufferRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"不是合法 base64", "!!!not-base64!!!"},
		{"是合法 base64 但不是 protobuf", base64.StdEncoding.EncodeToString([]byte("plain text"))},
		{"缺少设备标识字段", base64.StdEncoding.EncodeToString(pbLen(1, []byte("tk")))},
		{"ticket 为空", base64.StdEncoding.EncodeToString(pbLen(2, []byte("dv")))},
		{"空字符串", ""},
	}
	for _, c := range cases {
		if _, err := parseLoginBuffer(c.in); err == nil {
			t.Fatalf("%s 应当返回错误", c.name)
		}
	}
}

// TestRandomSessDeviceFormat 校验设备 MAC 的两个关键位：
// 最低位为 0 表示单播，次低位为 1 表示本地管理。
//
// 注意这里**不**断言首字节等于 02：generateDeviceMAC 用的是 (b|0x02)&0xfe，
// 首字节是随机的偶数（如 4A、B2），只保证上述两个位，并非固定 02 开头。
func TestRandomSessDeviceFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		d := string(randomSessDevice())
		seen[d] = true

		// 6 组两位十六进制 + 5 个连字符 = 17 字符
		if len(d) != 17 {
			t.Fatalf("设备标识格式错误: %q（长度 %d，应为 17）", d, len(d))
		}
		if strings.Count(d, "-") != 5 {
			t.Fatalf("设备标识应有 5 个连字符: %q", d)
		}

		first, err := strconv.ParseUint(d[0:2], 16, 8)
		if err != nil {
			t.Fatalf("设备标识首段不是合法十六进制: %q", d)
		}
		if first&0x01 != 0 {
			t.Fatalf("设备 MAC 最低位应为 0（单播），实际首字节 %02X（%q）", first, d)
		}
		if first&0x02 == 0 {
			t.Fatalf("设备 MAC 次低位应为 1（本地管理），实际首字节 %02X（%q）", first, d)
		}
	}
	if len(seen) < 30 {
		t.Fatalf("32 次调用只生成 %d 个不同标识，随机性不足", len(seen))
	}
}

func TestRandomAppDeviceIDLength(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		id, err := randomAppDeviceID()
		if err != nil {
			t.Fatalf("生成设备随机串失败: %v", err)
		}
		if len(id) != 32 {
			t.Fatalf("设备随机串应为 32 字节，实际 %d", len(id))
		}
		seen[string(id)] = true
	}
	if len(seen) != 16 {
		t.Fatalf("16 次调用只生成 %d 个不同随机串，随机源可能失效", len(seen))
	}
}

// TestBuildManualAuthRequest 确认合法输入能构造出握手请求体，
// 且非法 login_buffer 会被拦下而不是产出一段残缺的请求。
func TestBuildManualAuthRequest(t *testing.T) {
	appDeviceID, err := randomAppDeviceID()
	if err != nil {
		t.Fatalf("生成设备随机串失败: %v", err)
	}
	valid := buildTestLoginBuffer([]byte("tk"), []byte("dv"), []byte("wx141bfb9b73c970a9"))

	body, err := buildManualAuthRequest(valid, appDeviceID)
	if err != nil {
		t.Fatalf("构造握手请求失败: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("握手请求体不应为空")
	}
	// 请求体应包含传入的设备随机串
	if !bytes.Contains(body, appDeviceID) {
		t.Fatal("握手请求体应包含设备随机串")
	}

	if _, err := buildManualAuthRequest("not-a-valid-buffer", appDeviceID); err == nil {
		t.Fatal("非法 login_buffer 应当返回错误")
	}
}
