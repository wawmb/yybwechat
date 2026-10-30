package protocol

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

type pskEntry struct {
	PSKType      int    `json:"psk_type"`
	PreSharedKey string `json:"pre_shared_key"`
	TicketEntry  string `json:"ticket_entry"`
	Lifetime     int64  `json:"lifetime"`
	ExpiredAt    int64  `json:"expired_at"`
}

type mmtlsClient struct {
	conn    net.Conn
	timeout time.Duration
	// ctx 与 stopCancel 让握手之后的读写也能响应取消。
	// 只有 dial 阶段用 ctx 是不够的：握手一旦开始，ctx 到期必须能把阻塞中的
	// Read/Write 打断，否则调用方的整体时间预算形同虚设。
	ctx        context.Context
	stopCancel func() bool

	g1Priv *ecdh.PrivateKey
	g1Pub  []byte
	g2Pub  []byte

	clientHello []byte
	serverPub   []byte
	secret      []byte
	master      []byte

	transcript []byte
	hCertV     []byte
	hFinal     []byte
	nstEntries []pskTicket

	hsCKey, hsCIV   []byte
	hsSKey, hsSIV   []byte
	appCKey, appCIV []byte
	appSKey, appSIV []byte

	txSeq  uint64
	rxSeq  uint64
	appSeq uint32

	rxbuf []byte
	recq  []record
}

func newMmtlsClient(ctx context.Context, conn net.Conn, timeout time.Duration) *mmtlsClient {
	return &mmtlsClient{conn: conn, timeout: timeout, ctx: ctx,
		stopCancel: context.AfterFunc(ctx, func() { _ = conn.Close() })}
}

func (m *mmtlsClient) close() {
	// 先注销回调再关连接：AfterFunc 触发时会关连接，这里再关一次是幂等的，
	// 但若不注销，回调会一直挂到 ctx 生命周期结束。
	if m.stopCancel != nil {
		m.stopCancel()
	}
	if m.conn != nil {
		_ = m.conn.Close()
	}
}

func (m *mmtlsClient) feed(payload []byte) {
	m.transcript = append(m.transcript, payload...)
}

func (m *mmtlsClient) transcriptHash() []byte {
	sum := sha256.Sum256(m.transcript)
	return sum[:]
}

func (m *mmtlsClient) nextRecord() (record, error) {
	for len(m.recq) == 0 {
		if err := m.ctx.Err(); err != nil {
			return record{}, err
		}
		recs, consumed := parseRecords(m.rxbuf)
		if len(recs) > 0 {
			m.recq = append(m.recq, recs...)
			m.rxbuf = m.rxbuf[consumed:]
			break
		}
		// 单次读的超时不能把外层的截止时间往后推，必须取两者更早的那个
		_ = m.conn.SetReadDeadline(ioDeadline(m.ctx, m.timeout))
		buf := make([]byte, 65536)
		n, err := m.conn.Read(buf)
		if err != nil {
			return record{}, err
		}
		if n == 0 {
			return record{}, io.EOF
		}
		m.rxbuf = append(m.rxbuf, buf[:n]...)
	}
	r := m.recq[0]
	m.recq = m.recq[1:]
	return r, nil
}

func (m *mmtlsClient) sendRaw(data []byte) error {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	_ = m.conn.SetWriteDeadline(ioDeadline(m.ctx, m.timeout))
	_, err := m.conn.Write(data)
	return err
}

func (m *mmtlsClient) sendPlain(contentType byte, payload []byte) error {
	rec, err := buildRecord(contentType, payload)
	if err != nil {
		return err
	}
	if err = m.sendRaw(rec); err != nil {
		return err
	}
	m.txSeq++
	return nil
}

func (m *mmtlsClient) sendEncrypted(contentType byte, plaintext, key, iv []byte) error {
	ct, err := encryptRecordPayload(key, iv, m.txSeq, contentType, plaintext)
	if err != nil {
		return err
	}
	rec, err := buildRecord(contentType, ct)
	if err != nil {
		return err
	}
	if err = m.sendRaw(rec); err != nil {
		return err
	}
	m.txSeq++
	return nil
}

func (m *mmtlsClient) recvPlain() (byte, []byte, error) {
	r, err := m.nextRecord()
	if err != nil {
		return 0, nil, err
	}
	m.rxSeq++
	return r.ContentType, r.Payload, nil
}

func (m *mmtlsClient) recvDecrypt(key, iv []byte) (byte, []byte, error) {
	r, err := m.nextRecord()
	if err != nil {
		return 0, nil, err
	}
	pt, err := decryptRecordPayload(key, iv, m.rxSeq, r.ContentType, r.Payload)
	if err != nil {
		return 0, nil, err
	}
	m.rxSeq++
	return r.ContentType, pt, nil
}

func (m *mmtlsClient) buildClientHello() ([]byte, error) {
	priv, g1, err := generateECDH()
	if err != nil {
		return nil, err
	}
	_, g2, err := generateECDH()
	if err != nil {
		return nil, err
	}
	randomBytes := make([]byte, 32)
	if _, err = rand.Read(randomBytes); err != nil {
		return nil, err
	}
	ch, err := buildClientHello(randomBytes, uint32(time.Now().Unix()), g1, g2)
	if err != nil {
		return nil, err
	}
	m.g1Priv = priv
	m.g1Pub = g1
	m.g2Pub = g2
	m.clientHello = ch
	return ch, nil
}

