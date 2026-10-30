package protocol

import (
	"strings"
	"testing"

	"yybwechat/internal/pcmgr"
)

// jsapiField 按字段号取出 buildJSAPIPlaintext 结果里的字段。
//
// 明文包的字段布局：1=sessionInfo 2=transURL 3=hostAppID 4=固定 5 5=loginReq
// 6=appID 7=transCmdID 8=固定 9=plugin 10=固定。
func jsapiField(t *testing.T, plain []byte, want int) []byte {
	t.Helper()
	fields := pbParse(plain)
	value, ok := fields[want]
	if !ok {
		t.Fatalf("JSAPI 明文缺少字段 %d", want)
	}
	data, ok := value.([]byte)
	if !ok {
		t.Fatalf("字段 %d 不是字节串: %T", want, value)
	}
	return data
}

func jsapiVar(t *testing.T, plain []byte, want int) int64 {
	t.Helper()
	return pbFieldVar(t, pbParse(plain), want)
}

func pbFieldVar(t *testing.T, fields map[int]any, want int) int64 {
	t.Helper()
	value, ok := fields[want].(int64)
	if !ok {
		t.Fatalf("字段 %d 不是 varint: %T", want, fields[want])
	}
	return value
}

// TestPhoneRequestUsesSessionChannelIdentity 锁住 0.2.49 的修复：
// 手机号请求必须带**会话所属渠道**的 HostAppID，并按该渠道的原生构造发包。
//
// 这条测试的价值在于反向验证：把 buildPhoneRequest 的 hostAppID 参数改回 nil，
// 或者把 1141 改回 1020，两个渠道都会退回同一条「应用宝身份 + 兼容形态」的路径，
// 于是用电脑管家会话要手机号必然被拒——而这类问题在本地只表现为一个业务码，
// 很容易被误判成「小程序没权限」。
func TestPhoneRequestUsesSessionChannelIdentity(t *testing.T) {
	uin := int64(123456789)
	testDevice := []byte("02-11-22-33-44-55")

	t.Run("应用宝：1020 兼容形态", func(t *testing.T) {
		plain := buildPhoneRequest(uin, "wxappid", []byte(hostAppIDDefault), testDevice)
		if got := jsapiVar(t, plain, 7); got != phoneCmdID {
			t.Fatalf("应用宝手机号命令号 = %d, want %d", got, phoneCmdID)
		}
		if got := string(jsapiField(t, plain, 3)); got != string(hostAppIDDefault) {
			t.Fatalf("hostAppID = %q, want 应用宝默认 %q", got, hostAppIDDefault)
		}
		// 兼容形态的标志：loginReq 里有字段 4 且没有 JSON 字段 3
		loginReq := pbParse(jsapiField(t, plain, 5))
		if _, ok := loginReq[4]; !ok {
			t.Fatal("应用宝手机号请求应保留字段 4")
		}
	})

	t.Run("电脑管家：1141 原生形态", func(t *testing.T) {
		plain := buildPhoneRequest(uin, "wxappid", []byte(pcmgr.HostAppID), testDevice)
		if got := jsapiVar(t, plain, 7); got != 1141 {
			t.Fatalf("管家手机号命令号 = %d, want 1141", got)
		}
		if got := string(jsapiField(t, plain, 3)); got != pcmgr.HostAppID {
			t.Fatalf("hostAppID = %q, want %q", got, pcmgr.HostAppID)
		}
		loginReq := pbParse(jsapiField(t, plain, 5))
		body, ok := loginReq[3].([]byte)
		if !ok {
			t.Fatal("管家手机号请求应在字段 3 放 JSON 请求体")
		}
		if !strings.Contains(string(body), "webapi_getuserwxphone") {
			t.Fatalf("请求体应包含 webapi_getuserwxphone，got %q", body)
		}
	})
}

// TestOperateRequestVersionTypePerChannel 锁住 versionType 的渠道差异。
//
// 管家原生默认按正式版（1）发送，应用宝沿用 0。两者混用会让服务端按错误的
// 客户端类型解释整个请求，表现同样是「业务码拒绝」。
func TestOperateRequestVersionTypePerChannel(t *testing.T) {
	uin := int64(1)
	testDevice := []byte("02-11-22-33-44-55")
	payload := map[string]any{"api_name": "webapi_test", "data": map[string]any{}, "env": 1}

	appstore, err := buildOperateRequest(uin, "wxappid", payload, []byte(hostAppIDDefault), testDevice)
	if err != nil {
		t.Fatalf("buildOperateRequest(appstore) error = %v", err)
	}
	if got := jsapiVar(t, appstore, 7); got != operateCmdID {
		t.Fatalf("应用宝转发命令号 = %d, want %d", got, operateCmdID)
	}
	if got := pbFieldVar(t, pbParse(jsapiField(t, appstore, 5)), 6); got != 0 {
		t.Fatalf("应用宝 versionType = %d, want 0", got)
	}

	pcmgrPlain, err := buildOperateRequest(uin, "wxappid", payload, []byte(pcmgr.HostAppID), testDevice)
	if err != nil {
		t.Fatalf("buildOperateRequest(pcmgr) error = %v", err)
	}
	if got := pbFieldVar(t, pbParse(jsapiField(t, pcmgrPlain, 5)), 6); got != 1 {
		t.Fatalf("管家 versionType = %d, want 1", got)
	}
	if got := string(jsapiField(t, pcmgrPlain, 3)); got != pcmgr.HostAppID {
		t.Fatalf("管家 hostAppID = %q, want %q", got, pcmgr.HostAppID)
	}
}

