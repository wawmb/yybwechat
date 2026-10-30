package httpapi

import "net/http"

// requireAdmin 是本项目「单管理员」模型下的鉴权入口（用在 /logs 与 /api/system-logs）。
//
// 上游用它区分管理员与普通用户；我们没有用户体系，所以它只做一件事：
// 没登录就 401。任何"不做用户体系就把它放行"的写法都等于把维护接口公开。
func (a *App) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if a.isAuthenticated(r) {
		return true
	}
	writeError(w, http.StatusUnauthorized, "需要管理员登录")
	return false
}
