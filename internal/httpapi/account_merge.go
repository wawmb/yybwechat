package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"

	"yybwechat/internal/store"
)

// accountMergePreviewKey 为一次合并预览生成不可伪造的指纹。
//
// 预览和确认是两次独立请求，中间可能被别的事情改变状态（保活刷新、重新扫码、
// 另一个浏览器窗口删了账号）。把两边的 ID / openid / login_buffer / 凭据一起
// 做 HMAC：确认时比对，只要有任何一项变了就要求重新预览，绝不在过期信息上动手。
func (a *App) accountMergePreviewKey(ctx context.Context, source, target *store.WechatAccount) string {
	mac := hmac.New(sha256.New, a.accountLinkKey(ctx))
	for _, acc := range []*store.WechatAccount{source, target} {
		// 凭据只参与计算，不出现在响应里
		encoded, _ := json.Marshal([]any{acc.ID, acc.OpenID, acc.LoginBuffer, acc.Credentials})
		mac.Write(encoded)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// handleAccountMerge 合并账号：把来源授权移入目标 ID，然后删除来源。
//
// 两步式（先 preview 再 confirm）不是为了多一次点击，而是因为「哪条记录是哪条」
// 在界面上很难一眼看准，误合并会直接丢掉一个账号的 ID 与配置。
func (a *App) handleAccountMerge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Source     string `json:"source_ref"`
		Target     string `json:"target_ref"`
		Confirm    bool   `json:"confirm"`
		PreviewKey string `json:"preview_key"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	source, ok := a.resolveAccountRef(w, r, body.Source)
	if !ok {
		return
	}
	target, ok := a.resolveAccountRef(w, r, body.Target)
	if !ok {
		return
	}
	if source.ID == target.ID {
		writeError(w, http.StatusUnprocessableEntity, "来源和保留 ID 不能相同")
		return
	}
	if source.LoginBuffer == "" || len(source.Credentials) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "来源账号授权不可用，请先扫码更新来源账号")
		return
	}
	if source.Status != nil && *source.Status == "expired" {
		writeError(w, http.StatusUnprocessableEntity, "来源账号已标记失效，请先扫码更新来源账号再合并")
		return
	}

	key := a.accountMergePreviewKey(r.Context(), source, target)
	if !body.Confirm {
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":        "preview",
			"preview_key": key,
			"source":      source.Public(),
			"target":      target.Public(),
			"note":        store.MergePreviewNote,
		})
		return
	}
	if !hmac.Equal([]byte(key), []byte(body.PreviewKey)) {
		writeError(w, http.StatusConflict, "账号授权已变化，请重新预览后确认")
		return
	}

	// 固定顺序取锁，避免两个方向相反的合并请求互相死锁。
	ids := []int64{source.ID, target.ID}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		lock := a.refreshLockFor(id)
		select {
		case lock <- struct{}{}:
			defer func() { <-lock }()
		case <-r.Context().Done():
			return
		}
	}

	merged, err := a.db.MergeAccountAuthorization(r.Context(), source, target)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrAccountAuthorizationChanged):
			writeError(w, http.StatusConflict, "账号授权已变化，请重新预览")
		default:
			log.Printf("[merge] 合并账号 %d → %d 失败: %v", source.ID, target.ID, err)
			writeError(w, http.StatusInternalServerError, "合并失败，账号未更改："+err.Error())
		}
		return
	}
	for _, id := range ids {
		a.invalidateProxyLease(id)
		a.clearKeepAliveRetry(id)
		a.clearCredentialRejections(id)
	}
	log.Printf("[merge] 已把账号 %d 的授权合并到账号 %d，来源 ID 已释放", source.ID, target.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":       "merged",
		"account":    merged.Public(),
		"removed_id": source.ID,
		"note": "原 ID 已保留，来源 ID " + strconv.FormatInt(source.ID, 10) +
			" 已释放。脚本或外部系统中硬编码了这个 ID 的地方需要自行调整。",
	})
}
