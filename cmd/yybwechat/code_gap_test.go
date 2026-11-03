package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// 熔断只对失败反应、登录预算只对 OAuth 调用反应，所以**一串成功的取码
// 能同时穿过两者**。请求间隔是唯一拦得住它的东西——默认值丢了就等于
// 悄悄把这个限制删掉了。
func TestCodeMinGapDefaultsToThreeSecondsAndCanBeSwitchedOff(t *testing.T) {
	if defaultCodeMinGap != 3*time.Second {
		t.Fatalf("默认间隔 = %s, want 3s", defaultCodeMinGap)
	}
	for _, off := range []string{"0", "off", "OFF"} {
		t.Setenv("YYB_CODE_MIN_GAP", off)
		if got := envDuration("YYB_CODE_MIN_GAP", defaultCodeMinGap); got != 0 {
			t.Fatalf("YYB_CODE_MIN_GAP=%q 得到 %s，应当关闭限速", off, got)
		}
	}
	t.Setenv("YYB_CODE_MIN_GAP", "5s")
	if got := envDuration("YYB_CODE_MIN_GAP", defaultCodeMinGap); got != 5*time.Second {
		t.Fatalf("间隔 = %s, want 5s", got)
	}
	// 解析失败必须退回默认值并打日志，不能静默变成 0（等于关闭限速）。
	t.Setenv("YYB_CODE_MIN_GAP", "garbage")
	if got := envDuration("YYB_CODE_MIN_GAP", defaultCodeMinGap); got != 3*time.Second {
		t.Fatalf("非法值得到 %s，应当退回 3s 默认值", got)
	}
}

// main 是内联组装的配置，所以这里核对源码接线：
// 服务必须以默认间隔启动，而不是以「限速关闭」启动。
func TestMainStartsWithTheDefaultCodeMinGap(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if !strings.Contains(text, "YYB_CODE_MIN_GAP") || !strings.Contains(text, defaultCodeMinGapName) {
		t.Fatal("main 不再用 defaultCodeMinGap 作为 YYB_CODE_MIN_GAP 的默认值")
	}
	if !strings.Contains(text, "CodeMinGap:") {
		t.Fatal("main 没有把限速间隔传进 Config")
	}
}

// defaultCodeMinGapName 让上一条断言不必依赖 gofmt 的对齐空格。
const defaultCodeMinGapName = "defaultCodeMinGap"
