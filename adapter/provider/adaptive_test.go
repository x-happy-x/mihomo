package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/common/structure"
	C "github.com/metacubex/mihomo/constant"
)

type memoryAdaptiveStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (s *memoryAdaptiveStore) GetStorage(k string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data[k]...)
}
func (s *memoryAdaptiveStore) SetStorage(k string, v []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[k] = append([]byte(nil), v...)
}
func adaptiveFixture(t *testing.T, confirmations int) (*adaptiveHealth, []C.Proxy, *memoryAdaptiveStore) {
	t.Helper()
	store := &memoryAdaptiveStore{data: map[string][]byte{}}
	a, err := newAdaptiveHealth("test", AdaptiveHealthOptions{Enable: true, Confirmations: confirmations, Concurrency: 1,
		Targets:       []AdaptiveTarget{{URL: "https://payload.test", ExpectedStatus: "200", MinBytes: 64}},
		DirectAllowed: []AdaptiveTarget{{URL: "https://allowed.test", ExpectedStatus: "200"}},
		DirectGlobal:  []AdaptiveTarget{{URL: "https://global.test", ExpectedStatus: "204"}},
	}, time.Second, time.Minute, store)
	if err != nil {
		t.Fatal(err)
	}
	ps := []C.Proxy{
		adapter.NewProxy(outbound.NewDirectWithOption(outbound.DirectOption{Name: "first"})),
		adapter.NewProxy(outbound.NewDirectWithOption(outbound.DirectOption{Name: "second"})),
	}
	a.setProxies(ps)
	return a, ps, store
}
func fakeAdaptiveProbe(a *adaptiveHealth, whitelist *bool, order *[]string) adaptiveProbeFunc {
	return func(ctx context.Context, p C.ProxyAdapter, target AdaptiveTarget) AdaptiveProbeResult {
		ok := true
		if p == a.direct {
			if strings.Contains(target.URL, "global") {
				ok = !*whitelist
			}
		} else {
			if order != nil {
				*order = append(*order, p.Name())
			}
			ok = (p.Name() == "first" && !*whitelist) || (p.Name() == "second" && *whitelist)
		}
		return AdaptiveProbeResult{URL: target.URL, OK: ok, MS: 50, Status: 200, Stage: "ok"}
	}
}
func TestAdaptiveModesAndSeparateRankings(t *testing.T) {
	a, ps, _ := adaptiveFixture(t, 2)
	whitelist := false
	a.probe = fakeAdaptiveProbe(a, &whitelist, nil)
	a.check(context.Background(), ps)
	if a.mode != adaptiveUnknown || len(a.records[adaptiveNormal]) != 0 {
		t.Fatal("first observation learned an unconfirmed mode")
	}
	for i := 0; i < 4; i++ {
		a.check(context.Background(), ps)
	}
	if a.mode != adaptiveNormal || !a.alive(ps[0]) || a.alive(ps[1]) {
		t.Fatal("normal health wrong")
	}
	normalChecks := a.records[adaptiveNormal][adaptiveIdentity(ps[0])].Checks
	whitelist = true
	a.check(context.Background(), ps)
	if a.alive(ps[0]) || a.alive(ps[1]) {
		t.Fatal("pending transition reused old availability")
	}
	if a.records[adaptiveNormal][adaptiveIdentity(ps[0])].Checks != normalChecks {
		t.Fatal("transition poisoned normal history")
	}
	for i := 0; i < 4; i++ {
		a.check(context.Background(), ps)
	}
	if a.mode != adaptiveWhitelist || a.order(ps)[0] != ps[1] || !a.alive(ps[1]) {
		t.Fatal("whitelist history not used")
	}
	snap := a.snapshot()
	if !snap.Rankings[adaptiveNormal][0].Stable || snap.Rankings[adaptiveNormal][0].Name != "first" {
		t.Fatal("normal stable list lost")
	}
	if !snap.Rankings[adaptiveWhitelist][0].Stable || snap.Rankings[adaptiveWhitelist][0].Name != "second" {
		t.Fatal("whitelist stable list wrong")
	}
	whitelist = false
	a.check(context.Background(), ps)
	a.check(context.Background(), ps)
	if a.order(ps)[0] != ps[0] {
		t.Fatal("return to normal did not restore normal ranking")
	}
}

