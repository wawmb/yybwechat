package httpapi

import (
	"log"
	"net/http"
	"strings"
)

// 本文件实现账号维护类接口：备注修改、残缺账号清理、ID 紧凑化与手动状态调整。

// accountRemarkIn 是 /accounts/remark 的请求体。
type accountRemarkIn struct {
	Ref    string `json:"ref"`
	Remark string `json:"remark"`
}

// accountStatusIn 是 /accounts/status 的请求体。
type accountStatusIn struct {
	Ref    string `json:"ref"`
	Status string `json:"status"`
}

// accountConfirmIn 是 /accounts/repair 与 /accounts/compact 的请求体。
// 两个接口都采用「先预览后确认」的两段式设计，confirm 为 false 时只返回影响范围。
type accountConfirmIn struct {
	Confirm bool `json:"confirm"`
}

// handleAccountRemark 设置或清除账号备注。
// 备注是纯本地字段，不影响协议调用，也不参与任何微信侧同步。
func (a *App) handleAccountRemark(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/accounts/remark" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body accountRemarkIn
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	acc, ok := a.resolveAccountRef(w, r, body.Ref)
	if !ok {
		return
	}
	remark := strings.TrimSpace(body.Remark)
	if len(remark) > 128 {
		writeError(w, http.StatusBadRequest, "remark must be at most 128 characters")
		return
	}
	updated, err := a.db.SetAccountRemark(r.Context(), acc.ID, remark)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated.Public())
}

// handleAccountRepair 清理扫码流程中断留下的残缺账号。
//
// 第一次调用（confirm=false）返回待清理清单，第二次（confirm=true）才真正删除。
// 确认阶段会重新查询一次残缺条件，避免预览之后账号已被补全而误删。
func (a *App) handleAccountRepair(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/accounts/repair" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body accountConfirmIn
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	candidates, err := a.db.ListIncompleteAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !body.Confirm {
		writeJSON(w, http.StatusOK, map[string]any{
			"candidates": candidates,
			"count":      len(candidates),
			"hint":       "确认清理请再次提交 {\"confirm\": true}",
		})
		return
	}
	ids := make([]int64, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ID)
	}
	deleted, err := a.db.DeleteIncompleteAccounts(r.Context(), ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("[maintenance] 已清理 %d 条残缺账号记录", deleted)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "requested": len(ids)})
}

// handleAccountCompact 将账号 ID 重排为连续的 1..N。
//
// ID 空洞会在反复增删账号后累积，导致账号引用（脚本里的 @1、@2）看起来断断续续。
// 紧凑化会同步更新会话表外键，但**不会**改写任何外部脚本，
// 因此执行后请同步调整脚本中硬编码的账号 ID。
func (a *App) handleAccountCompact(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/accounts/compact" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body accountConfirmIn
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if !body.Confirm {
		accounts, err := a.db.ListAccounts(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		ids := make([]int64, 0, len(accounts))
		gaps := 0
		for i, acc := range accounts {
			ids = append(ids, acc.ID)
			if acc.ID != int64(i+1) {
				gaps++
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"current_ids": ids,
			"gaps":        gaps,
			"will_change": gaps > 0,
			"hint":        "确认整理请再次提交 {\"confirm\": true}，执行后需同步调整脚本中的账号 ID",
		})
		return
	}
	mapping, err := a.db.CompactAccountIDs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(mapping) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"changed": 0, "mapping": map[string]int64{}})
		return
	}
	// 返回 old -> new 映射，便于调用方核对脚本中的账号引用
	out := make(map[string]int64, len(mapping))
	for oldID, newID := range mapping {
		out[itoa(oldID)] = newID
	}
	log.Printf("[maintenance] 账号 ID 已紧凑化，共调整 %d 个账号", len(mapping))
	writeJSON(w, http.StatusOK, map[string]any{"changed": len(mapping), "mapping": out})
}

// handleAccountStatus 手动设置账号状态，用于修正自动检测的误判。
// status 仅接受 alive 与 expired；置为 expired 时会同步清理该账号的会话。
func (a *App) handleAccountStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/accounts/status" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body accountStatusIn
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	body.Status = strings.ToLower(strings.TrimSpace(body.Status))
	if body.Status != "alive" && body.Status != "expired" {
		writeError(w, http.StatusBadRequest, "status must be alive or expired")
		return
	}
	acc, ok := a.resolveAccountRef(w, r, body.Ref)
	if !ok {
		return
	}
	oldStatus := accountStatus(acc)
	if err := a.db.SetAccountStatus(r.Context(), acc.ID, body.Status); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 标记为失效时清理会话，避免继续复用已不可用的登录态
	if body.Status == "expired" {
		a.invalidateAccountSessions(r.Context(), acc.ID)
		a.markKeepAliveRetry(acc.ID)
	} else {
		a.clearKeepAliveRetry(acc.ID)
	}
	if shouldNotifyStatusChange(oldStatus, body.Status) {
		log.Printf("[maintenance] 账号 %s (%s) 状态被手动调整: %s → %s",
			deref(acc.Nickname), acc.OpenID, oldStatus, body.Status)
		// 手动调整也要走「送达才记账」的规则：推送失败时留给后台补推，
		// 否则运营者以为已经通知过了，实际一条都没发出去。
		if a.notifyWebhook(acc, oldStatus, body.Status) {
			if err := a.db.MarkStatusNotified(r.Context(), acc.ID, body.Status); err != nil {
				log.Printf("[maintenance] 记录推送状态失败 (账号 %d): %v", acc.ID, err)
			}
		}
	} else if err := a.db.MarkStatusNotified(r.Context(), acc.ID, body.Status); err != nil {
		// 无需通知的调整（如状态未变化）直接对齐标记，避免被后台误判成待补推
		log.Printf("[maintenance] 记录状态失败 (账号 %d): %v", acc.ID, err)
	}
	updated, err := a.db.GetAccount(r.Context(), acc.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated.Public())
}
