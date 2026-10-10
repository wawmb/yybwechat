package httpapi

// newOpenAPISpec 构建并返回完整的 OpenAPI 3.0.3 规范文档。
// 该文档描述了所有 API 端点的请求/响应格式，供 Swagger UI 渲染和外部工具使用。
func newOpenAPISpec() map[string]any {
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "yybwechat 接口文档",
			"description": "用于微信扫码登录、账号管理和 wxapp 接口调用的 API。",
			"version":     "1.0.0",
		},
		"servers": []map[string]any{
			{"url": "/"},
		},
		"tags": []map[string]any{
			{"name": "health", "description": "服务健康检查"},
			{"name": "qr", "description": "微信扫码登录"},
			{"name": "accounts", "description": "已保存的微信账号"},
			{"name": "wxapp", "description": "wxapp 业务接口调用"},
			{"name": "settings", "description": "系统设置"},
			{"name": "auth", "description": "管理员认证与会话"},
			{"name": "system", "description": "服务元信息"},
		},
		"paths": map[string]any{
			"/health": map[string]any{
				"get": openAPIOperation(
					[]string{"health"},
					"检查服务状态",
					nil,
					nil,
					defaulted(map[string]any{
						"200": jsonResponse("服务正常。", refSchema("HealthResponse")),
					}),
				),
			},
			"/qr": map[string]any{
				"post": openAPIOperation(
					[]string{"qr"},
					"创建扫码登录会话",
					[]map[string]any{
						boolQueryParam("as_base64", "是否同时返回二维码图片的 data URI。"),
					},
					nil,
					defaulted(map[string]any{
						"200": jsonResponse("二维码会话创建成功。", refSchema("QRCreateResponse")),
					}),
				),
			},
			"/qr/{session_id}/image": map[string]any{
				"get": openAPIOperation(
					[]string{"qr"},
					"获取二维码图片",
					[]map[string]any{pathStringParam("session_id", "二维码会话 ID。")},
					nil,
					defaulted(map[string]any{
						"200": imageResponse("二维码图片。"),
					}),
				),
			},
			"/qr/{session_id}/poll": map[string]any{
				"get": openAPIOperation(
					[]string{"qr"},
					"轮询扫码登录状态",
					[]map[string]any{pathStringParam("session_id", "二维码会话 ID。")},
					nil,
					defaulted(map[string]any{
						"200": jsonResponse("当前扫码状态。", refSchema("QRPollResponse")),
					}),
				),
			},
			"/qr/{session_id}/confirm": map[string]any{
				"post": openAPIOperation(
					[]string{"qr"},
					"确认已授权的扫码会话并保存账号",
					[]map[string]any{pathStringParam("session_id", "二维码会话 ID。")},
					nil,
					defaulted(map[string]any{
						"200": jsonResponse("已保存的账号信息。", refSchema("AccountPublic")),
					}),
				),
			},
			"/accounts": map[string]any{
				"get": openAPIOperation(
					[]string{"accounts"},
					"获取账号列表（公开只读，免认证且无需令牌，供自动化脚本探测服务与挑选在线账号）。返回脱敏视图 AccountPublic，不含 login_buffer 与 credentials",
					nil,
					nil,
					defaulted(map[string]any{
						"200": jsonResponse("已保存的账号列表。", arraySchema(refSchema("AccountPublic"))),
					}),
				),
				"delete": openAPIOperation(
					[]string{"accounts"},
					"删除账号（需管理员登录：本接口虽与免认证的 GET 共用路径，但写操作只认管理员会话）",
					[]map[string]any{queryStringParam("ref", "账号 ID、UIN 或 openid。", true)},
					nil,
					defaulted(map[string]any{
						"200": jsonResponse("删除结果。", refSchema("DeleteAccountResponse")),
					}),
				),
			},
			"/accounts/refresh": map[string]any{
				"post": openAPIOperation(
					[]string{"accounts"},
					"刷新单个或全部账号的存活状态 — 传 ref 刷新指定账号，不传则批量刷新全部。",
					nil,
					jsonOptionalRequestBody(refSchema("AccountRefRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("传 ref 时返回单个 RefreshResult，不传时返回 RefreshResult 数组。", refSchema("RefreshResponse")),
					}),
				),
			},
			"/accounts/resync": map[string]any{
				"post": openAPIOperation(
					[]string{"accounts"},
					"重新同步单个或全部账号资料 — 传 ref 同步指定账号，不传则批量同步全部。",
					nil,
					jsonOptionalRequestBody(refSchema("AccountRefRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("传 ref 时返回单个 AccountPublic，不传时返回 AccountPublic 数组。", refSchema("ResyncResponse")),
					}),
				),
			},
			"/accounts/avatar": map[string]any{
				"get": openAPIOperation(
					[]string{"accounts"},
					"获取账号头像",
					[]map[string]any{queryStringParam("ref", "账号 ID、UIN 或 openid。", true)},
					nil,
					defaulted(map[string]any{
						"200": imageResponse("头像图片。"),
						"302": map[string]any{"description": "跳转到远程头像地址。"},
					}),
				),
			},
			"/wxapp/getCode": map[string]any{
				"post": openAPIOperation(
					[]string{"wxapp"},
					"获取小程序code",
					nil,
					jsonRequestBody(refSchema("WxappRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("getCode 调用结果。", refSchema("WxappResponse")),
					}),
				),
			},
			"/wxapp/getPhoneNumber": map[string]any{
				"post": openAPIOperation(
					[]string{"wxapp"},
					"获取手机号",
					nil,
					jsonRequestBody(refSchema("WxappRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("getPhoneNumber 调用结果。", refSchema("WxappResponse")),
					}),
				),
			},
			"/wxapp/operateWxData": map[string]any{
				"post": openAPIOperation(
					[]string{"wxapp"},
					"调用 operateWxData 接口（微信小程序数据操作）",
					nil,
					jsonRequestBody(refSchema("OperateWXDataRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("operateWxData 调用结果。", refSchema("WxappResponse")),
					}),
				),
			},
			"/settings/webhook": map[string]any{
				"get": openAPIOperation(
					[]string{"settings"},
					"读取 Webhook 地址",
					nil,
					nil,
					defaulted(map[string]any{
						"200": jsonResponse("当前 Webhook 配置。", refSchema("WebhookGetResponse")),
					}),
				),
				"put": openAPIOperation(
					[]string{"settings"},
					"设置 Webhook 地址",
					nil,
					jsonRequestBody(refSchema("WebhookPutRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("更新后的 Webhook 配置。", refSchema("WebhookGetResponse")),
					}),
				),
			},
			"/settings/webhook/test": map[string]any{
				"post": openAPIOperation(
					[]string{"settings"},
					"测试 Webhook 推送（由服务端发送测试请求，避免浏览器 CORS 限制）",
					nil,
					jsonRequestBody(refSchema("WebhookPutRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("测试推送成功。", refSchema("WebhookTestResponse")),
					}),
				),
			},
			"/wxapp/getUserInfo": map[string]any{
				"get": openAPIOperation(
					[]string{"wxapp"},
					"获取微信用户信息（走 HTTPS 通道，无需建立 MMTLS 会话）",
					[]map[string]any{queryStringParam("ref", "账号 ID、UIN 或 openid。", true)},
					nil,
					defaulted(map[string]any{
						"200": jsonResponse("用户信息。", refSchema("WxappResponse")),
					}),
				),
				"post": openAPIOperation(
					[]string{"wxapp"},
					"获取微信用户信息（POST 形式，ref 放请求体）",
					nil,
					jsonOptionalRequestBody(refSchema("AccountRefRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("用户信息。", refSchema("WxappResponse")),
					}),
				),
			},
			"/wxapp/oauth": map[string]any{
				"post": openAPIOperation(
					[]string{"wxapp"},
					"构造公众号网页授权 URL — 只生成链接，code 需用户在微信内完成授权后由回调携带",
					nil,
					jsonRequestBody(refSchema("OAuthRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("授权链接。", refSchema("OAuthResponse")),
					}),
				),
			},
			"/accounts/remark": map[string]any{
				"post": openAPIOperation(
					[]string{"accounts"},
					"设置或清除账号备注（本地字段，不参与微信侧同步）",
					nil,
					jsonRequestBody(refSchema("AccountRemarkRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("更新后的账号。", refSchema("AccountPublic")),
					}),
				),
			},
			"/accounts/status": map[string]any{
				"post": openAPIOperation(
					[]string{"accounts"},
					"手动设置账号状态（alive 或 expired），用于修正自动检测的误判",
					nil,
					jsonRequestBody(refSchema("AccountStatusRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("更新后的账号。", refSchema("AccountPublic")),
					}),
				),
			},
			"/accounts/repair": map[string]any{
				"post": openAPIOperation(
					[]string{"accounts"},
					"清理扫码中断留下的残缺账号 — confirm=false 仅预览，confirm=true 才删除",
					nil,
					jsonRequestBody(refSchema("AccountConfirmRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("预览清单或清理结果。", freeFormObjectSchema("confirm=false 返回 candidates 与 count；confirm=true 返回 deleted。")),
					}),
				),
			},
			"/accounts/compact": map[string]any{
				"post": openAPIOperation(
					[]string{"accounts"},
					"将账号 ID 重排为连续的 1..N — confirm=false 仅预览，confirm=true 才执行",
					nil,
					jsonRequestBody(refSchema("AccountConfirmRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("预览信息或重排映射。", freeFormObjectSchema("confirm=false 返回 current_ids 与 gaps；confirm=true 返回 changed 与 mapping。")),
					}),
				),
			},
			"/api/auth/status": map[string]any{
				"get": openAPIOperation(
					[]string{"auth"},
					"查询管理员初始化状态与当前登录状态（公开接口）",
					nil,
					nil,
					defaulted(map[string]any{
						"200": jsonResponse("认证状态。", refSchema("AuthStatusResponse")),
					}),
				),
			},
			"/api/auth/setup": map[string]any{
				"post": openAPIOperation(
					[]string{"auth"},
					"首次启动创建管理员 — 系统只允许存在一个管理员，已存在时返回 409",
					nil,
					jsonRequestBody(refSchema("AuthCredentialRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("创建成功并自动登录。", refSchema("AuthLoginResponse")),
					}),
				),
			},
			"/api/auth/login": map[string]any{
				"post": openAPIOperation(
					[]string{"auth"},
					"管理员登录，成功后下发会话 Cookie（同一 IP 连续失败 8 次将限流 15 分钟）",
					nil,
					jsonRequestBody(refSchema("AuthCredentialRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("登录成功。", refSchema("AuthLoginResponse")),
					}),
				),
			},
			"/api/auth/logout": map[string]any{
				"post": openAPIOperation(
					[]string{"auth"},
					"注销当前会话并清除 Cookie",
					nil,
					nil,
					defaulted(map[string]any{
						"200": jsonResponse("登出结果。", freeFormObjectSchema("ok 表示已登出。")),
					}),
				),
			},
			"/api/auth/me": map[string]any{
				"get": openAPIOperation(
					[]string{"auth"},
					"查询当前登录管理员信息与版本（与 /api/auth/status 等价）",
					nil,
					nil,
					defaulted(map[string]any{
						"200": jsonResponse("认证状态。", refSchema("AuthStatusResponse")),
					}),
				),
			},
			"/api/auth/password": map[string]any{
				"put": openAPIOperation(
					[]string{"auth"},
					"修改管理员密码（需登录），成功后其它设备的会话会被下线",
					nil,
					jsonRequestBody(refSchema("AuthPasswordRequest")),
					defaulted(map[string]any{
						"200": jsonResponse("修改结果。", freeFormObjectSchema("ok 表示已修改。")),
					}),
				),
			},
			"/api/version": map[string]any{
				"get": openAPIOperation(
					[]string{"system"},
					"获取服务构建元数据（需登录）",
					nil,
					nil,
					defaulted(map[string]any{
						"200": jsonResponse("版本信息。", refSchema("VersionResponse")),
					}),
				),
			},
		},
		"components": map[string]any{
			"schemas": map[string]any{
				"APIResponse": objectSchema([]string{"code", "msg", "data"}, map[string]any{
					"code": map[string]any{"type": "integer", "example": 0, "description": "业务状态码，0 表示成功，非 0 表示业务错误。"},
					"msg":  map[string]any{"type": "string", "example": "success", "description": "提示信息，前端可直接用于 Toast 提示。"},
					"data": nullableObjectSchema("实际数据载荷，可以是对象、数组或 null。"),
				}),
				"APIErrorResponse": objectSchema([]string{"code", "msg", "data"}, map[string]any{
					"code": map[string]any{"type": "integer", "example": 400, "description": "非 0 业务错误码。"},
					"msg":  map[string]any{"type": "string", "example": "ref is required"},
					"data": nullableObjectSchema("错误响应当前固定返回 null。"),
				}),
				"HealthResponse": objectSchema([]string{"ok"}, map[string]any{
					"ok": map[string]any{"type": "boolean"},
				}),
				"QRCreateResponse": objectSchema([]string{"session_id", "status", "image_url"}, map[string]any{
					"session_id":   map[string]any{"type": "string"},
					"status":       map[string]any{"type": "string", "example": "pending"},
					"image_url":    map[string]any{"type": "string", "example": "/qr/{session_id}/image"},
					"image_base64": nullableStringSchema("二维码图片 data URI。始终包含，但仅当 as_base64=true 时为非 null 值。"),
				}),
				"QRPollResponse": objectSchema([]string{"status"}, map[string]any{
					"status": map[string]any{
						"type": "string",
						"enum": []string{"pending", "scanned", "authorized", "confirmed", "expired", "cancelled", "unknown"},
					},
					"errcode": map[string]any{"type": "integer", "nullable": true},
				}),
				"AccountPublic": objectSchema([]string{"id", "openid", "created_at", "updated_at"}, map[string]any{
					"id":              int64Schema(),
					"openid":          map[string]any{"type": "string"},
					"uin":             nullableInt64Schema(),
					"alias":           nullableStringSchema("账号别名。"),
					"nickname":        nullableStringSchema("账号昵称。"),
					"avatar":          nullableStringSchema("本地头像路径或远程头像 URL。"),
					"status":          nullableStringSchema("账号状态。"),
					"device_id":       nullableStringSchema("稳定设备 MAC 标识，格式 02-XX-XX-XX-XX-XX。"),
					"remark":          nullableStringSchema("账号备注，本地字段，不参与微信侧同步。"),
					"last_checked_at": nullableInt64Schema(),
					"credential_expires_at": map[string]any{
						"type":        "integer",
						"format":      "int64",
						"description": "凭据到期时间戳（Unix 秒）。0 表示无法判断。不含 token 本身，仅用于展示剩余有效期。",
					},
					"created_at": int64Schema(),
					"updated_at": int64Schema(),
				}),
				"RefreshResult": objectSchema([]string{"id", "openid", "status"}, map[string]any{
					"id":       int64Schema(),
					"openid":   map[string]any{"type": "string"},
					"uin":      nullableInt64Schema(),
					"nickname": nullableStringSchema("账号昵称。"),
					"status":   map[string]any{"type": "string", "enum": []string{"alive", "expired", "unknown"}, "description": "存活状态：alive=在线，expired=已过期，unknown=无法判定。"},
				}),
				"DeleteAccountResponse": objectSchema([]string{"deleted", "openid"}, map[string]any{
					"deleted": map[string]any{"type": "integer", "format": "int64", "description": "被删除账号的 ID。"},
					"openid":  map[string]any{"type": "string"},
				}),
				"AccountRefRequest": objectSchema(nil, map[string]any{
					"ref": map[string]any{"type": "string", "description": "账号 ID、UIN 或 openid。支持批量操作的接口不传时表示全部账号。"},
				}),
				"RefreshResponse": oneOfSchema(
					refSchema("RefreshResult"),
					arraySchema(refSchema("RefreshResult")),
				),
				"ResyncResponse": oneOfSchema(
					refSchema("AccountPublic"),
					arraySchema(refSchema("AccountPublic")),
				),
				"WxappRequest": objectSchema([]string{"ref", "app_id"}, map[string]any{
					"ref":    map[string]any{"type": "string", "description": "账号 ID、UIN 或 openid。"},
					"app_id": map[string]any{"type": "string"},
				}),
				"OperateWXDataRequest": objectSchema([]string{"ref", "app_id", "payload"}, map[string]any{
					"ref":     map[string]any{"type": "string", "description": "账号 ID、UIN 或 openid。"},
					"app_id":  map[string]any{"type": "string"},
					"payload": freeFormObjectSchema("完整的 operateWxData 请求 JSON。"),
				}),
				"WxappResponse": objectSchema([]string{"openid", "result"}, map[string]any{
					"openid": map[string]any{"type": "string"},
					"result": freeFormObjectSchema("wxapp 接口返回结果。"),
				}),
				"WebhookPutRequest": objectSchema([]string{"url"}, map[string]any{
					"url": map[string]any{"type": "string", "description": "Webhook 回调地址，传空字符串则清空。"},
				}),
				"WebhookGetResponse": objectSchema([]string{"url"}, map[string]any{
					"url": map[string]any{"type": "string", "description": "当前 Webhook 回调地址，未配置时为空字符串。"},
				}),
				"WebhookTestResponse": objectSchema([]string{"ok", "http_status"}, map[string]any{
					"ok":          map[string]any{"type": "boolean", "description": "测试是否成功。"},
					"http_status": map[string]any{"type": "integer", "description": "目标服务器返回的 HTTP 状态码。"},
				}),
				"OAuthRequest": objectSchema([]string{"app_id", "redirect_uri"}, map[string]any{
					"ref":             nullableStringSchema("可选，账号引用，仅做存在性校验。"),
					"app_id":          map[string]any{"type": "string", "description": "公众号 AppID，18 位且以 wx 开头。"},
					"redirect_uri":    map[string]any{"type": "string", "description": "授权回调地址，必须是绝对 http/https URL。"},
					"scope":           nullableStringSchema("snsapi_base（静默）或 snsapi_userinfo（弹窗），默认 snsapi_base。"),
					"state":           nullableStringSchema("可选回传状态，留空自动生成随机值。"),
					"component_appid": nullableStringSchema("可选，第三方平台 AppID。"),
				}),
				"OAuthResponse": objectSchema([]string{"url", "full_url"}, map[string]any{
					"url":       map[string]any{"type": "string", "description": "授权链接。"},
					"full_url":  map[string]any{"type": "string", "description": "追加 #wechat_redirect 后的完整链接。"},
					"scope":     map[string]any{"type": "string"},
					"state":     map[string]any{"type": "string"},
					"authorize": map[string]any{"type": "string", "description": "授权方式说明。"},
				}),
				"AccountRemarkRequest": objectSchema([]string{"ref"}, map[string]any{
					"ref":    map[string]any{"type": "string", "description": "账号 ID、UIN 或 openid。"},
					"remark": nullableStringSchema("备注内容，留空表示清除，最多 128 字符。"),
				}),
				"AccountStatusRequest": objectSchema([]string{"ref", "status"}, map[string]any{
					"ref":    map[string]any{"type": "string", "description": "账号 ID、UIN 或 openid。"},
					"status": map[string]any{"type": "string", "description": "目标状态，仅接受 alive 或 expired。"},
				}),
				"AccountConfirmRequest": objectSchema(nil, map[string]any{
					"confirm": map[string]any{"type": "boolean", "description": "false 仅预览影响范围，true 才真正执行。"},
				}),
				"AuthCredentialRequest": objectSchema([]string{"username", "password"}, map[string]any{
					"username": map[string]any{"type": "string", "description": "管理员用户名。"},
					"password": map[string]any{"type": "string", "description": "密码，初始化与修改时至少 8 位。"},
				}),
				"AuthPasswordRequest": objectSchema([]string{"old_password", "new_password"}, map[string]any{
					"old_password": map[string]any{"type": "string", "description": "当前密码。"},
					"new_password": map[string]any{"type": "string", "description": "新密码，至少 8 位。"},
				}),
				"AuthStatusResponse": objectSchema(nil, map[string]any{
					"enabled":       map[string]any{"type": "boolean", "description": "是否启用认证。"},
					"initialized":   map[string]any{"type": "boolean", "description": "是否已创建管理员。"},
					"authenticated": map[string]any{"type": "boolean", "description": "当前请求是否已登录。"},
					"username":      nullableStringSchema("当前登录管理员用户名。"),
					"version":       nullableStringSchema("服务版本号。"),
					"commit":        nullableStringSchema("构建提交号。"),
					"build_date":    nullableStringSchema("构建时间。"),
				}),
				"AuthLoginResponse": objectSchema([]string{"authenticated"}, map[string]any{
					"authenticated": map[string]any{"type": "boolean", "description": "是否已建立会话。"},
					"username":      map[string]any{"type": "string"},
					"expires_in":    map[string]any{"type": "integer", "description": "会话有效期（秒）。"},
				}),
				"VersionResponse": objectSchema(nil, map[string]any{
					"version":    map[string]any{"type": "string", "description": "语义化版本号。"},
					"commit":     map[string]any{"type": "string", "description": "构建提交号（短哈希）。"},
					"build_date": map[string]any{"type": "string", "description": "构建时间。"},
				}),
			},
		},
	}
}

