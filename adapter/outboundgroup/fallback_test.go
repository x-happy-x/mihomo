package outboundgroup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/adapter/provider"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
)

type fallbackProxy struct {
	C.Proxy
	name    string
	mu      sync.Mutex
	states  map[string]C.ProxyState
	results map[string]bool
}

func (p *fallbackProxy) Name() string            { return p.name }
func (p *fallbackProxy) Adapter() C.ProxyAdapter { return nil }
func (p *fallbackProxy) AliveForTestUrl(url string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.states[url].Alive
}
func (p *fallbackProxy) ExtraDelayHistories() map[string]C.ProxyState {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]C.ProxyState{}
	for u, s := range p.states {
		out[u] = s
	}
	return out
}
func (p *fallbackProxy) state(url string, alive bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states[url] = C.ProxyState{Alive: alive, History: []C.DelayHistory{{Time: time.Now(), Delay: 10}}}
}
func (p *fallbackProxy) URLTest(ctx context.Context, url string, status utils.IntRanges[uint16]) (uint16, error) {
	alive := p.results[url]
	p.state(url, alive)
	if !alive {
		return 0, errors.New("endpoint unavailable")
	}
	return 10, nil
}
func (p *fallbackProxy) LastDelayForTestUrl(url string) uint16 { return 10 }

func fallbackFixture(t *testing.T, extra ...string) (*Fallback, *fallbackProxy, *fallbackProxy) {
	t.Helper()
	a := &fallbackProxy{name: "first", states: map[string]C.ProxyState{}}
	b := &fallbackProxy{name: "working", states: map[string]C.ProxyState{}}
	ps := []C.Proxy{a, b}
	hc := provider.NewHealthCheck(ps, "https://primary.test", 100, 300, false, nil)
	pd, err := provider.NewCompatibleProvider("nodes", ps, hc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pd.Close() })
	f, err := NewFallback(GroupCommonOption{Name: "fallback", URL: "https://primary.test", TestTimeout: 100}, FallbackOption{HealthCheckURLs: extra}, a, []P.ProxyProvider{pd})
	if err != nil {
		t.Fatal(err)
	}
	return f, a, b
}
func assertNow(t *testing.T, f *Fallback, want string) {
	t.Helper()
	if got := f.Now(); got != want {
		t.Fatalf("Now = %q, want %q", got, want)
	}
}
func TestFallbackKeepsCurrentWhenAllTestsFail(t *testing.T) {
	f, a, b := fallbackFixture(t)
	b.state(f.testUrl, true)
	assertNow(t, f, b.name)
	b.state(f.testUrl, false)
	assertNow(t, f, b.name)
	a.state(f.testUrl, true)
	assertNow(t, f, a.name)
}
func TestFallbackManualSelectionAndRecovery(t *testing.T) {
	f, a, b := fallbackFixture(t)
	if err := f.Set(b.name); err != nil {
		t.Fatal(err)
	}
	assertNow(t, f, b.name)
	assertNow(t, f, b.name)
	a.state(f.testUrl, true)
	assertNow(t, f, a.name)
	if f.selected != "" {
		t.Fatal("failed selection should clear once an alternative is alive")
	}
	if err := f.Set("missing"); err == nil {
		t.Fatal("missing selection accepted")
	}
	assertNow(t, f, a.name)
}
func TestFallbackFindsEarlierHealthyAlternative(t *testing.T) {
	f, a, b := fallbackFixture(t)
	a.state(f.testUrl, true)
	f.ForceSet(b.name)
	assertNow(t, f, a.name)
}
func TestFallbackHonorsHealthyManualSelection(t *testing.T) {
	f, a, b := fallbackFixture(t)
	a.state(f.testUrl, true)
	b.state(f.testUrl, true)
	if err := f.Set(b.name); err != nil {
		t.Fatal(err)
	}
	assertNow(t, f, b.name)
	f.ForceSet("")
	assertNow(t, f, a.name)
}
func TestFallbackAdditionalEndpoint(t *testing.T) {
	extra := "https://secondary.test"
	f, a, b := fallbackFixture(t, extra)
	b.results = map[string]bool{extra: true}
	if err := f.Set(b.name); err != nil {
		t.Fatal(err)
	}
	a.state(f.testUrl, true)
	assertNow(t, f, b.name)
	b.state(extra, false)
	assertNow(t, f, a.name)
}
func TestFallbackUnknownExtraIsNotSuccess(t *testing.T) {
	f, _, b := fallbackFixture(t, "https://secondary.test")
	if f.alive(b) {
		t.Fatal("untested endpoint accepted")
	}
}
func TestFallbackRemovedSelection(t *testing.T) {
	f, a, b := fallbackFixture(t)
	f.ForceSet(b.name)
	assertNow(t, f, b.name)
	// Replace provider list with a new snapshot, as after subscription refresh.
	hc := provider.NewHealthCheck([]C.Proxy{a}, f.testUrl, 100, 300, false, nil)
	pd, err := provider.NewCompatibleProvider("replacement", []C.Proxy{a}, hc)
	if err != nil {
		t.Fatal(err)
	}
	defer pd.Close()
	f.providers = []P.ProxyProvider{pd}
	f.providerVersions = nil
	assertNow(t, f, a.name)
}
func TestFallbackConcurrentSelectionAndAPI(t *testing.T) {
	f, a, b := fallbackFixture(t)
	a.state(f.testUrl, true)
	b.state(f.testUrl, true)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				f.ForceSet(b.name)
				_ = f.Now()
				_, err := json.Marshal(f)
				if err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}
