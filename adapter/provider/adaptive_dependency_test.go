package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
)

func dependencyFixture(t *testing.T) (*adaptiveHealth, *adaptiveHealth, []C.Proxy, []C.Proxy) {
	t.Helper()
	base, baseProxies, _ := adaptiveFixture(t, 1)
	child, childProxies, _ := adaptiveFixture(t, 1)
	for i := range baseProxies {
		mapping := map[string]any{"name": baseProxies[i].Name(), "type": "http", "server": "vpn.test", "port": 443, "password": baseProxies[i].Name()}
		var err error
		baseProxies[i], err = identifyHealthProxy(baseProxies[i], mapping)
		if err != nil {
			t.Fatal(err)
		}
		mapping["name"] = "AI | " + baseProxies[i].Name()
		childProxies[i], err = identifyHealthProxy(childProxies[i], mapping)
		if err != nil {
			t.Fatal(err)
		}
	}
	base.setProxies(baseProxies)
	child.setProxies(childProxies)
	child.options.DependsOn = "base"
	child.source = base
	return base, child, baseProxies, childProxies
}

func TestAdaptiveDependencyAdmissionRecoveryAndStats(t *testing.T) {
	base, child, ps, cs := dependencyFixture(t)
	whitelist := false
	base.probe = fakeAdaptiveProbe(base, &whitelist, nil)
	var calls []string
	child.probe = func(_ context.Context, p C.ProxyAdapter, target AdaptiveTarget) AdaptiveProbeResult {
		if p != child.direct {
			calls = append(calls, p.Name())
		}
		return AdaptiveProbeResult{URL: target.URL, OK: true, MS: 10}
	}
	child.check(context.Background(), cs)
	if len(calls) != 0 || len(child.records[adaptiveNormal]) != 0 || child.snapshot().Results[cs[0].Name()].Skipped != "dependency-not-ready" {
		t.Fatal("untested base generated service traffic or statistics")
	}
	base.check(context.Background(), ps)
	child.check(context.Background(), cs)
	if len(calls) != 1 || calls[0] != cs[0].Name() || !child.alive(cs[0]) || child.alive(cs[1]) {
		t.Fatal("base filter failed")
	}
	if child.records[adaptiveNormal][adaptiveIdentity(cs[1])].Checks != 0 {
		t.Fatal("skipped node recorded as failure")
	}
	child.testProxy(context.Background(), cs[1])
	if len(calls) != 1 {
		t.Fatal("manual check bypassed dependency")
	}
	base.options.FailureThreshold = 3
	base.probe = func(_ context.Context, p C.ProxyAdapter, target AdaptiveTarget) AdaptiveProbeResult {
		return AdaptiveProbeResult{URL: target.URL, OK: p == base.direct, MS: 10}
	}
	base.check(context.Background(), ps)
	if !base.alive(ps[0]) || child.alive(cs[0]) {
		t.Fatal("base failure grace admitted secondary checks")
	}
	child.check(context.Background(), cs)
	if len(calls) != 1 || child.records[adaptiveNormal][adaptiveIdentity(cs[0])].Checks != 1 {
		t.Fatal("skip changed service statistics")
	}
	base.probe = fakeAdaptiveProbe(base, &whitelist, nil)
	base.check(context.Background(), ps)
	child.check(context.Background(), cs)
	if len(calls) != 2 || !child.alive(cs[0]) {
		t.Fatal("recovered node did not return")
	}
	// Removing a subscription member invalidates admission immediately.
	base.setProxies(ps[1:])
	if child.alive(cs[0]) {
		t.Fatal("removed member still admitted")
	}
	base.setProxies(ps)
	child.testProxy(context.Background(), cs[0])
	if len(calls) != 2 {
		t.Fatal("re-added untested member inherited admission")
	}
	base.check(context.Background(), ps)
	r := base.latest[ps[0]]
	r.At = time.Now().Add(-base.freshness - time.Second)
	base.latest[ps[0]] = r
	child.check(context.Background(), cs)
	if len(calls) != 2 {
		t.Fatal("stale success admitted secondary checks")
	}
}