func (m *mmtlsClient) sendClientHello() error {
	ch, err := m.buildClientHello()
	if err != nil {
		return err
	}
	m.feed(ch)
	return m.sendPlain(ctHandshake, ch)
}

func (m *mmtlsClient) recvServerFlight() error {
	_, payload, err := m.recvPlain()
	if err != nil {
		return err
	}
	mt, body, err := splitHandshake(payload)
	if err != nil {
		return err
	}
	if mt != hsServerHello {
		return fmt.Errorf("expected ServerHello, got 0x%02x", mt)
	}
	m.feed(payload)
	sh, err := parseServerHello(body)
	if err != nil {
		return err
	}
	if sh.ServerGroup != groupSecP256R1 {
		return fmt.Errorf("server group %d is not P-256", sh.ServerGroup)
	}
	m.serverPub = sh.ServerPubKey
	m.secret, err = mmtlsSecret(m.g1Priv, sh.ServerPubKey)
	if err != nil {
		return err
	}
	hChSh := m.transcriptHash()
	hk := deriveHandshakeKeys(m.secret, hChSh)
	m.hsCKey, m.hsCIV = hk.ClientKey, hk.ClientIV
	m.hsSKey, m.hsSIV = hk.ServerKey, hk.ServerIV

	for {
		_, pt, err := m.recvDecrypt(m.hsSKey, m.hsSIV)
		if err != nil {
			return err
		}
		mt, mbody, err := splitHandshake(pt)
		if err != nil {
			return err
		}
		if mt == hsFinished {
			m.hFinal = m.transcriptHash()
			m.master = deriveMaster(m.secret, m.hFinal)
			ak := deriveAppKeys(m.master, m.hFinal)
			m.appCKey, m.appCIV = ak.ClientKey, ak.ClientIV
			m.appSKey, m.appSIV = ak.ServerKey, ak.ServerIV
			got, err := parseFinished(mbody)
			if err != nil {
				return err
			}
			want := finishedVerifyData(m.secret, false, m.hFinal)
			if !bytes.Equal(got, want) {
				return fmt.Errorf("ServerFinished verification failed")
			}
			m.feed(pt)
			return nil
		}
		m.feed(pt)
		switch mt {
		case hsCertVerify:
			m.hCertV = m.transcriptHash()
		case hsNewSessionTicket:
			m.nstEntries = parseNewSessionTicket(mbody)
		}
	}
}

func (m *mmtlsClient) sendClientFinished() error {
	if m.hFinal == nil {
		return fmt.Errorf("H_final is not ready")
	}
	vd := finishedVerifyData(m.secret, true, m.hFinal)
	msg, err := buildFinished(vd)
	if err != nil {
		return err
	}
	return m.sendEncrypted(ctHandshake, msg, m.hsCKey, m.hsCIV)
}

func (m *mmtlsClient) doHandshake() error {
	if err := m.sendClientHello(); err != nil {
		return err
	}
	if err := m.recvServerFlight(); err != nil {
		return err
	}
	return m.sendClientFinished()
}

func (m *mmtlsClient) extractPSKs() ([]pskEntry, error) {
	if m.secret == nil || m.hCertV == nil {
		return nil, fmt.Errorf("handshake secret/h_certv not ready")
	}
	now := time.Now().Unix()
	out := make([]pskEntry, 0, len(m.nstEntries))
	for _, e := range m.nstEntries {
		psk := derivePSK(m.secret, m.hCertV, e.PSKType)
		out = append(out, pskEntry{
			PSKType:      e.PSKType,
			PreSharedKey: fmt.Sprintf("%x", psk),
			TicketEntry:  fmt.Sprintf("%x", e.TicketEntry),
			Lifetime:     e.Lifetime,
			ExpiredAt:    now + e.Lifetime,
		})
	}
	return out, nil
}

func pickAccessPSK(entries []pskEntry) (pskEntry, bool) {
	now := time.Now().Unix()
	for _, e := range entries {
		if e.PSKType == 1 && e.ExpiredAt > now {
			return e, true
		}
	}
	return pskEntry{}, false
}

func (m *mmtlsClient) sendApp(cmd uint32, body []byte) error {
	pkt := buildShortlink(cmd, m.appSeq, body, defaultVer)
	m.appSeq++
	return m.sendEncrypted(ctAppData, pkt, m.appCKey, m.appCIV)
}

func (m *mmtlsClient) recvApp() (shortlinkPacket, error) {
	ct, pt, err := m.recvDecrypt(m.appSKey, m.appSIV)
	if err != nil {
		return shortlinkPacket{}, err
	}
	if ct != ctAppData {
		return shortlinkPacket{}, fmt.Errorf("expected AppData, got 0x%02x", ct)
	}
	return parseShortlink(pt)
}

func connectMmtls(ctx context.Context, target Target, timeout time.Duration, tcpProxy string, fallbackDirect bool) (*mmtlsClient, error) {
	conn, err := dialTCP(ctx, target.IP, target.Port, timeout, tcpProxy, fallbackDirect)
	if err != nil {
		return nil, err
	}
	mc := newMmtlsClient(ctx, conn, timeout)
	if err = mc.doHandshake(); err != nil {
		mc.close()
		// 带上端点，并明确这是握手阶段失败——与建连失败区分开，
		// 排查时才能判断是「连不上」还是「连上了但握手被拒」
		return nil, fmt.Errorf("LongLink handshake to %s failed: %w", net.JoinHostPort(target.IP, strconv.Itoa(target.Port)), err)
	}
	return mc, nil
}