// TestSessionHostAppIDFallsBackToAppstore 验证缺省回落，避免空值让请求头没有产品身份。
func TestSessionHostAppIDFallsBackToAppstore(t *testing.T) {
	if got := string(sessionHostAppID(nil)); got != string(hostAppIDDefault) {
		t.Fatalf("空 hostAppID 应回落应用宝默认值，got %q", got)
	}
	if got := string(sessionHostAppID([]byte(pcmgr.HostAppID))); got != pcmgr.HostAppID {
		t.Fatalf("已有渠道身份不应被覆盖，got %q", got)
	}
}

// TestExtractSessionReportsManualAuthRejection 验证票据被拒时返回类型化错误。
//
// 修复前这种响应会走到「missing f3」分支，上层只看到解析失败：
// 既无法据此去换一张新票，也无法把「票据问题」和「账号失效」分开。
func TestExtractSessionReportsManualAuthRejection(t *testing.T) {
	// 外层字段 1 是 base 块，其中字段 1 是业务码
	rejectCode := int64(-10)
	base := pbVar(1, uint64(rejectCode))
	mar := pbLen(1, base)

	_, err := extractSession(mar)
	if err == nil {
		t.Fatal("业务码非 0 时应返回错误")
	}
	rejected, ok := err.(*ManualAuthRejectedError)
	if !ok {
		t.Fatalf("应为 *ManualAuthRejectedError，got %T: %v", err, err)
	}
	if rejected.Code != -10 {
		t.Fatalf("Code = %d, want -10", rejected.Code)
	}

	// 业务码为 0 时不应误判，继续走正常解析（此处缺 f3，应报解析错误而非拒绝）
	zero := pbLen(1, pbVar(1, 0))
	if _, err := extractSession(zero); err == nil {
		t.Fatal("缺少会话块时应报解析错误")
	} else if _, isRejected := err.(*ManualAuthRejectedError); isRejected {
		t.Fatal("业务码为 0 不应被判为拒绝")
	}
}

// TestPhoneRequestUsesProvidedDevice 验证 builder 用的是调用方传入的设备标识，
// 而不是内部随机生成。
//
// 这是本项目「稳定设备标识」特性的落点：账号的 device_id 由 store 持久化，
// 一路传到 packet 里。上游是每次 randomSessDevice()，两者在这里分道——
// 如果哪天有人为了"对齐上游"把它改回随机，这条测试会立刻变红，
// 而不是等到线上发现同一账号的指纹在变才回头查。
func TestPhoneRequestUsesProvidedDevice(t *testing.T) {
	uin := int64(1)
	dev := []byte("02-AA-BB-CC-DD-01")
	other := []byte("02-AA-BB-CC-DD-02")

	first := buildPhoneRequest(uin, "wxappid", []byte(hostAppIDDefault), dev)
	second := buildPhoneRequest(uin, "wxappid", []byte(hostAppIDDefault), dev)
	third := buildPhoneRequest(uin, "wxappid", []byte(hostAppIDDefault), other)

	deviceOf := func(plain []byte) string {
		loginReq := pbParse(jsapiField(t, plain, 5))
		info, ok := loginReq[1].([]byte)
		if !ok {
			t.Fatal("loginReq 缺少 sessionInfo 字段")
		}
		raw, ok := pbParse(info)[3].([]byte)
		if !ok {
			t.Fatal("sessionInfo 缺少设备标识字段")
		}
		return string(raw)
	}

	if got := deviceOf(first); got != string(dev) {
		t.Fatalf("包内设备标识 = %q, want 传入值 %q", got, dev)
	}
	// 同一设备：两次构造必须完全一致（没有隐藏的随机量）
	if string(first) != string(second) {
		t.Fatal("相同入参应产出完全相同的报文，说明仍有随机成分")
	}
	// 不同设备：报文必须不同（证明确实读了这个参数）
	if string(first) == string(third) {
		t.Fatal("不同设备标识应产出不同报文")
	}
}
