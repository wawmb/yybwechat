package protocol

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

// ShortlinkRejectedError 表示短连接在传输层或应用层被明确拒绝。
//
// 以前这类响应会被统一吞成「解密/解析失败」，运维只能看到一个笼统错误，
// 分不清到底是网络层还是应用层拒绝、拒绝码是什么。
// 上游实测过：MMTLS 层认证通过、但应用头返回 -14 且没有加密正文。
type ShortlinkRejectedError struct {
	Code int64
	// Transport 为真表示拒绝来自传输层头（字段 4），否则来自应用层头（字段 23）。
	Transport bool
}

func (e *ShortlinkRejectedError) Error() string {
	layer := "application"
	if e.Transport {
		layer = "transport"
	}
	return fmt.Sprintf("shortlink %s rejected: code=%d", layer, e.Code)
}

// Unwrap 让它同时算作「载荷不可用」，上层的重建/重试策略保持原样。
func (e *ShortlinkRejectedError) Unwrap() error { return ErrShortlinkPayload }

// IsApplicationRequestRejection 判断这次失败是不是「单独的、已认证的应用层 -14」。
//
// v0.2.50 的核心修正：上游在**同一份会话**上做过对照——两次成功调用之间，
// 也会冒出一次应用头 -14 且没有加密正文。它只说明这一次请求没被受理，
// 既不证明会话坏了，也不证明凭据被撤了。早期实现一看到 -14 就删共享会话、
// 重建登录、轮换账号凭据，把一次瞬时拒绝放大成一次全量重登录，
// 反而制造了更多失败。
//
// 因此这里把它单独识别出来：调用方据此只让当前请求返回 502，
// 不动会话、不动凭据。真正的传输错误、以及真实的 ManualAuth / OAuth 拒绝
// 都不属于这一类，仍走原有的恢复策略。
//
// 对 errors.Join 的处理是「全部子节点都算才算」：任何夹带其它原因的混合错误
// 都不能被简化成单纯的应用拒绝，否则会掩盖真正需要恢复的那一段。
func IsApplicationRequestRejection(err error) bool {
	switch e := err.(type) {
	case *ShortlinkRejectedError:
		return !e.Transport && e.Code == -14
	case interface{ Unwrap() []error }:
		children := e.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !IsApplicationRequestRejection(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return IsApplicationRequestRejection(e.Unwrap())
	}
	return false
}

var (
	// ErrShortlinkPayload 表示收到的 AppData 无法解密或解析。
	// 单独定义成哨兵错误，便于上层区分「网络/代理故障」与「报文不合法」。
	ErrShortlinkPayload = errors.New("AppData decrypt/parse failed")
	type8Prefix         = []byte{0x00, 0x00, 0x00, 0x10, 0x08, 0x00, 0x00, 0x00, 0x0b, 0x01, 0x00, 0x00, 0x00, 0x06, 0x00, 0x12}
	earlyAlert          = []byte{0x00, 0x00, 0x00, 0x03, 0x00, 0x01, 0x01}
)

// shortlinkHeaderRejection 在「已解密、wpkg 头完整、但没有正文」时提取拒绝码。
//
// 只认完整匹配（size == len(body)）的响应头，避免把正常载荷误读成拒绝。
func shortlinkHeaderRejection(body []byte) error {
	fields, _, size, err := decodeWpkgHead(body)
	if err != nil || size != len(body) {
		return nil
	}
	var causes []error
	if code := int64(fields[4]); code != 0 {
		causes = append(causes, &ShortlinkRejectedError{Code: code, Transport: true})
	}
	if code := int64(fields[23]); code != 0 {
		causes = append(causes, &ShortlinkRejectedError{Code: code})
	}
	return errors.Join(causes...)
}

func build0RTTRequest(entry pskEntry, envelope []byte) ([]byte, []byte, []byte, []byte, error) {
	psk, err := hex.DecodeString(entry.PreSharedKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	ticket, err := hex.DecodeString(entry.TicketEntry)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	randomBytes := make([]byte, 32)
	if _, err = rand.Read(randomBytes); err != nil {
		return nil, nil, nil, nil, err
	}
	ts := uint32(time.Now().Unix())
	pskCH, err := buildPSKClientHello(randomBytes, ts, ticket)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	hEarly := sha256.Sum256(pskCH)
	ek := derivePSKOneWayKeys(psk, labelEarlyKeys, hEarly[:])
	type8 := append([]byte(nil), type8Prefix...)
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], ts)
	type8 = append(type8, tmp[:]...)

	body := []byte{}
	r0, _ := buildRecord(ctPSKHandshake, pskCH)
	body = append(body, r0...)
	ct1, err := encryptRecordPayload(ek.Key, ek.IV, 1, ctPSKHandshake, type8)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	r1, _ := buildRecord(ctPSKHandshake, ct1)
	body = append(body, r1...)
	ct2, err := encryptRecordPayload(ek.Key, ek.IV, 2, ctAppData, envelope)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	r2, _ := buildRecord(ctAppData, ct2)
	body = append(body, r2...)
	ct3, err := encryptRecordPayload(ek.Key, ek.IV, 3, ctAlert, earlyAlert)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	r3, _ := buildRecord(ctAlert, ct3)
	body = append(body, r3...)
	return body, psk, pskCH, type8, nil
}

