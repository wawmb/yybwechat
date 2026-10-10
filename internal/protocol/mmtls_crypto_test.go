package protocol

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

// -----------------------------------------------------------------------------
// ECDH 密钥协商
// -----------------------------------------------------------------------------

// TestECDHSharedSecretIsSymmetric 是整个 MMTLS 握手的正确性基石：
// 客户端与服务端各自用「自己的私钥 + 对方的公钥」计算共享密钥，两边必须得到
// 完全相同的结果。一旦这里不对称，握手必然失败，而且失败点会远离真正的根因。
func TestECDHSharedSecretIsSymmetric(t *testing.T) {
	for i := 0; i < 8; i++ {
		privClient, pubClient, err := generateECDH()
		if err != nil {
			t.Fatalf("生成客户端密钥对失败: %v", err)
		}
		privServer, pubServer, err := generateECDH()
		if err != nil {
			t.Fatalf("生成服务端密钥对失败: %v", err)
		}

		fromClient, err := mmtlsSecret(privClient, pubServer)
		if err != nil {
			t.Fatalf("客户端侧计算共享密钥失败: %v", err)
		}
		fromServer, err := mmtlsSecret(privServer, pubClient)
		if err != nil {
			t.Fatalf("服务端侧计算共享密钥失败: %v", err)
		}

		if !bytes.Equal(fromClient, fromServer) {
			t.Fatal("双方算出的共享密钥不一致，MMTLS 握手不可能成功")
		}
		if len(fromClient) != sha256.Size {
			t.Fatalf("共享密钥应为 SHA-256 摘要（32 字节），实际 %d 字节", len(fromClient))
		}
		if bytes.Equal(fromClient, make([]byte, sha256.Size)) {
			t.Fatal("共享密钥不应为全零")
		}
	}
}

// TestGenerateECDHPublicKeysDiffer 确认每次生成的密钥对互不相同，
// 避免随机数源失效导致所有连接复用同一密钥。
func TestGenerateECDHPublicKeysDiffer(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		_, pub, err := generateECDH()
		if err != nil {
			t.Fatalf("生成密钥对失败: %v", err)
		}
		// P-256 未压缩公钥固定 65 字节，首字节为 0x04
		if len(pub) != 65 || pub[0] != 0x04 {
			t.Fatalf("公钥应为 65 字节的未压缩格式，实际 %d 字节、首字节 %#x", len(pub), pub[0])
		}
		seen[string(pub)] = true
	}
	if len(seen) != 16 {
		t.Fatalf("16 次生成只得到 %d 个不同公钥，随机源可能失效", len(seen))
	}
}

// TestMmtlsSecretRejectsInvalidPeerPublicKey 确认损坏的公钥返回错误而不是
// 静默算出一个错误的共享密钥。
func TestMmtlsSecretRejectsInvalidPeerPublicKey(t *testing.T) {
	priv, _, err := generateECDH()
	if err != nil {
		t.Fatalf("生成密钥对失败: %v", err)
	}
	cases := map[string][]byte{
		"空公钥":     {},
		"长度不足":    {0x04, 0x01, 0x02},
		"格式标记错误":  append([]byte{0x05}, make([]byte, 64)...),
		"坐标不在曲线上": append([]byte{0x04}, make([]byte, 64)...),
	}
	for name, pub := range cases {
		if _, err := mmtlsSecret(priv, pub); err == nil {
			t.Fatalf("%s 应当返回错误，实际接受了无效公钥", name)
		}
	}
}

// -----------------------------------------------------------------------------
// AES-GCM 记录加密
// -----------------------------------------------------------------------------

func testKeyAndIV() ([]byte, []byte) {
	key := make([]byte, aesKeyLen)
	iv := make([]byte, aesIVLen)
	for i := range key {
		key[i] = byte(i + 1)
	}
	for i := range iv {
		iv[i] = byte(i + 0x40)
	}
	return key, iv
}