// openAPIOperation 构建 OpenAPI Operation 的辅助函数。
func openAPIOperation(tags []string, summary string, parameters []map[string]any, requestBody map[string]any, responses map[string]any) map[string]any {
	out := map[string]any{
		"tags":      tags,
		"summary":   summary,
		"responses": responses,
	}
	if len(parameters) > 0 {
		out["parameters"] = parameters
	}
	if requestBody != nil {
		out["requestBody"] = requestBody
	}
	return out
}

func defaulted(responses map[string]any) map[string]any {
	responses["default"] = jsonErrorResponse("错误响应。")
	return responses
}

func jsonResponse(description string, schema map[string]any) map[string]any {
	return map[string]any{
		"description": description,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": apiResponseSchema(schema),
			},
		},
	}
}

func jsonErrorResponse(description string) map[string]any {
	return map[string]any{
		"description": description,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": refSchema("APIErrorResponse"),
			},
		},
	}
}

func imageResponse(description string) map[string]any {
	return map[string]any{
		"description": description,
		"content": map[string]any{
			"image/jpeg": map[string]any{
				"schema": map[string]any{"type": "string", "format": "binary"},
			},
		},
	}
}

func jsonRequestBody(schema map[string]any) map[string]any {
	return map[string]any{
		"required": true,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": schema,
			},
		},
	}
}

