package protocol

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"

	"yybwechat/internal/store"
)

const (
	// DeviceIdentityStable 表示每个账号固定一套设备标识（默认）。
	DeviceIdentityStable = "stable"
	// DeviceIdentityRandom 恢复历史行为：每次登录/请求都换新设备。
	DeviceIdentityRandom = "random"

	deviceSeedSetting = "protocol_device_seed"
)

// DeviceIdentity 是一个账号发往 WMPF 接口时使用的设备标识。
//
// 官方客户端会长期保存同一个 AppDeviceId 与机器地址；此前本项目每次 ManualAuth
// 都重新生成 AppDeviceId，同一账号在上游看来就是一台不断更换设备的新机器。
// 会话设备（MAC）本项目早已按账号持久化（wechat_accounts.device_id），这一轮补上
// 稳定的 AppDeviceId，两者才都与官方客户端的口径一致。
type DeviceIdentity struct {
	AppDeviceID []byte
	SessDevice  []byte
}

func (d DeviceIdentity) empty() bool {
	return len(d.AppDeviceID) == 0 || len(d.SessDevice) == 0
}

// deriveAppDeviceID 对「实例种子 + 账号」是确定的，
// 因此重启、缓存重建、重新扫码都不改变设备；账号之间、部署之间互不相同。
func deriveAppDeviceID(seed []byte, account string) []byte {
	if len(seed) == 0 || account == "" {
		return nil
	}
	return deviceHMAC(seed, "yybwechat/app-device/v1", account)
}

func deviceHMAC(seed []byte, label, account string) []byte {
	h := hmac.New(sha256.New, seed)
	h.Write([]byte(label))
	h.Write([]byte{0})
	h.Write([]byte(account))
	return h.Sum(nil)
}

// formatSessDevice 把 6 字节 MAC 渲染成本地管理地址的文本形式，
// 与历史随机会话设备保持同一种形状（XX-XX-XX-XX-XX-XX）。
func formatSessDevice(mac []byte) []byte {
	b := append([]byte(nil), mac[:6]...)
	b[0] = (b[0] | 0x02) & 0xfe
	return []byte(fmt.Sprintf("%02X-%02X-%02X-%02X-%02X-%02X", b[0], b[1], b[2], b[3], b[4], b[5]))
}

func deviceAccountKey(acc *store.WechatAccount) string {
	if acc == nil {
		return ""
	}
	if acc.OpenID != "" {
		return acc.OpenID
	}
	if acc.ID > 0 {
		return "account:" + strconv.FormatInt(acc.ID, 10)
	}
	return ""
}

// deviceFor 返回该账号应使用的设备标识；禁用稳定标识或种子读不到时返回空值，
// 调用方据此回落到历史上的随机设备。
//
// 与本项目自研的设备持久化保持一致：会话设备（MAC）依然取 wechat_accounts.device_id，
// 这样 /accounts 与界面上显示的那个 device_id 就是实际发出去的值，不会出现
// 「库里一个、请求里另一个」的两套口径。
func (p *Pool) deviceFor(ctx context.Context, acc *store.WechatAccount) DeviceIdentity {
	if p.cfg.DeviceIdentity == DeviceIdentityRandom {
		return DeviceIdentity{}
	}
	seed, err := p.deviceSeed(ctx)
	if err != nil {
		log.Printf("[device] 设备标识种子不可用，本次使用随机设备: %v", err)
		return DeviceIdentity{}
	}
	device := DeviceIdentity{AppDeviceID: deriveAppDeviceID(seed, deviceAccountKey(acc))}
	if acc != nil && acc.ID > 0 {
		if mac, macErr := p.db.EnsureAccountDeviceID(ctx, acc.ID); macErr == nil && mac != "" {
			device.SessDevice = []byte(mac)
		}
	}
	return device
}

// deviceSeed 取（必要时生成并落库）实例级设备种子。
func (p *Pool) deviceSeed(ctx context.Context) ([]byte, error) {
	p.deviceSeedMu.Lock()
	defer p.deviceSeedMu.Unlock()
	if len(p.deviceSeedValue) != 0 {
		return p.deviceSeedValue, nil
	}
	if p.db == nil {
		return nil, fmt.Errorf("no database")
	}
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	// 多个实例共用一个库时，以先写入的种子为准，后启动的读回同一份。
	value, err := p.db.EnsureSetting(ctx, deviceSeedSetting, hex.EncodeToString(fresh))
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(value)
	if err != nil || len(seed) < 16 {
		return nil, fmt.Errorf("invalid stored device seed")
	}
	p.deviceSeedValue = seed
	return seed, nil
}