// TestGCMRoundTrip 验证加解密往返，并确认密文长度比明文多出一个 GCM 认证标签。
func TestGCMRoundTrip(t *testing.T) {
	key, iv := testKeyAndIV()
	aad := buildRecordAAD(7, 0x17, 32)

	plaintext := []byte("mmtls handshake payload")
	ct, err := gcmEncrypt(key, iv, 7, plaintext, aad)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if len(ct) != len(plaintext)+gcmTagLen {
		t.Fatalf("密文长度应为明文 + 16 字节标签，实际明文 %d、密文 %d", len(plaintext), len(ct))
	}
	got, err := gcmDecrypt(key, iv, 7, ct, aad)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("加解密往返不一致: %q != %q", got, plaintext)
	}
}

// TestGCMRejectsTampering 是安全性的核心保证：任何篡改都必须被认证标签检出，
// 绝不能解出一段看似正常但实际错误的数据。
func TestGCMRejectsTampering(t *testing.T) {
	key, iv := testKeyAndIV()
	plaintext := []byte("transfer this payload")
	aad := buildRecordAAD(3, 0x17, len(plaintext)+gcmTagLen)
	ct, err := gcmEncrypt(key, iv, 3, plaintext, aad)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}

	t.Run("密文被篡改", func(t *testing.T) {
		bad := append([]byte(nil), ct...)
		bad[0] ^= 0x01
		if _, err := gcmDecrypt(key, iv, 3, bad, aad); err == nil {
			t.Fatal("篡改后的密文不应通过认证")
		}
	})
	t.Run("AAD 被篡改", func(t *testing.T) {
		badAAD := append([]byte(nil), aad...)
		badAAD[8] = 0x16 // 换一个 contentType
		if _, err := gcmDecrypt(key, iv, 3, ct, badAAD); err == nil {
			t.Fatal("AAD 不一致时不应通过认证，否则记录类型可被伪造")
		}
	})
	t.Run("序列号被调换", func(t *testing.T) {
		// 序列号参与 nonce 计算，用错序列号等同于重放攻击
		if _, err := gcmDecrypt(key, iv, 4, ct, aad); err == nil {
			t.Fatal("序列号不一致时不应通过认证，否则无法防御重放")
		}
	})
	t.Run("密钥被调换", func(t *testing.T) {
		other := make([]byte, aesKeyLen)
		other[0] = 0xff
		if _, err := gcmDecrypt(other, iv, 3, ct, aad); err == nil {
			t.Fatal("密钥错误时不应通过认证")
		}
	})
}

// TestNonceOnlyMutatesTail 确认 nonce 由 IV 尾部 8 字节与序列号异或得到，
// 前缀保持不变——这是 MMTLS 与标准 TLS 1.3 nonce 构造的差异点。
func TestNonceOnlyMutatesTail(t *testing.T) {
	_, iv := testKeyAndIV()
	base := nonce(iv, 0)
	if len(base) != len(iv) {
		t.Fatalf("nonce 长度应与 IV 一致（%d），实际 %d", len(iv), len(base))
	}
	if !bytes.Equal(base, iv) {
		t.Fatal("序列号 0 时 nonce 应等于原始 IV")
	}
	if !bytes.Equal(base[:len(iv)-8], iv[:len(iv)-8]) {
		t.Fatal("nonce 的前缀不应被序列号影响")
	}

	seen := map[string]bool{}
	for seq := uint64(0); seq < 64; seq++ {
		n := nonce(iv, seq)
		if len(n) != len(iv) {
			t.Fatalf("序列号 %d 的 nonce 长度错误: %d", seq, len(n))
		}
		seen[string(n)] = true
	}
	if len(seen) != 64 {
		t.Fatalf("64 个不同序列号只产生 %d 个 nonce，存在碰撞", len(seen))
	}
}

// TestBuildRecordAAD 确认 AAD 的字段布局：8 字节序列号 + 1 字节内容类型 +
// 2 字节固定值 + 2 字节密文长度。
func TestBuildRecordAAD(t *testing.T) {
	aad := buildRecordAAD(0x0102030405060708, 0x17, 0x1234)
	if len(aad) != 13 {
		t.Fatalf("AAD 应为 13 字节，实际 %d", len(aad))
	}
	if !bytes.Equal(aad[0:8], []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("AAD 前 8 字节应为大端序列号，实际 % x", aad[0:8])
	}
	if aad[8] != 0x17 {
		t.Fatalf("AAD 第 9 字节应为内容类型，实际 %#x", aad[8])
	}
	if aad[9] != 0xf1 || aad[10] != 0x03 {
		t.Fatalf("AAD 第 10-11 字节应为固定值 f103，实际 %#x %#x", aad[9], aad[10])
	}
	if !bytes.Equal(aad[11:13], []byte{0x12, 0x34}) {
		t.Fatalf("AAD 末 2 字节应为大端密文长度，实际 % x", aad[11:13])
	}
}

