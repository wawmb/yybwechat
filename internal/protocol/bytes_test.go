package protocol

import (
	"bytes"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------------
// varint 编解码
// -----------------------------------------------------------------------------

// TestVarintRoundTrip 覆盖单字节到十字节的全部边界：0、127/128（单字节上限）、
// 16383/16384（双字节上限）以及 uint64 最大值。
func TestVarintRoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 2, 127, 128, 129, 300, 16383, 16384, 1 << 20, 1 << 40, ^uint64(0)}
	for _, n := range cases {
		enc := varint(n)
		got, next, err := readVarint(enc, 0)
		if err != nil {
			t.Fatalf("varint(%d) 编码后无法读回: %v", n, err)
		}
		if got != n {
			t.Fatalf("varint 往返不一致: 写入 %d, 读回 %d", n, got)
		}
		if next != len(enc) {
			t.Fatalf("varint(%d) 读取后偏移错误: 期望 %d, 实际 %d", n, len(enc), next)
		}
	}
}

// TestReadVarintErrors 确认损坏输入会返回错误而不是被静默解析成一个错误的值。
func TestReadVarintErrors(t *testing.T) {
	t.Run("高位 continuation 后无后续字节", func(t *testing.T) {
		// 0x80 表示「后面还有字节」，但数据已经结束
		if _, _, err := readVarint([]byte{0x80}, 0); err == nil {
			t.Fatal("截断的 varint 应当返回错误")
		}
	})
	t.Run("continuation 字节过多导致溢出", func(t *testing.T) {
		// 10 个 continuation 字节会把 shift 推到 70，超过 63 的上限
		if _, _, err := readVarint(bytes.Repeat([]byte{0x80}, 10), 0); err == nil {
			t.Fatal("超过 64 位的 varint 应当返回溢出错误")
		}
	})
	t.Run("起始偏移越界", func(t *testing.T) {
		if _, _, err := readVarint([]byte{0x01}, 5); err == nil {
			t.Fatal("起始偏移超出数据长度时应当返回错误")
		}
	})
}

// -----------------------------------------------------------------------------
// protobuf 编解码
// -----------------------------------------------------------------------------

// TestPbParseRoundTrip 用 pbLen / pbVar 写、用 pbParse 读，验证往返一致。
func TestPbParseRoundTrip(t *testing.T) {
	payload := []byte("wx141bfb9b73c970a9")
	out := pbLen(1, payload)
	out = append(out, pbVar(2, 1)...)
	out = append(out, pbVar(3, 300)...)

	fields := pbParse(out)
	got, ok := fields[1].([]byte)
	if !ok || !bytes.Equal(got, payload) {
		t.Fatalf("字段 1 往返不一致: %#v", fields[1])
	}
	if v := fields[2]; v != int64(1) {
		t.Fatalf("字段 2 期望 int64(1)，实际 %#v", v)
	}
	if v := fields[3]; v != int64(300) {
		t.Fatalf("字段 3 期望 int64(300)，实际 %#v", v)
	}
}

// TestPbParseStopsAtGarbage 确认遇到无法解析的内容时不会 panic，
// 而是返回已解析出的部分（调用方据此判断完整性）。
func TestPbParseStopsAtGarbage(t *testing.T) {
	valid := pbVar(2, 1)
	// 在合法字段后追加一个 wire type 5（不支持）的字段
	garbage := append(append([]byte(nil), valid...), byte((3<<3)|5))
	fields := pbParse(garbage)
	if v, ok := fields[2]; !ok || v != int64(1) {
		t.Fatalf("遇到不支持的 wire type 时，已解析字段应当保留，实际 %#v", fields)
	}
	if _, ok := fields[3]; ok {
		t.Fatal("不支持的 wire type 不应被解析出值")
	}
}