func jsonOptionalRequestBody(schema map[string]any) map[string]any {
	return map[string]any{
		"required": false,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": schema,
			},
		},
	}
}

func pathStringParam(name, description string) map[string]any {
	return map[string]any{
		"name":        name,
		"in":          "path",
		"description": description,
		"required":    true,
		"schema":      map[string]any{"type": "string"},
	}
}

func queryStringParam(name, description string, required bool) map[string]any {
	return map[string]any{
		"name":        name,
		"in":          "query",
		"description": description,
		"required":    required,
		"schema":      map[string]any{"type": "string"},
	}
}

func boolQueryParam(name, description string) map[string]any {
	return map[string]any{
		"name":        name,
		"in":          "query",
		"description": description,
		"required":    false,
		"schema":      map[string]any{"type": "boolean"},
	}
}

func oneOfSchema(schemas ...map[string]any) map[string]any {
	return map[string]any{"oneOf": schemas}
}

func refSchema(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func arraySchema(item map[string]any) map[string]any {
	return map[string]any{
		"type":  "array",
		"items": item,
	}
}

func objectSchema(required []string, properties map[string]any) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func apiResponseSchema(dataSchema map[string]any) map[string]any {
	if dataSchema == nil {
		dataSchema = nullableObjectSchema("实际数据载荷。")
	}
	return objectSchema([]string{"code", "msg", "data"}, map[string]any{
		"code": map[string]any{"type": "integer", "example": 0, "description": "业务状态码，0 表示成功，非 0 表示业务错误。"},
		"msg":  map[string]any{"type": "string", "example": "success", "description": "提示信息，前端可直接用于 Toast 提示。"},
		"data": dataSchema,
	})
}

func freeFormObjectSchema(description string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"description":          description,
		"additionalProperties": true,
	}
}

func nullableObjectSchema(description string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"description":          description,
		"additionalProperties": true,
		"nullable":             true,
	}
}

func nullableStringSchema(description string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": description,
		"nullable":    true,
	}
}

func int64Schema() map[string]any {
	return map[string]any{"type": "integer", "format": "int64"}
}

func nullableInt64Schema() map[string]any {
	return map[string]any{"type": "integer", "format": "int64", "nullable": true}
}