func TestAdaptiveDependencyLostDuringServiceCheck(t *testing.T) {
	base, child, ps, cs := dependencyFixture(t)
	whitelist := false
	base.probe = fakeAdaptiveProbe(base, &whitelist, nil)
	base.check(context.Background(), ps)
	child.probe = func(_ context.Context, p C.ProxyAdapter, target AdaptiveTarget) AdaptiveProbeResult {
		if p != child.direct {
			base.observe(adaptiveWhitelist, time.Now())
		}
		return AdaptiveProbeResult{URL: target.URL, OK: true, MS: 10}
	}
	child.check(context.Background(), cs)
	if len(child.records[adaptiveNormal]) != 0 || child.alive(cs[0]) {
		t.Fatal("source transition during probe contaminated history")
	}
}

func TestAdaptiveDependencyAlsoGatesLegacyHEAD(t *testing.T) {
	_, child, _, cs := dependencyFixture(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(204) }))
	defer srv.Close()
	child.probe = func(_ context.Context, _ C.ProxyAdapter, target AdaptiveTarget) AdaptiveProbeResult {
		return AdaptiveProbeResult{URL: target.URL, OK: true, MS: 10}
	}
	hc := NewHealthCheck(cs, srv.URL, 500, 300, false, nil)
	defer hc.close()
	hc.adaptive = child
	hc.check()
	if requests.Load() != 0 {
		t.Fatal("legacy HEAD bypassed unavailable base")
	}
}

func TestAdaptiveDependencyConcurrentBaseAndService(t *testing.T) {
	base, child, ps, cs := dependencyFixture(t)
	whitelist := false
	base.probe = fakeAdaptiveProbe(base, &whitelist, nil)
	child.probe = func(_ context.Context, _ C.ProxyAdapter, target AdaptiveTarget) AdaptiveProbeResult {
		return AdaptiveProbeResult{URL: target.URL, OK: true, MS: 10}
	}
	var wg sync.WaitGroup
	for _, job := range []func(){func() { base.check(context.Background(), ps) }, func() { child.check(context.Background(), cs) }, func() { child.snapshot(); child.alive(cs[0]) }} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				job()
			}
		}()
	}
	wg.Wait()
}

func TestAdaptiveDependencyConnectionIdentity(t *testing.T) {
	parse := func(prefix, password string) C.Proxy {
		t.Helper()
		parser, err := NewProxiesParser("test", nil, "", "", "", "", overrideSchema{AdditionalPrefix: &prefix}, "")
		if err != nil {
			t.Fatal(err)
		}
		ps, err := parser([]byte("proxies:\n  - name: node\n    type: http\n    server: 127.0.0.1\n    port: 1234\n    username: test\n    password: " + password + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		return ps[0]
	}
	base, child, other := parse("", "one"), parse("AI | ", "one"), parse("AI | ", "two")
	if healthProxyKey(base) == "" || healthProxyKey(base) != healthProxyKey(child) || healthProxyKey(base) == healthProxyKey(other) {
		t.Fatal("identity confused prefix or credentials")
	}
	data, err := json.Marshal(child)
	if err != nil || strings.Contains(string(data), healthProxyKey(child)) {
		t.Fatal("internal fingerprint leaked into API")
	}
}

func TestAdaptiveDependencyConfigValidation(t *testing.T) {
	makeProvider := func(name, dependency string, enable bool) P.ProxyProvider {
		t.Helper()
		pd, err := ParseProxyProvider(name, map[string]any{"type": "inline", "payload": []map[string]any{{"name": name, "type": "direct"}},
			"health-check": map[string]any{"enable": true, "adaptive": map[string]any{"enable": enable, "depends-on": dependency}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pd.(interface{ Close() error }).Close() })
		return pd
	}
	for _, test := range []struct {
		name, baseDep, childDep string
		baseEnabled, valid      bool
	}{
		{"valid", "", "base", true, true}, {"missing", "", "missing", true, false},
		{"disabled", "", "base", false, false}, {"self", "", "child", true, false},
		{"cycle", "child", "base", true, false}, {"chain", "root", "base", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			providers := map[string]P.ProxyProvider{"root": makeProvider("root", "", true), "base": makeProvider("base", test.baseDep, test.baseEnabled), "child": makeProvider("child", test.childDep, true)}
			err := ResolveAdaptiveDependencies(providers)
			if (err == nil) != test.valid {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}
