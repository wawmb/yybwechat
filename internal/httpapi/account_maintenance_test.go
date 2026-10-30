package httpapi

import (
	"testing"

	"yybwechat/internal/store"
)

// 紧凑化预览要给「哪个号会变成几」的逐条对照，
// 只报空洞数量的话，调用方无从知道自己的脚本要改哪些引用。
func TestAccountCompactPlanListsOnlyChangingIDs(t *testing.T) {
	remark := "甲"
	accounts := []*store.WechatAccount{
		{ID: 1, OpenID: "a", Remark: &remark},
		{ID: 5, OpenID: "c"},
		{ID: 8, OpenID: "d", Nickname: strPtr("丁")},
	}
	items, mapping := accountCompactPlan(accounts)
	if len(items) != 2 {
		t.Fatalf("accountCompactPlan() items = %d, want 2: %#v", len(items), items)
	}
	// 新 ID 就是「列表里的第几个」，与空洞前后的数值无关
	if items[0].OldID != 5 || items[0].NewID != 2 {
		t.Fatalf("first change = %#v, want 5 -> 2", items[0])
	}
	if items[1].OldID != 8 || items[1].NewID != 3 {
		t.Fatalf("second change = %#v, want 8 -> 3", items[1])
	}
	// 没有备注/昵称时要有可读的占位，否则界面上是一行空白
	if items[0].Label != "未命名账号" {
		t.Fatalf("label = %q, want 未命名账号", items[0].Label)
	}
	if items[1].Label != "丁" {
		t.Fatalf("label = %q, want 丁", items[1].Label)
	}
	// 已经连续的 ID 不该出现在 mapping 里
	if _, ok := mapping[1]; ok {
		t.Fatalf("unchanged account 1 appeared in mapping: %#v", mapping)
	}
	if mapping[5] != 2 || mapping[8] != 3 {
		t.Fatalf("mapping = %#v, want 5->2 8->3", mapping)
	}
}

// 列表里混进 nil 不能 panic，也不能算出一个假的 ID。
func TestAccountCompactPlanSkipsNilAccounts(t *testing.T) {
	items, _ := accountCompactPlan([]*store.WechatAccount{nil, {ID: 9, OpenID: "z"}})
	if len(items) != 1 || items[0].OldID != 9 || items[0].NewID != 2 {
		t.Fatalf("accountCompactPlan() with nil = %#v", items)
	}
}

func strPtr(v string) *string { return &v }
