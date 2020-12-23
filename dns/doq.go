package dns

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"github.com/Dreamacro/clash/log"
	"github.com/lucas-clemente/quic-go"
	D "github.com/miekg/dns"
)

const NextProtoDQ = "doq-i00"

type doqClient struct {
	addr    string
	session quic.Session

	bytesPool    *sync.Pool // byte packets pool
	sync.RWMutex            // protects session and bytesPool
}

func (dc *doqClient) Exchange(m *D.Msg) (msg *D.Msg, err error) {
	return dc.ExchangeContext(context.Background(), m)
}

func (dc *doqClient) ExchangeContext(ctx context.Context, m *D.Msg) (msg *D.Msg, err error) {
	session, err := dc.getSession()
	if err != nil {
		return nil, err
	}

	stream, err := dc.openStream(ctx, session)
	if err != nil {
		return nil, fmt.Errorf("failed to open new stream to %s", dc.addr)
	}

	buf, err := m.Pack()
	if err != nil {
		return nil, err
	}

	_, err = stream.Write(buf)
	if err != nil {
		return nil, err
	}

	// The client MUST send the DNS query over the selected stream, and MUST
	// indicate through the STREAM FIN mechanism that no further data will
	// be sent on that stream.
	// stream.Close() -- closes the write-direction of the stream.
	_ = stream.Close()

	pool := dc.getBytesPool()
	respBuf := pool.Get().([]byte)

	// Linter says that the argument needs to be pointer-like
	// But it's already pointer-like
	// nolint
	defer pool.Put(respBuf)

	n, err := stream.Read(respBuf)
	if err != nil && n == 0 {
		return nil, err
	}

	reply := new(D.Msg)
	err = reply.Unpack(respBuf)
	if err != nil {
		return nil, err
	}

	return reply, nil
}

func isActive(s quic.Session) bool {
	select {
	case <-s.Context().Done():
		return false
	default:
		return true
	}
}

// getSession - opens or returns an existing quic.Session
// useCached - if true and cached session exists, return it right away
// otherwise - forcibly creates a new session
func (dc *doqClient) getSession() (quic.Session, error) {
	var session quic.Session
	dc.RLock()
	session = dc.session
	if session != nil && isActive(session) {
		dc.RUnlock()
		return session, nil
	}
	if session != nil {
		// we're recreating the session, let's create a new one
		_ = session.CloseWithError(0, "")
	}
	dc.RUnlock()

	dc.Lock()
	defer dc.Unlock()

	var err error
	session, err = dc.openSession()
	if err != nil {
		// This does not look too nice, but QUIC (or maybe quic-go)
		// doesn't seem stable enough.
		// Maybe retransmissions aren't fully implemented in quic-go?
		// Anyways, the simple solution is to make a second try when
		// it fails to open the QUIC session.
		session, err = dc.openSession()
		if err != nil {
			return nil, err
		}
	}
	dc.session = session
	return session, nil
}

func (dc *doqClient) getBytesPool() *sync.Pool {
	dc.Lock()
	if dc.bytesPool == nil {
		dc.bytesPool = &sync.Pool{
			New: func() interface{} {
				return make([]byte, D.MaxMsgSize)
			},
		}
	}
	dc.Unlock()
	return dc.bytesPool
}

func (dc *doqClient) openSession() (quic.Session, error) {
	tlsConfig := &tls.Config{
		ClientSessionCache: globalSessionCache,
		InsecureSkipVerify: true,
		NextProtos: []string{
			"http/1.1", "h2", NextProtoDQ,
		},
		SessionTicketsDisabled: false,
	}
	quicConfig := &quic.Config{
		ConnectionIDLength: 12,
		HandshakeTimeout:   time.Second * 8,
	}

	log.Debugln("opening session to %s", dc.addr)
	session, err := quic.DialAddrContext(context.Background(), dc.addr, tlsConfig, quicConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to open QUIC session: %w", err)
	}

	return session, nil
}

func (dc *doqClient) openStream(ctx context.Context, session quic.Session) (quic.Stream, error) {
	stream, err := session.OpenStreamSync(ctx)
	if err == nil {
		return stream, nil
	}

	// try to recreate the session
	newSession, err := dc.getSession()
	if err != nil {
		return nil, err
	}
	// open a new stream
	return newSession.OpenStreamSync(ctx)
}