// TestRecordPayloadRoundTrip 覆盖记录层的封装：AAD 由函数内部自行构造，
// 加密侧用「明文长度 + 标签」、解密侧用「密文长度」，两者必须一致。
func TestRecordPayloadRoundTrip(t *testing.T) {
	key, iv := testKeyAndIV()
	for seq := uint64(0); seq < 4; seq++ {
		plaintext := bytes.Repeat([]byte("record"), 3+int(seq))
		ct, err := encryptRecordPayload(key, iv, seq, 0x17, plaintext)
		if err != nil {
			t.Fatalf("序列号 %d 加密失败: %v", seq, err)
		}
		got, err := decryptRecordPayload(key, iv, seq, 0x17, ct)
		if err != nil {
			t.Fatalf("序列号 %d 解密失败: %v", seq, err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("序列号 %d 往返不一致", seq)
		}
	}
}

// -----------------------------------------------------------------------------
// 密钥派生
// -----------------------------------------------------------------------------

// TestHKDFIsDeterministic 确认相同输入必然得到相同输出——握手双方正是靠这一点
// 各自独立地派生出同一套密钥。
func TestHKDFIsDeterministic(t *testing.T) {
	prk := bytes.Repeat([]byte{0xab}, 32)
	info := []byte("handshake key expansion")
	first := hkdfExpand(prk, info, 56)
	second := hkdfExpand(prk, info, 56)
	if !bytes.Equal(first, second) {
		t.Fatal("相同输入的 HKDF 展开结果不一致，双方无法派生出同一套密钥")
	}
	if len(first) != 56 {
		t.Fatalf("展开结果应为 56 字节，实际 %d", len(first))
	}
	if bytes.Equal(first, make([]byte, 56)) {
		t.Fatal("展开结果不应为全零")
	}
}

// TestHKDFExpandLength 确认短输出是长输出的前缀（HKDF-Expand 的分块特性）。
func TestHKDFExpandLength(t *testing.T) {
	prk := bytes.Repeat([]byte{0x11}, 32)
	info := []byte("prefix check")
	long := hkdfExpand(prk, info, 64)
	short := hkdfExpand(prk, info, 16)
	if !bytes.Equal(short, long[:16]) {
		t.Fatal("短输出应是长输出的前缀")
	}
}

// TestSplitBiKeys 确认 56 字节密钥块被切成客户端/服务端各一份，
// 且各段之间不共享底层数组（避免后续就地修改互相污染）。
func TestSplitBiKeys(t *testing.T) {
	kb := make([]byte, 56)
	for i := range kb {
		kb[i] = byte(i)
	}
	tk := splitBiKeys(kb)

	if len(tk.ClientKey) != 16 || len(tk.ServerKey) != 16 {
		t.Fatalf("双向密钥各应 16 字节，实际 %d / %d", len(tk.ClientKey), len(tk.ServerKey))
	}
	if len(tk.ClientIV) != 12 || len(tk.ServerIV) != 12 {
		t.Fatalf("双向 IV 各应 12 字节，实际 %d / %d", len(tk.ClientIV), len(tk.ServerIV))
	}
	if !bytes.Equal(tk.ClientKey, kb[0:16]) || !bytes.Equal(tk.ServerKey, kb[16:32]) {
		t.Fatal("密钥切片位置不正确")
	}
	if !bytes.Equal(tk.ClientIV, kb[32:44]) || !bytes.Equal(tk.ServerIV, kb[44:56]) {
		t.Fatal("IV 切片位置不正确")
	}

	// 就地修改返回值的第一段，不应影响其它段
	tk.ClientKey[0] = 0xff
	if kb[0] == 0xff {
		t.Fatal("splitBiKeys 返回的切片与输入共享底层数组，存在互相污染风险")
	}
	if tk.ServerKey[0] == 0xff {
		t.Fatal("修改客户端密钥不应影响服务端密钥")
	}
}
