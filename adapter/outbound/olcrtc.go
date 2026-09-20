package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/transport/socks5"

	olcclient "github.com/openlibrecommunity/olcrtc/pkg/olcrtc/client"
)

// OlcRTC tunnels TCP connections through an embedded OLCRTC client.
//
// The OLCRTC client runs in-process as a library and exposes a SOCKS5 listener
// bound to a loopback address with an OS-assigned port. DialContext connects to
// that listener and performs a SOCKS5 CONNECT for the requested destination, so
// no separate olcrtc executable is involved.
type OlcRTC struct {
	*Base

	option *OlcRTCOption

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closed  bool
	session *olcrtcSession // current run, nil when nothing is running
}

// olcrtcSession is one run of the embedded OLCRTC client. A session is not
// reusable: once its run loop returns, the next dial starts a fresh session.
// That is what lets the outbound recover after the remote side goes away.
type olcrtcSession struct {
	ready chan struct{} // closed once the local SOCKS5 listener is accepting
	done  chan struct{} // closed once the run loop returned

	localAddr atomic.Value // string, the real "127.0.0.1:port" of the listener

	errMu sync.Mutex
	err   error
}

func newOlcrtcSession() *olcrtcSession {
	return &olcrtcSession{
		ready: make(chan struct{}),
		done:  make(chan struct{}),
	}
}

func (s *olcrtcSession) setErr(err error) {
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
}

func (s *olcrtcSession) getErr() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *olcrtcSession) addr() (string, error) {
	addr, _ := s.localAddr.Load().(string)
	if addr == "" {
		return "", errors.New("olcrtc: local SOCKS address unavailable")
	}
	return addr, nil
}

type OlcRTCOption struct {
	BasicOption

	Name          string `proxy:"name"`
	Provider      string `proxy:"auth-provider"`
	Transport     string `proxy:"transport"`
	RoomID        string `proxy:"room-id"`
	EncryptionKey string `proxy:"encryption-key"`
	DNSServer     string `proxy:"dns-server,omitempty"`

	ChannelID     string `proxy:"channel-id,omitempty"`
	ProviderToken string `proxy:"provider-token,omitempty"`
}

// currentSession returns the live session, starting a new one when the previous
// run has already finished (or when none ever ran).
func (o *OlcRTC) currentSession() (*olcrtcSession, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.closed {
		return nil, fmt.Errorf("olcrtc %s is closed", o.Name())
	}

	if o.session != nil {
		select {
		case <-o.session.done:
			// Previous run ended; fall through and start a fresh one.
			o.session = nil
		default:
			return o.session, nil
		}
	}

	session := newOlcrtcSession()
	o.session = session
	o.startSession(session)
	return session, nil
}

// startSession launches the embedded OLCRTC client for one session.
func (o *OlcRTC) startSession(session *olcrtcSession) {
	cfg := olcclient.Config{
		Transport: o.option.Transport,
		Provider:  o.option.Provider,
		RoomURL:   o.option.RoomID,
		KeyHex:    o.option.EncryptionKey,

		ChannelID:     o.option.ChannelID,
		ProviderToken: o.option.ProviderToken,

		DNSServer: o.option.DNSServer,

		// Let the OS pick a free loopback port.
		LocalAddr: "127.0.0.1:0",
	}

	log.Debugln("[OLCRTC] %s starting (provider=%s transport=%s)", o.Name(), cfg.Provider, cfg.Transport)

	client := olcclient.New(cfg)

	go func() {
		defer close(session.done)
		err := client.RunWithAddress(o.ctx, func(actualAddr string) {
			session.localAddr.Store(actualAddr)
			log.Infoln("[OLCRTC] %s local SOCKS ready: %s", o.Name(), actualAddr)
			close(session.ready)
		})
		if err != nil && o.ctx.Err() == nil {
			session.setErr(err)
			log.Errorln("[OLCRTC] %s session ended: %v", o.Name(), err)
			return
		}
		log.Debugln("[OLCRTC] %s session ended", o.Name())
	}()
}

