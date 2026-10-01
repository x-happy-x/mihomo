package outboundgroup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/callback"
	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
)

type FallbackOption struct {
	HealthCheckURLs []string `group:"health-check-urls,omitempty"`
}

type Fallback struct {
	*GroupBase
	disableUDP      bool
	testUrl         string
	selected        string
	expectedStatus  string
	stateMu         sync.Mutex
	current         string
	healthCheckURLs []string
}

func (f *Fallback) Now() string {
	proxy := f.findAliveProxy(false)
	return proxy.Name()
}

// DialContext implements C.ProxyAdapter
func (f *Fallback) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	proxy := f.findAliveProxy(true)
	c, err := proxy.DialContext(ctx, metadata)
	if err == nil {
		c.AppendToChains(f)
	} else {
		f.onDialFailed(proxy.Type(), err, f.healthCheck)
	}

	if N.NeedHandshake(c) {
		c = callback.NewFirstWriteCallBackConn(c, func(err error) {
			if err == nil {
				f.onDialSuccess()
			} else {
				f.onDialFailed(proxy.Type(), err, f.healthCheck)
			}
		})
	}

	return c, err
}

// ListenPacketContext implements C.ProxyAdapter
func (f *Fallback) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (C.PacketConn, error) {
	proxy := f.findAliveProxy(true)
	pc, err := proxy.ListenPacketContext(ctx, metadata)
	if err == nil {
		pc.AppendToChains(f)
	}

	return pc, err
}

// SupportUDP implements C.ProxyAdapter
func (f *Fallback) SupportUDP() bool {
	if f.disableUDP {
		return false
	}

	proxy := f.findAliveProxy(false)
	return proxy.SupportUDP()
}

// IsL3Protocol implements C.ProxyAdapter
func (f *Fallback) IsL3Protocol(metadata *C.Metadata) bool {
	return f.findAliveProxy(false).IsL3Protocol(metadata)
}

// MarshalJSON implements C.ProxyAdapter
func (f *Fallback) MarshalJSON() ([]byte, error) {
	all := []string{}
	for _, proxy := range f.GetProxies(false) {
		all = append(all, proxy.Name())
	}
	now := f.Now()
	f.stateMu.Lock()
	selected := f.selected
	f.stateMu.Unlock()
	return json.Marshal(map[string]any{
		"type":            f.Type().String(),
		"now":             now,
		"all":             all,
		"testUrl":         f.testUrl,
		"expectedStatus":  f.expectedStatus,
		"fixed":           selected,
		"healthCheckUrls": f.healthCheckURLs,
		"hidden":          f.Hidden(),
		"icon":            f.Icon(),
		"emptyFallback":   f.EmptyFallback().Name(),
	})
}

// Unwrap implements C.ProxyAdapter
func (f *Fallback) Unwrap(metadata *C.Metadata, touch bool) C.Proxy {
	proxy := f.findAliveProxy(touch)
	return proxy
}

func (f *Fallback) findAliveProxy(touch bool) C.Proxy {
	proxies := f.GetProxies(touch)
	f.stateMu.Lock()
	defer f.stateMu.Unlock()
	var selected, current, firstAlive C.Proxy
	for _, proxy := range proxies {
		if proxy.Name() == f.selected {
			selected = proxy
		}
		if proxy.Name() == f.current {
			current = proxy
		}
		if firstAlive == nil && f.alive(proxy) {
			firstAlive = proxy
		}
	}
	var chosen C.Proxy
	switch {
	case selected != nil && f.alive(selected):
		chosen = selected
	case firstAlive != nil:
		f.selected = ""
		chosen = firstAlive
	case selected != nil:
		// Failure of a test endpoint does not prove the tunnel is unusable.
		chosen = selected
	case current != nil:
		f.selected = ""
		chosen = current
	default:
		f.selected = ""
		chosen = proxies[0]
	}
	f.current = chosen.Name()
	return chosen
}

func (f *Fallback) alive(proxy C.Proxy) bool {
	if proxy.AliveForTestUrl(f.testUrl) {
		return true
	}
	states := proxy.ExtraDelayHistories()
	for _, url := range f.healthCheckURLs {
		// Unknown URLs inherit global liveness; require an actual test result.
		if state, ok := states[url]; ok && len(state.History) > 0 && state.Alive {
			return true
		}
	}
	return false
}

func (f *Fallback) Set(name string) error {
	var p C.Proxy
	for _, proxy := range f.GetProxies(false) {
		if proxy.Name() == name {
			p = proxy
			break
		}
	}

	if p == nil {
		return errors.New("proxy not exist")
	}

	if !f.alive(p) {
		expectedStatus, _ := utils.NewUnsignedRanges[uint16](f.expectedStatus)
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*time.Duration(f.testTimeout))
		defer cancel()
		var wg sync.WaitGroup
		for _, url := range append([]string{f.testUrl}, f.healthCheckURLs...) {
			wg.Add(1)
			go func(url string) {
				defer wg.Done()
				_, _ = p.URLTest(ctx, url, expectedStatus)
			}(url)
		}
		wg.Wait()
	}
	f.ForceSet(name)

	return nil
}

func (f *Fallback) ForceSet(name string) {
	f.stateMu.Lock()
	defer f.stateMu.Unlock()
	f.selected = name
}

func (f *Fallback) Providers() []P.ProxyProvider {
	return f.providers
}

func (f *Fallback) Proxies() []C.Proxy {
	return f.GetProxies(false)
}

func NewFallback(option GroupCommonOption, fallbackOption FallbackOption, emptyFallback C.Proxy, providers []P.ProxyProvider) (*Fallback, error) {
	if len(fallbackOption.HealthCheckURLs) > 4 {
		return nil, fmt.Errorf("fallback %s: at most 4 additional health-check URLs are allowed", option.Name)
	}
	urls := make([]string, 0, len(fallbackOption.HealthCheckURLs))
	seen := map[string]bool{option.URL: true}
	for _, raw := range fallbackOption.HealthCheckURLs {
		u := strings.TrimSpace(raw)
		parsed, err := url.Parse(u)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			return nil, fmt.Errorf("fallback %s: invalid health-check URL %q", option.Name, raw)
		}
		if !seen[u] {
			urls = append(urls, u)
			seen[u] = true
		}
	}
	expectedStatus, err := utils.NewUnsignedRanges[uint16](option.ExpectedStatus)
	if err != nil {
		return nil, err
	}
	interval := option.Interval
	if interval <= 0 {
		interval = 300
	}
	for _, u := range urls {
		for _, pd := range providers {
			filter := option.Filter
			if pd.VehicleType() == P.Compatible {
				filter = ""
			}
			pd.RegisterHealthCheckTask(u, expectedStatus, filter, uint(interval))
		}
	}

	return &Fallback{
		GroupBase: NewGroupBase(GroupBaseOption{
			Name:           option.Name,
			Type:           C.Fallback,
			Hidden:         option.Hidden,
			Icon:           option.Icon,
			Filter:         option.Filter,
			ExcludeFilter:  option.ExcludeFilter,
			ExcludeType:    option.ExcludeType,
			TestTimeout:    option.TestTimeout,
			MaxFailedTimes: option.MaxFailedTimes,
			EmptyFallback:  emptyFallback,
			Providers:      providers,
		}),
		disableUDP:      option.DisableUDP,
		testUrl:         option.URL,
		expectedStatus:  option.ExpectedStatus,
		healthCheckURLs: urls,
	}, nil
}
