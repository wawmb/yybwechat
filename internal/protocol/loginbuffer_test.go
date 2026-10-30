package protocol

import "net/http"

// loginBufferRoundTripFunc 让测试用自定义 RoundTrip 替换真实 HTTP 客户端，
// 从而在不联网的前提下断言请求走了哪个端点、用了哪个业务与签名。
type loginBufferRoundTripFunc func(*http.Request) (*http.Response, error)

func (f loginBufferRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