func TestFallbackParserRegistersExtraChecks(t *testing.T) {
	a := &fallbackProxy{name: "node", states: map[string]C.ProxyState{}, results: map[string]bool{"https://secondary.test": true}}
	providers := map[string]P.ProxyProvider{}
	g, err := ParseProxyGroup(map[string]any{
		"name": "test", "type": "fallback", "proxies": []string{"node"}, "url": "https://primary.test",
		"health-check-urls": []string{"https://secondary.test", "https://secondary.test"}, "filter": "does-not-match",
	}, map[string]C.Proxy{"node": a, "COMPATIBLE": a}, providers, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer providers["test"].(*provider.CompatibleProvider).Close()
	providers["test"].HealthCheck()
	f := g.(*Fallback)
	if len(f.healthCheckURLs) != 1 || !f.alive(a) {
		t.Fatal("extra check not registered, deduplicated or executed")
	}
}
func TestFallbackRejectsInvalidEndpoint(t *testing.T) {
	for _, u := range []string{"", "ftp://host", "https://"} {
		if _, err := NewFallback(GroupCommonOption{Name: "test"}, FallbackOption{HealthCheckURLs: []string{u}}, nil, nil); err == nil {
			t.Fatalf("accepted %q", u)
		}
	}
}

func TestFallbackRealHTTPStatusChecks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/secondary" {
			w.WriteHeader(204)
		} else {
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	p := adapter.NewProxy(outbound.NewDirect())
	providers := map[string]P.ProxyProvider{}
	g, err := ParseProxyGroup(map[string]any{
		"name": "http-test", "type": "fallback", "proxies": []string{p.Name()},
		"url": srv.URL + "/primary", "health-check-urls": []string{srv.URL + "/secondary"},
		"expected-status": "204",
	}, map[string]C.Proxy{p.Name(): p, "COMPATIBLE": p}, providers, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer providers["http-test"].(*provider.CompatibleProvider).Close()
	providers["http-test"].HealthCheck()
	f := g.(*Fallback)
	if p.AliveForTestUrl(f.testUrl) {
		t.Fatal("503 should fail expected 204")
	}
	if !f.alive(p) {
		t.Fatal("secondary 204 should keep the proxy eligible")
	}
}

type adaptiveFallbackProvider struct {
	P.ProxyProvider
	status      map[C.Proxy]bool
	manualTests int
}

func (p *adaptiveFallbackProvider) AdaptiveProxyAlive(proxy C.Proxy) (bool, bool) {
	value, exists := p.status[proxy]
	return value, exists
}
func (p *adaptiveFallbackProvider) TestAdaptiveProxy(ctx context.Context, proxy C.Proxy) bool {
	if _, exists := p.status[proxy]; !exists {
		return false
	}
	p.manualTests++
	p.status[proxy] = true
	return true
}
func TestFallbackUsesAdaptiveReachabilityAndManualProbe(t *testing.T) {
	f, a, b := fallbackFixture(t)
	a.state(f.testUrl, true) // HEAD succeeds, real GET does not.
	pd := &adaptiveFallbackProvider{ProxyProvider: f.providers[0], status: map[C.Proxy]bool{a: false, b: true}}
	f.providers = []P.ProxyProvider{pd}
	assertNow(t, f, b.name)
	if err := f.Set(a.name); err != nil {
		t.Fatal(err)
	}
	if pd.manualTests != 1 {
		t.Fatal("manual selection did not run the adaptive probe")
	}
	assertNow(t, f, a.name)
	pd.status[a], pd.status[b] = false, false
	assertNow(t, f, a.name)
}
