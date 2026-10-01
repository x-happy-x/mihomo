package provider

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/metacubex/http"
	"github.com/metacubex/mihomo/common/utils"
	"github.com/metacubex/mihomo/component/ca"
	C "github.com/metacubex/mihomo/constant"
)

// AdaptiveTarget describes an actual GET, not a TCP ping or HEAD response.
type AdaptiveTarget struct {
	URL            string `provider:"url" json:"url"`
	ExpectedStatus string `provider:"expected-status,omitempty" json:"expectedStatus"`
	MinBytes       int64  `provider:"min-bytes,omitempty" json:"minBytes"`
}

type AdaptiveProbeResult struct {
	URL    string `json:"url"`
	OK     bool   `json:"ok"`
	Status int    `json:"status,omitempty"`
	Bytes  int64  `json:"bytes"`
	MS     int64  `json:"ms"`
	Stage  string `json:"stage"`
	Error  string `json:"error,omitempty"`
}

func validateAdaptiveTargets(targets []AdaptiveTarget) error {
	if len(targets) == 0 || len(targets) > 4 {
		return fmt.Errorf("adaptive checks require 1..4 targets per list")
	}
	for i := range targets {
		t := &targets[i]
		u, err := url.Parse(t.URL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("invalid adaptive target URL")
		}
		if t.ExpectedStatus == "" {
			t.ExpectedStatus = "200-299"
		}
		if _, err := utils.NewUnsignedRanges[uint16](t.ExpectedStatus); err != nil {
			return fmt.Errorf("invalid adaptive expected-status: %w", err)
		}
		if t.MinBytes < 0 || t.MinBytes > 64<<10 {
			return fmt.Errorf("adaptive min-bytes must be 0..65536")
		}
	}
	return nil
}

// probeAdaptive uses this outbound explicitly. It never changes selectors or
// passes the request back through routing rules. HTTPS verifies certificates.
func probeAdaptive(ctx context.Context, p C.ProxyAdapter, target AdaptiveTarget) (res AdaptiveProbeResult) {
	res.URL, res.Stage = target.URL, "dial"
	started := time.Now()
	defer func() { res.MS = max(1, time.Since(started).Milliseconds()) }()
	tlsConfig, err := ca.GetTLSConfig(ca.Option{})
	if err != nil {
		res.Error = "TLS configuration failed"
		return
	}
	var connected atomic.Bool
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, portText, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, err := strconv.ParseUint(portText, 10, 16)
			if err != nil {
				return nil, err
			}
			metadata := &C.Metadata{NetWork: C.TCP, Type: C.INNER, Host: host, DstPort: uint16(port)}
			if ip, err := netip.ParseAddr(host); err == nil {
				metadata.Host = ""
				metadata.DstIP = ip
			}
			conn, err := p.DialContext(ctx, metadata)
			if err == nil {
				connected.Store(true)
			}
			return conn, err
		},
		TLSClientConfig:    tlsConfig,
		DisableKeepAlives:  true,
		DisableCompression: true,
	}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err != nil {
		res.Error = "invalid request"
		return
	}
	req.Header.Set("User-Agent", "Mihomo-Adaptive-Health/1")
	// RoundTrip intentionally does not follow redirects into unrelated sites.
	resp, err := transport.RoundTrip(req)
	if err != nil {
		if connected.Load() {
			res.Stage = "tls/http"
		}
		if ctx.Err() != nil {
			res.Error = "timeout or cancellation"
		} else {
			res.Error = "connection, TLS or HTTP failure"
		}
		return
	}
	defer resp.Body.Close()
	res.Stage = "http"
	res.Status = resp.StatusCode
	expected, _ := utils.NewUnsignedRanges[uint16](target.ExpectedStatus)
	if !expected.Check(uint16(resp.StatusCode)) {
		res.Error = "unexpected HTTP status"
		return
	}
	res.Stage = "body"
	// EOF is fine for a short complete page. A reset/timeout or insufficient
	// payload is a failure, unlike the original mobile header-only verdict.
	res.Bytes, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		res.Error = "body read failed"
		return
	}
	if res.Bytes < target.MinBytes {
		res.Error = "body shorter than min-bytes"
		return
	}
	res.OK = true
	res.Stage = "ok"
	return
}