// TestProtobufToMap 覆盖字段取值的四条分支。
//
// 这里有个容易踩坑的启发式：**只有内容不是有效 UTF-8 时才会尝试递归解析**。
// 也就是说，一个完全由 ASCII 组成的嵌套 protobuf 不会被视为嵌套结构，而会直接
// 退化成十六进制（因为它的 tag 字节是控制字符，isPrintableShort 判定失败）。
// 微信协议里的嵌套结构通常都带有二进制字段，所以这条启发式在实践中能工作，
// 但它不是通用的 protobuf 解析——改动这里前请先确认协议实际形态。
func TestProtobufToMap(t *testing.T) {
	// 分支一：可打印字符串，直接作为字符串返回
	outer := pbLen(1, []byte("nickname"))
	// 分支二：非 UTF-8（0xc8 不是合法 UTF-8 起始字节）且能解析出字段 → 递归成嵌套结构
	outer = append(outer, pbLen(2, pbVar(1, 200))...)
	// 分支三：非 UTF-8 但递归后解析不出任何字段 → 退化成 __bytes__
	outer = append(outer, pbLen(3, []byte{0xff, 0xfe})...)
	// 分支四：有效 UTF-8 但含控制字符 → 同样退化成 __bytes__，不会递归
	outer = append(outer, pbLen(4, pbLen(1, []byte("wxid")))...)

	m := protobufToMap(outer)

	if v, ok := m["1"].(string); !ok || v != "nickname" {
		t.Fatalf("字段 1 期望字符串 nickname，实际 %#v", m["1"])
	}

	sub, ok := m["2"].(map[string]any)
	if !ok {
		t.Fatalf("字段 2 期望嵌套结构，实际 %#v", m["2"])
	}
	if sub["1"] != uint64(200) {
		t.Fatalf("嵌套字段 1 期望 uint64(200)，实际 %#v", sub["1"])
	}

	if raw, ok := m["3"].(map[string]any); !ok || raw["__bytes__"] != "fffe" {
		t.Fatalf("字段 3 期望 __bytes__=fffe，实际 %#v", m["3"])
	}

	// 这条断言记录了「纯 ASCII 嵌套不会被递归」的行为：如果哪天改了解析策略，
	// 这里会立刻失败，提醒重新评估对微信协议的影响。
	if raw, ok := m["4"].(map[string]any); !ok || raw["__bytes__"] != "0a0477786964" {
		t.Fatalf("字段 4 期望 __bytes__=0a0477786964，实际 %#v", m["4"])
	}
}

// TestIsPrintableShort 覆盖判定规则的几个分支。
func TestIsPrintableShort(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"普通 ASCII", "hello-world", true},
		{"含制表符与换行仍算可打印", "a\tb\r\nc", true},
		{"含控制字符", "a\x01b", false},
		{"含非 ASCII 字节", "café", false},
		{"以 { 开头的 JSON 即使超长也算可打印", `{"k":1}`, true},
		{"超长文本不算可打印", strings.Repeat("a", 200), false},
		{"空字符串算可打印", "", true},
	}
	for _, c := range cases {
		if got := isPrintableShort(c.in); got != c.want {
			t.Fatalf("%s: 期望 %v，实际 %v", c.name, c.want, got)
		}
	}
}

func TestSafeBytes(t *testing.T) {
	if got := safeBytes([]byte("ab")); !bytes.Equal(got, []byte("ab")) {
		t.Fatalf("[]byte 应原样返回，实际 %#v", got)
	}
	if got := safeBytes("ab"); !bytes.Equal(got, []byte("ab")) {
		t.Fatalf("string 应转成 []byte，实际 %#v", got)
	}
	if got := safeBytes(123); got != nil {
		t.Fatalf("非字节类型应返回 nil，实际 %#v", got)
	}
}

// -----------------------------------------------------------------------------
// LZ4 全字面量块
// -----------------------------------------------------------------------------

// TestLZ4RoundTrip 覆盖字面量长度编码的三个分支：n<15 直接编码、
// n==15 走一次扩展、n>15 需要多个 255 续字节。
func TestLZ4RoundTrip(t *testing.T) {
	sizes := []int{0, 1, 5, 14, 15, 16, 17, 254, 255, 256, 270, 1000}
	for _, n := range sizes {
		src := make([]byte, n)
		for i := range src {
			src[i] = byte(i % 251)
		}
		packed := lz4AllLiteral(src)
		got, err := lz4Decompress(packed)
		if err != nil {
			t.Fatalf("长度 %d 解压缩失败: %v", n, err)
		}
		if !bytes.Equal(got, src) {
			t.Fatalf("长度 %d 往返不一致（解出 %d 字节）", n, len(got))
		}
	}
}

// TestLZ4DecompressRejectsTruncated 确认损坏数据返回错误而不是解出一段错内容。
func TestLZ4DecompressRejectsTruncated(t *testing.T) {
	src := bytes.Repeat([]byte("x"), 100)
	packed := lz4AllLiteral(src)

	t.Run("截断字面量长度", func(t *testing.T) {
		// 只保留 token 0xf0，后面的续字节被截掉
		if _, err := lz4Decompress(packed[:1]); err == nil {
			t.Fatal("字面量长度被截断时应当返回错误")
		}
	})
	t.Run("声明长度超过实际数据", func(t *testing.T) {
		bad := append([]byte(nil), packed...)
		bad = bad[:len(bad)-10]
		if _, err := lz4Decompress(bad); err == nil {
			t.Fatal("字面量数据被截断时应当返回错误")
		}
	})
	t.Run("空输入", func(t *testing.T) {
		got, err := lz4Decompress(nil)
		if err != nil {
			t.Fatalf("空输入应当解出空结果而非报错: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("空输入应解出 0 字节，实际 %d", len(got))
		}
	})
}
