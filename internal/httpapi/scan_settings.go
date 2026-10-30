package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

// scanDefaultKey 是「系统默认扫码渠道」在 settings 表里的键名。
//
// 上游还有一层「个人默认」，按用户 ID 存一份覆盖值。本项目是单管理员部署，
// 没有用户体系，所以只保留系统默认这一层——留着一层永远取不到的覆盖值
// 只会让人误以为设置没生效。
const scanDefaultKey = "scan_default_product"

// defaultScanProduct 读取系统默认扫码渠道，未配置或配置非法时回落到应用宝。
//
// 故意对非法值静默回落而不是报错：这个值只用于「没显式选择时选哪个」，
// 为了一个坏的默认值让扫码入口整体不可用，代价明显大于收益。
func (a *App) defaultScanProduct(ctx context.Context) string {
	value, err := a.db.GetSetting(ctx, scanDefaultKey)
	if err != nil {
		return loginProductAppStore
	}
	product, err := normalizeLoginProduct(value)
	if err != nil {
		return loginProductAppStore
	}
	return product
}

// handleScanSettings 读写系统默认扫码渠道。
//
// 只影响「没显式指定渠道」的扫码与新增链接；显式选择永远优先，
// 已存在账号的重新扫码与更新链接一律沿用该账号原渠道。
func (a *App) handleScanSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		value, err := a.db.GetSetting(r.Context(), scanDefaultKey)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		product := a.defaultScanProduct(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{
			"default_product": product,
			"configured":      strings.TrimSpace(value) != "",
			"options":         []string{loginProductAppStore, loginProductPCMgr},
		})
	case http.MethodPost:
		var body struct {
			DefaultProduct string `json:"default_product"`
		}
		if err := decodeOptionalJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		product, err := normalizeLoginProduct(body.DefaultProduct)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if err := a.db.SetSetting(r.Context(), scanDefaultKey, product); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"default_product": product, "configured": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// scanProductFor 决定一次「全新扫码」用哪个渠道：显式选择优先，其次系统默认。
func (a *App) scanProductFor(r *http.Request, explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return normalizeLoginProduct(explicit)
	}
	return a.defaultScanProduct(r.Context()), nil
}
