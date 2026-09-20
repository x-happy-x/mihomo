package outbound

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/transport/socks5"
)

// newTestOlcRTC builds an OlcRTC adapter that is already "ready" and points at
// socksAddr, bypassing the embedded OLCRTC client. This exercises the Mihomo
// side of the MVP: DialContext -> loopback SOCKS5 CONNECT -> NewConn.
func newTestOlcRTC(t *testing.T, socksAddr string) *OlcRTC {
	t.Helper()

	o, err := NewOlcRTC(OlcRTCOption{
		Name:          "olcrtc-test",
		Provider:      "jitsi",
		Transport:     "datachannel",
		RoomID:        "https://meet.example.org/room",
		EncryptionKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("NewOlcRTC: %v", err)
	}

	// Install a session that is already ready and points at socksAddr, standing
	// in for the embedded client. Its lifecycle matches the real one: the run
	// goroutine returns (closing done) when the adapter context is cancelled.
	session := newOlcrtcSession()
	session.localAddr.Store(socksAddr)
	close(session.ready)
	go func() {
		defer close(session.done)
		<-o.ctx.Done()
	}()
	o.mu.Lock()
	o.session = session
	o.mu.Unlock()

	t.Cleanup(func() { _ = o.Close() })
	return o
}

// startEchoSOCKS5 runs a minimal SOCKS5 CONNECT server that, instead of
// dialing out, echoes the payload back and reports the requested destination.
func startEchoSOCKS5(t *testing.T) (addr string, gotDst <-chan string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	dstCh := make(chan string, 1)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		dst, command, _, err := socks5.ServerHandshake(conn, nil)
		if err != nil {
			return
		}
		if command != socks5.CmdConnect {
			return
		}
		select {
		case dstCh <- dst.String():
		default:
		}
		_, _ = io.Copy(conn, conn)
	}()

	return ln.Addr().String(), dstCh
}

func TestOlcRTCDialContextPerformsSocks5Connect(t *testing.T) {
	socksAddr, gotDst := startEchoSOCKS5(t)
	o := newTestOlcRTC(t, socksAddr)

	metadata := &C.Metadata{
		NetWork: C.TCP,
		Host:    "example.com",
		DstPort: 443,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := o.DialContext(ctx, metadata)
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer func() { _ = conn.Close() }()

	select {
	case dst := <-gotDst:
		want := net.JoinHostPort("example.com", strconv.Itoa(443))
		if dst != want {
			t.Fatalf("SOCKS5 CONNECT destination = %q, want %q", dst, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SOCKS5 CONNECT destination")
	}

	// Prove the tunnelled stream carries payload both ways.
	payload := []byte("olcrtc-mvp-roundtrip")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != string(payload) {
		t.Fatalf("echo = %q, want %q", buf, payload)
	}

	if conn.Chains()[0] != "olcrtc-test" {
		t.Fatalf("chains = %v, want first hop olcrtc-test", conn.Chains())
	}
}

func TestOlcRTCUDPNotSupported(t *testing.T) {
	socksAddr, _ := startEchoSOCKS5(t)
	o := newTestOlcRTC(t, socksAddr)

	if o.SupportUDP() {
		t.Fatal("SupportUDP() = true, want false for the TCP-only MVP")
	}
	if _, err := o.ListenPacketContext(context.Background(), &C.Metadata{}); err != C.ErrNotSupport {
		t.Fatalf("ListenPacketContext error = %v, want %v", err, C.ErrNotSupport)
	}
}

// TestOlcRTCCloseBeforeStart makes sure a never-dialed adapter closes cleanly
// and can never start afterwards (reload safety).
func TestOlcRTCCloseBeforeStart(t *testing.T) {
	o, err := NewOlcRTC(OlcRTCOption{
		Name:          "olcrtc-close",
		Provider:      "jitsi",
		Transport:     "datachannel",
		RoomID:        "https://meet.example.org/room",
		EncryptionKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("NewOlcRTC: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- o.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close() blocked on an adapter that was never started")
	}

	// A dial after Close must fail rather than spin up a new OLCRTC client.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := o.DialContext(ctx, &C.Metadata{NetWork: C.TCP, Host: "example.com", DstPort: 443}); err == nil {
		t.Fatal("DialContext succeeded after Close(), want error")
	}
}

func TestOlcRTCRequiredFields(t *testing.T) {
	base := OlcRTCOption{
		Name:          "n",
		Provider:      "jitsi",
		Transport:     "datachannel",
		RoomID:        "https://meet.example.org/room",
		EncryptionKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	for _, tc := range []struct {
		name   string
		mutate func(*OlcRTCOption)
	}{
		{"missing provider", func(o *OlcRTCOption) { o.Provider = "" }},
		{"missing transport", func(o *OlcRTCOption) { o.Transport = "" }},
		{"missing room-id", func(o *OlcRTCOption) { o.RoomID = "" }},
		{"missing encryption-key", func(o *OlcRTCOption) { o.EncryptionKey = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := base
			tc.mutate(&opt)
			if _, err := NewOlcRTC(opt); err == nil {
				t.Fatalf("NewOlcRTC(%s) = nil error, want error", tc.name)
			}
		})
	}
}

// TestOlcRTCRestartsAfterSessionFailure covers the recovery requirement: when a
// session dies (remote OLCRTC server went away), the next dial must start a
// fresh session instead of caching the failure forever.
func TestOlcRTCRestartsAfterSessionFailure(t *testing.T) {
	o, err := NewOlcRTC(OlcRTCOption{
		Name:          "olcrtc-restart",
		Provider:      "jitsi",
		Transport:     "datachannel",
		RoomID:        "https://meet.example.org/room",
		EncryptionKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("NewOlcRTC: %v", err)
	}
	t.Cleanup(func() { _ = o.Close() })

	// Simulate a session that already failed.
	dead := newOlcrtcSession()
	dead.setErr(errors.New("link lost"))
	close(dead.done)
	o.mu.Lock()
	o.session = dead
	o.mu.Unlock()

	// The next lookup must hand back a different, freshly started session.
	revived, err := o.currentSession()
	if err != nil {
		t.Fatalf("currentSession after failure: %v", err)
	}
	if revived == dead {
		t.Fatal("currentSession reused the dead session; outbound would never recover")
	}

	select {
	case <-revived.done:
		// A real start attempt against an unreachable room may fail fast; what
		// matters is that a new session was created at all.
	default:
	}
}
