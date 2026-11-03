package protocol

import (
	"bytes"
	"regexp"
	"testing"
)

// 同一实例种子 + 同一账号必须永远得到同一套设备：
// 重启、缓存重建、重新扫码都不能换设备，否则等于把「固定设备」白做了。
func TestDeriveAppDeviceIDIsStablePerAccount(t *testing.T) {
	seed := bytes.Repeat([]byte{0x7f}, 32)
	first := deriveAppDeviceID(seed, "owNAX6nYDz83cktEDQxIoaAvAmk0")
	second := deriveAppDeviceID(seed, "owNAX6nYDz83cktEDQxIoaAvAmk0")
	if !bytes.Equal(first, second) {
		t.Fatal("same seed and account produced different device ids")
	}
	if len(first) != 32 {
		t.Fatalf("app device id length = %d, want 32", len(first))
	}
	if bytes.Equal(first, deriveAppDeviceID(seed, "pcmgr:624614834")) {
		t.Fatal("different accounts share one app device id")
	}
	if bytes.Equal(first, deriveAppDeviceID(bytes.Repeat([]byte{0x11}, 32), "owNAX6nYDz83cktEDQxIoaAvAmk0")) {
		t.Fatal("different instances share one app device id")
	}
}

func TestDeriveAppDeviceIDNeedsSeedAndAccount(t *testing.T) {
	if got := deriveAppDeviceID(nil, "openid"); got != nil {
		t.Fatal("empty seed must not derive a device id")
	}
	if got := deriveAppDeviceID(bytes.Repeat([]byte{1}, 32), ""); got != nil {
		t.Fatal("empty account must not derive a device id")
	}
}

// 设备 MAC 必须是「本地管理 + 单播」的形状，和历史上随机会话设备一致。
func TestFormatSessDeviceShape(t *testing.T) {
	got := string(formatSessDevice([]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab}))
	if !regexp.MustCompile(`^[0-9A-F]{2}(-[0-9A-F]{2}){5}$`).MatchString(got) {
		t.Fatalf("unexpected MAC shape: %s", got)
	}
	first := got[:2]
	if first != "03" && first != "02" {
		// 0x01 | 0x02 = 0x03，再清掉组播位
		t.Fatalf("first octet = %s, want locally administered unicast", first)
	}
}

// 随机设备开关打开时不能返回稳定标识，否则「关掉」这个选项就是假的。
func TestDeviceForHonoursRandomMode(t *testing.T) {
	pool := &Pool{cfg: Config{DeviceIdentity: DeviceIdentityRandom}}
	if device := pool.deviceFor(nil, nil); !device.empty() {
		t.Fatal("random mode still produced a stable device")
	}
}