// ensureReady starts a session if needed and waits for its local SOCKS5
// listener to come up, returning that listener address.
//
// The session keeps coming up in the background even when the caller context
// expires first: bringing up the WebRTC link can outlast a single dial timeout,
// so a later dial finds a warm session instead of restarting from scratch.
func (o *OlcRTC) ensureReady(ctx context.Context) (*olcrtcSession, error) {
	session, err := o.currentSession()
	if err != nil {
		return nil, err
	}

	select {
	case <-session.ready:
		return session, nil
	default:
	}

	select {
	case <-session.ready:
		return session, nil
	case <-session.done:
		if runErr := session.getErr(); runErr != nil {
			return nil, fmt.Errorf("olcrtc %s failed to start: %w", o.Name(), runErr)
		}
		return nil, fmt.Errorf("olcrtc %s stopped before becoming ready", o.Name())
	case <-o.ctx.Done():
		return nil, fmt.Errorf("olcrtc %s is closed", o.Name())
	case <-ctx.Done():
		return nil, fmt.Errorf("olcrtc %s not ready yet: %w", o.Name(), ctx.Err())
	}
}

// StreamConnContext implements C.ProxyAdapter
func (o *OlcRTC) StreamConnContext(ctx context.Context, c net.Conn, metadata *C.Metadata) (_ net.Conn, err error) {
	if ctx.Done() != nil {
		done := N.SetupContextForConn(ctx, c)
		defer done(&err)
	}
	if _, err = socks5.ClientHandshake(c, serializesSocksAddr(metadata), socks5.CmdConnect, nil); err != nil {
		return nil, fmt.Errorf("olcrtc %s socks5 connect error: %w", o.Name(), err)
	}
	return c, nil
}

// DialContext implements C.ProxyAdapter
func (o *OlcRTC) DialContext(ctx context.Context, metadata *C.Metadata) (_ C.Conn, err error) {
	session, err := o.ensureReady(ctx)
	if err != nil {
		return nil, err
	}

	addr, err := session.addr()
	if err != nil {
		return nil, err
	}

	// The embedded SOCKS5 listener is on loopback, so dial it directly instead
	// of going through the configured dialer (interface binding / routing marks
	// would only break a loopback connection).
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("olcrtc %s local socks dial error: %w", o.Name(), err)
	}

	defer func(c net.Conn) {
		safeConnClose(c, err)
	}(c)

	c, err = o.StreamConnContext(ctx, c, metadata)
	if err != nil {
		return nil, err
	}

	return NewConn(c, o), nil
}

// ListenPacketContext implements C.ProxyAdapter
//
// UDP is out of scope for the OLCRTC MVP.
func (o *OlcRTC) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (C.PacketConn, error) {
	return nil, C.ErrNotSupport
}

// Close implements C.ProxyAdapter
func (o *OlcRTC) Close() error {
	o.mu.Lock()
	o.closed = true
	session := o.session
	o.session = nil
	o.mu.Unlock()

	o.cancel()

	if session == nil {
		return nil
	}
	select {
	case <-session.done:
	case <-time.After(5 * time.Second):
		log.Warnln("[OLCRTC] %s did not shut down within 5s", o.Name())
	}
	return nil
}

func NewOlcRTC(option OlcRTCOption) (*OlcRTC, error) {
	if option.Provider == "" {
		return nil, errors.New("olcrtc: auth-provider is required")
	}
	if option.Transport == "" {
		return nil, errors.New("olcrtc: transport is required")
	}
	if option.RoomID == "" {
		return nil, errors.New("olcrtc: room-id is required")
	}
	if option.EncryptionKey == "" {
		return nil, errors.New("olcrtc: encryption-key is required")
	}

	ctx, cancel := context.WithCancel(context.Background())

	outbound := &OlcRTC{
		Base: NewBase(BaseOption{
			Name:         option.Name,
			Addr:         option.RoomID,
			Type:         C.OlcRTC,
			UDP:          false,
			TFO:          option.TFO,
			MPTCP:        option.MPTCP,
			Interface:    option.Interface,
			RoutingMark:  option.RoutingMark,
			Prefer:       option.IPVersion,
			ProviderName: option.ProviderName,
		}),
		option: &option,
		ctx:    ctx,
		cancel: cancel,
	}

	return outbound, nil
}
