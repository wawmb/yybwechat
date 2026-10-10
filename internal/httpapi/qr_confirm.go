package httpapi

import (
	"context"
	"errors"
	"strings"
	"time"

	"yybwechat/internal/protocol"
	"yybwechat/internal/qr"
)

// 扫码确认阶段的凭据获取重试参数。
//
// 微信报告「二维码已被授权」到 login buffer 接口真正可读之间存在短暂竞态。
// 直接调用一次常常在这几秒内失败并返回 409，用户只能手动重试。
// 这里加一层短窗口退避重试来吸收这段延迟。
const (
	loginBufferRetryWindow = 8 * time.Second        // 重试总窗口
	loginBufferRetryEvery  = 500 * time.Millisecond // 重试间隔
)

// retryLoginBufferError 判断凭据获取的错误是否值得重试。
//
// 只对「凭据尚未就绪」这类可恢复错误重试：
//   - context.Canceled 不重试，用户主动取消应立即失败；
//   - 其他错误（如参数错误、账号被封）重试没有意义。
func retryLoginBufferError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"not authorized yet", // 尚未完成授权
		"buffer not ready",   // 凭据尚未生成
		"timeout",
		"timed out",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// getLoginBufferWithRetry 在短窗口内重试获取 login buffer。
//
// 重试成功前不会消费任何账号链接或产生副作用，
// 因此瞬时失败不会留下「扫了一半」的中间状态。
func getLoginBufferWithRetry(ctx context.Context, client *qr.Client, sess *qr.Session) (protocol.LoginBufferResult, error) {
	deadline := time.Now().Add(loginBufferRetryWindow)
	var lastErr error
	for {
		result, err := client.GetLoginBuffer(ctx, sess)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !retryLoginBufferError(err) {
			return protocol.LoginBufferResult{}, err
		}
		if time.Now().Add(loginBufferRetryEvery).After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return protocol.LoginBufferResult{}, ctx.Err()
		case <-time.After(loginBufferRetryEvery):
		}
	}
	return protocol.LoginBufferResult{}, lastErr
}