func TestAdaptivePartialConnectivityNeverSelectsNode(t *testing.T) {
	a, ps, _ := adaptiveFixture(t, 1)
	a.options.Targets = append(a.options.Targets, AdaptiveTarget{URL: "https://cdn.test", ExpectedStatus: "200"})
	a.probe = func(ctx context.Context, p C.ProxyAdapter, target AdaptiveTarget) AdaptiveProbeResult {
		// First node reaches only the CDN; the second reaches both destinations.
		return AdaptiveProbeResult{URL: target.URL, OK: p == a.direct || p == ps[1] || target.URL == "https://cdn.test", MS: 20}
	}
	for range 4 {
		a.check(context.Background(), ps)
	}
	if a.alive(ps[0]) || !a.alive(ps[1]) || a.order(ps)[0] != ps[1] {
		t.Fatal("partially reachable node became eligible for automatic selection")
	}
	if a.records[adaptiveNormal][adaptiveIdentity(ps[0])].stable(time.Now()) {
		t.Fatal("partial connectivity entered stable history")
	}
	a.testProxy(context.Background(), ps[0])
	if a.alive(ps[0]) {
		t.Fatal("manual probe accepted partial connectivity")
	}
	if adaptiveTargetsOK([]AdaptiveProbeResult{{OK: true}}, 2) || adaptiveTargetsOK(nil, 0) {
		t.Fatal("missing required probes accepted")
	}
}
func TestAdaptiveStableFirstWithoutStarvation(t *testing.T) {
	a, ps, _ := adaptiveFixture(t, 1)
	whitelist := true
	a.probe = fakeAdaptiveProbe(a, &whitelist, nil)
	for i := 0; i < 4; i++ {
		a.check(context.Background(), ps)
	}
	var order []string
	a.probe = fakeAdaptiveProbe(a, &whitelist, &order)
	a.check(context.Background(), ps)
	if strings.Join(order, ",") != "second,first" {
		t.Fatalf("probe order %v", order)
	}
}
func TestAdaptiveOfflineDoesNotPoisonStats(t *testing.T) {
	a, ps, _ := adaptiveFixture(t, 1)
	whitelist := false
	a.probe = fakeAdaptiveProbe(a, &whitelist, nil)
	a.check(context.Background(), ps)
	before := a.records[adaptiveNormal][adaptiveIdentity(ps[0])].Checks
	a.probe = func(context.Context, C.ProxyAdapter, AdaptiveTarget) AdaptiveProbeResult {
		return AdaptiveProbeResult{}
	}
	a.check(context.Background(), ps)
	if a.mode != adaptiveOffline || a.alive(ps[0]) {
		t.Fatal("offline classification failed")
	}
	if a.records[adaptiveNormal][adaptiveIdentity(ps[0])].Checks != before {
		t.Fatal("offline poisoned history")
	}
}
func TestAdaptivePersistenceAndFreshness(t *testing.T) {
	a, ps, store := adaptiveFixture(t, 1)
	whitelist := false
	a.probe = fakeAdaptiveProbe(a, &whitelist, nil)
	a.check(context.Background(), ps)
	restored, err := newAdaptiveHealth("test", a.options, a.timeout, time.Minute, store)
	if err != nil {
		t.Fatal(err)
	}
	if restored.records[adaptiveNormal][adaptiveIdentity(ps[0])].Checks != 1 {
		t.Fatal("history did not survive restart")
	}
	if restored.alive(ps[0]) {
		t.Fatal("persisted rank became live availability")
	}
	a.mu.Lock()
	r := a.latest[ps[0]]
	r.At = time.Now().Add(-time.Hour)
	a.latest[ps[0]] = r
	a.mu.Unlock()
	if a.alive(ps[0]) {
		t.Fatal("stale probe treated as live")
	}
	changed := a.options
	changed.NetworkKey = "other-uplink"
	fresh, err := newAdaptiveHealth("test", changed, a.timeout, time.Minute, store)
	if err != nil || len(fresh.records[adaptiveNormal]) != 0 {
		t.Fatal("network scoping failed")
	}
}
func TestAdaptiveChangedNetworkDuringBatch(t *testing.T) {
	a, ps, _ := adaptiveFixture(t, 1)
	whitelist := false
	base := fakeAdaptiveProbe(a, &whitelist, nil)
	a.probe = func(ctx context.Context, p C.ProxyAdapter, target AdaptiveTarget) AdaptiveProbeResult {
		r := base(ctx, p, target)
		if p == ps[len(ps)-1] {
			whitelist = true
		}
		return r
	}
	a.check(context.Background(), ps)
	if len(a.records[adaptiveNormal]) != 0 || len(a.records[adaptiveWhitelist]) != 0 {
		t.Fatal("cross-mode batch contaminated history")
	}
	if a.alive(ps[0]) {
		t.Fatal("cross-mode batch remained eligible")
	}
}
func TestAdaptiveCancellation(t *testing.T) {
	a, ps, _ := adaptiveFixture(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.check(ctx, ps)
	if !a.checkedAt.IsZero() || len(a.latest) != 0 {
		t.Fatal("cancelled round changed state")
	}
}
func TestAdaptiveConcurrentSnapshotsAndChecks(t *testing.T) {
	a, ps, _ := adaptiveFixture(t, 1)
	a.options.Concurrency = 2
	whitelist := false
	a.probe = fakeAdaptiveProbe(a, &whitelist, nil)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				a.check(context.Background(), ps)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = json.Marshal(a.snapshot())
				_ = a.order(ps)
				_ = a.alive(ps[0])
			}
		}()
	}
	wg.Wait()
}
func TestAdaptivePrunesAndRecoversCorruptStorage(t *testing.T) {
	a, _, store := adaptiveFixture(t, 1)
	for i := 0; i < 600; i++ {
		a.records[adaptiveNormal][fmt.Sprint(i)] = AdaptiveRecord{Checks: 1, LastCheck: time.Now()}
	}
	a.records[adaptiveNormal]["old"] = AdaptiveRecord{LastCheck: time.Now().Add(-40 * 24 * time.Hour)}
	a.prune(time.Now())
	if len(a.records[adaptiveNormal]) != 500 {
		t.Fatal("unbounded storage")
	}
	store.SetStorage(a.storageKey, []byte("{broken"))
	b, err := newAdaptiveHealth("test", a.options, a.timeout, time.Minute, store)
	if err != nil || b.persistenceError == "" || len(b.records[adaptiveNormal]) != 0 {
		t.Fatal("corrupt history recovery failed")
	}
}
func TestAdaptiveProviderSchema(t *testing.T) {
	var schema healthCheckSchema
	decoder := structure.NewDecoder(structure.Option{TagName: "provider", WeaklyTypedInput: true})
	err := decoder.Decode(map[string]any{"enable": true, "adaptive": map[string]any{"enable": true, "confirmations": 3, "targets": []any{map[string]any{"url": "https://test.example", "min-bytes": 1024, "expected-status": "200"}}}}, &schema)
	if err != nil || !schema.Adaptive.Enable || schema.Adaptive.Confirmations != 3 || schema.Adaptive.Targets[0].MinBytes != 1024 {
		t.Fatalf("decode: %+v %v", schema, err)
	}
	a, _, _ := adaptiveFixture(t, 1)
	bad := a.options
	bad.Targets = []AdaptiveTarget{{URL: "file:///test"}}
	if _, err := newAdaptiveHealth("bad", bad, time.Second, time.Minute, nil); err == nil {
		t.Fatal("invalid target accepted")
	}
}
func TestAdaptiveRealGETAndBodyFailures(t *testing.T) {
	var mu sync.Mutex
	methods := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		switch r.URL.Path {
		case "/ok":
			_, _ = io.WriteString(w, strings.Repeat("x", 4096))
		case "/short":
			_, _ = io.WriteString(w, "short")
		case "/broken":
			w.Header().Set("Content-Length", "4096")
			_, _ = io.WriteString(w, "short")
		case "/redirect":
			w.Header().Set("Location", "/ok")
			w.WriteHeader(302)
		case "/error":
			w.WriteHeader(503)
		case "/stall":
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	defer srv.Close()
	direct := outbound.NewDirect()
	for _, test := range []struct {
		path  string
		want  bool
		stage string
	}{
		{"/ok", true, "ok"}, {"/short", false, "body"}, {"/broken", false, "body"}, {"/redirect", false, "http"}, {"/error", false, "http"}, {"/stall", false, "body"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		r := probeAdaptive(ctx, direct, AdaptiveTarget{URL: srv.URL + test.path, ExpectedStatus: "200", MinBytes: 1024})
		cancel()
		if r.OK != test.want || r.Stage != test.stage {
			t.Errorf("%s: %+v", test.path, r)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, method := range methods {
		if method != "GET" {
			t.Fatal("not a real GET")
		}
	}
	if len(methods) != 6 {
		t.Fatal("unexpected redirected request")
	}
}

func TestAdaptiveIdentitySurvivesRenameButNotProtocolChange(t *testing.T) {
	makeProxy := func(name string, tp C.AdapterType) C.Proxy {
		return adapter.NewProxy(outbound.NewBase(outbound.BaseOption{Name: name, Addr: "vpn.example:443", Type: tp}))
	}
	if adaptiveIdentity(makeProxy("old", C.Http)) != adaptiveIdentity(makeProxy("new", C.Http)) {
		t.Fatal("rename lost identity")
	}
	if adaptiveIdentity(makeProxy("old", C.Http)) == adaptiveIdentity(makeProxy("old", C.Socks5)) {
		t.Fatal("protocols shared identity")
	}
}

func TestAdaptiveManualProbeRefreshesAvailability(t *testing.T) {
	a, ps, _ := adaptiveFixture(t, 1)
	whitelist := false
	a.probe = fakeAdaptiveProbe(a, &whitelist, nil)
	a.check(context.Background(), ps)
	if a.alive(ps[1]) {
		t.Fatal("fixture second node should fail")
	}
	a.probe = func(ctx context.Context, p C.ProxyAdapter, target AdaptiveTarget) AdaptiveProbeResult {
		return AdaptiveProbeResult{URL: target.URL, OK: true, MS: 10}
	}
	a.testProxy(context.Background(), ps[1])
	if !a.alive(ps[1]) {
		t.Fatal("manual GET success did not refresh availability")
	}
	replacement := adapter.NewProxy(outbound.NewDirectWithOption(outbound.DirectOption{Name: "second"}))
	a.setProxies([]C.Proxy{replacement})
	if a.alive(ps[1]) || a.alive(replacement) {
		t.Fatal("replacement inherited availability of a different instance")
	}
}