func httpPost(path, host string, body []byte) []byte {
	head := fmt.Sprintf("POST %s HTTP/1.0\r\nAccept: */*\r\nCache-Control: no-cache\r\nConnection: close\r\nContent-Length: %d\r\nContent-Type: application/octet-stream\r\nHost: %s\r\nUpgrade: mmtls\r\nUser-Agent: MicroMessenger Client\r\nX-Online-Host: %s\r\n\r\n", path, len(body), host, host)
	return append([]byte(head), body...)
}

func send0RTT(ctx context.Context, targets []Target, entry pskEntry, recvKey, envelope []byte, timeout time.Duration, tcpProxy string, fallbackDirect bool) ([]byte, []byte, error) {
	var last error
	for _, t := range targets {
		// 请求已被取消（或整体预算耗尽）就不要继续挨个打靶了
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		code, resp, err := send0RTTRaw(ctx, t, entry, recvKey, envelope, timeout, tcpProxy, fallbackDirect)
		if err == nil && (len(code) > 0 || len(resp) > 0) {
			return code, resp, nil
		}
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("ShortLink response did not contain application data")
		}
	}
	if last == nil {
		last = fmt.Errorf("no ShortLink targets")
	}
	return nil, nil, fmt.Errorf("all ShortLink targets failed; last error: %w", last)
}

func send0RTTRaw(ctx context.Context, target Target, entry pskEntry, recvKey, envelope []byte, timeout time.Duration, tcpProxy string, fallbackDirect bool) ([]byte, []byte, error) {
	body, psk, pskCH, type8, err := build0RTTRequest(entry, envelope)
	if err != nil {
		return nil, nil, err
	}
	path := fmt.Sprintf("/mmtls/%08x", uint32(time.Now().Unix()))
	req := httpPost(path, "shortcloud.weixin.com", body)
	conn, err := dialTCP(ctx, target.IP, target.Port, timeout, tcpProxy, fallbackDirect)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	// 与 LongLink 同理：deadline 必须取自 ctx，否则 io.ReadAll 会无视取消
	// 一直阻塞到 timeout，把调用方的整体预算拖垮。
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	_ = conn.SetDeadline(ioDeadline(ctx, timeout))
	if _, err = conn.Write(req); err != nil {
		return nil, nil, err
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		return nil, nil, fmt.Errorf("ShortLink response from %s:%d failed: %w", target.IP, target.Port, err)
	}
	_, responseBody := splitHTTP(raw)
	if len(responseBody) == 0 {
		return nil, nil, fmt.Errorf("empty ShortLink response")
	}
	return parse0RTTResponse(responseBody, psk, pskCH, type8, recvKey)
}

func parse0RTTResponse(rbody, psk, pskCH, type8, recvKey []byte) ([]byte, []byte, error) {
	recs, _ := parseRecords(rbody)
	var sh []byte
	var encHS [][]byte
	var appdata []byte
	for _, r := range recs {
		if r.ContentType == ctHandshake {
			if sh == nil {
				sh = r.Payload
			}
			encHS = append(encHS, r.Payload)
		}
		if r.ContentType == ctAppData && appdata == nil {
			appdata = r.Payload
		}
	}
	if sh == nil || appdata == nil {
		return nil, nil, fmt.Errorf("response missing ServerHello/AppData")
	}
	candidates := [][]byte{
		bytes.Join([][]byte{pskCH, sh}, nil),
		bytes.Join([][]byte{pskCH, type8, sh}, nil),
		bytes.Join([][]byte{pskCH, sh, type8}, nil),
	}
	for _, trans := range candidates {
		h := sha256.Sum256(trans)
		hk := derivePSKOneWayKeys(psk, labelHandshakeKeys, h[:])
		for _, seq := range []uint64{2, 1, 3} {
			ad, err := decryptRecordPayload(hk.Key, hk.IV, seq, ctAppData, appdata)
			if err != nil {
				continue
			}
			if sl, err := parseShortlink(ad); err == nil {
				if rejection := shortlinkHeaderRejection(sl.Body); rejection != nil {
					return nil, nil, rejection
				}
				if resp, err := sessionDecrypt(sl.Body, recvKey); err == nil {
					return extractCode(resp), resp, nil
				}
			}
			if rejection := shortlinkHeaderRejection(ad); rejection != nil {
				return nil, nil, rejection
			}
			if resp, err := sessionDecrypt(ad, recvKey); err == nil {
				return extractCode(resp), resp, nil
			}
		}
	}
	return nil, nil, ErrShortlinkPayload
}
