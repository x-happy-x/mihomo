package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/common/structure"
	C "github.com/metacubex/mihomo/constant"
)

func TestAdaptiveServiceResponsePolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch r.URL.Path {
		case "/ok":
			io.WriteString(w, `{"service":"ready"}`)
		case "/blocked":
			io.WriteString(w, `{"service":"ready","error":"unsupported_country"}`)
		case "/challenge":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html>service ready</html>`)
		case "/wrong":
			io.WriteString(w, `{"other":"service"}`)
		case "/forbidden":
			w.WriteHeader(403)
			io.WriteString(w, `{"service":"ready"}`)
		case "/stall":
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	defer srv.Close()
	for _, test := range []struct{ path, errorText string }{
		{"/ok", ""}, {"/blocked", "body matched forbidden pattern"},
		{"/challenge", "unexpected Content-Type"}, {"/wrong", "body did not match required pattern"},
		{"/forbidden", "unexpected HTTP status"}, {"/stall", "body read failed"},
	} {
		t.Run(test.path, func(t *testing.T) {
			targets := []AdaptiveTarget{{URL: srv.URL + test.path, ExpectedStatus: "200", Timeout: 100,
				ContentType: "application/json", BodyRegex: `"service"\s*:\s*"ready"`, BodyNotRegex: `(?i)unsupported_country`}}
			if err := validateAdaptiveTargets(targets); err != nil {
				t.Fatal(err)
			}
			r := probeAdaptive(context.Background(), outbound.NewDirect(), targets[0])
			if r.OK != (test.errorText == "") || r.Error != test.errorText {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestAdaptiveServiceConfigDecodeAndValidation(t *testing.T) {
	var schema healthCheckSchema
	err := structure.NewDecoder(structure.Option{TagName: "provider", WeaklyTypedInput: true}).Decode(map[string]any{
		"enable": true,
		"adaptive": map[string]any{"enable": true, "failure-threshold": 3, "recovery-threshold": 2,
			"targets": []any{map[string]any{"url": "https://service.test/", "timeout": 8000,
				"content-type": "application/json", "body-regex": "ready", "body-not-regex": "blocked"}}}}, &schema)
	if err != nil {
		t.Fatal(err)
	}
	a, err := newAdaptiveHealth("service", schema.Adaptive, time.Second, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := a.options.Targets[0]
	if target.Timeout != 8000 || target.bodyRE == nil || target.bodyNotRE == nil || a.options.FailureThreshold != 3 || a.options.RecoveryThreshold != 2 {
		t.Fatalf("decode: %+v", a.options)
	}
	for _, target := range []AdaptiveTarget{
		{URL: "https://test/", BodyRegex: "["}, {URL: "https://test/", BodyNotRegex: "["},
		{URL: "https://test/", BodyRegex: strings.Repeat("x", 4097)},
		{URL: "https://test/", Timeout: -1}, {URL: "https://test/", Timeout: 60001},
		{URL: "https://test/", ContentType: "text/html; charset=utf-8"},
	} {
		if validateAdaptiveTargets([]AdaptiveTarget{target}) == nil {
			t.Fatalf("invalid target accepted: %+v", target)
		}
	}
	for _, threshold := range []int{-1, 11} {
		bad := a.options
		bad.FailureThreshold = threshold
		if _, err := newAdaptiveHealth("bad", bad, time.Second, time.Minute, nil); err == nil {
			t.Fatal("invalid failure threshold")
		}
		bad = a.options
		bad.RecoveryThreshold = threshold
		if _, err := newAdaptiveHealth("bad", bad, time.Second, time.Minute, nil); err == nil {
			t.Fatal("invalid recovery threshold")
		}
	}
}

func TestAdaptiveServiceThresholdsAndExpiry(t *testing.T) {
	a, ps, _ := adaptiveFixture(t, 1)
	a.options.FailureThreshold, a.options.RecoveryThreshold = 3, 2
	ok := true
	a.probe = func(ctx context.Context, p C.ProxyAdapter, target AdaptiveTarget) AdaptiveProbeResult {
		return AdaptiveProbeResult{URL: target.URL, OK: p == a.direct || ok, MS: 10}
	}
	a.check(context.Background(), ps)
	if a.alive(ps[0]) {
		t.Fatal("recovery threshold ignored")
	}
	a.check(context.Background(), ps)
	if !a.alive(ps[0]) {
		t.Fatal("recovery failed")
	}
	ok = false
	for i := 1; i <= 3; i++ {
		a.check(context.Background(), ps)
		if a.alive(ps[0]) != (i < 3) || a.latest[ps[0]].OK || a.latest[ps[0]].Failures != i {
			t.Fatalf("failure %d: %+v", i, a.latest[ps[0]])
		}
	}
	ok = true
	a.testProxy(context.Background(), ps[0])
	if a.alive(ps[0]) {
		t.Fatal("manual check bypassed recovery threshold")
	}
	a.testProxy(context.Background(), ps[0])
	if !a.alive(ps[0]) {
		t.Fatal("manual recovery failed")
	}
	r := a.latest[ps[0]]
	r.At = time.Now().Add(-a.freshness - time.Second)
	a.latest[ps[0]] = r
	ok = false
	a.testProxy(context.Background(), ps[0])
	if a.alive(ps[0]) || a.latest[ps[0]].Failures != 1 {
		t.Fatal("expired success survived into grace period")
	}
	// A network transition must also invalidate both availability and streaks.
	a.observe(adaptiveWhitelist, time.Now())
	if len(a.latest) != 0 {
		t.Fatal("network transition retained streak")
	}
}

func TestAdaptiveTargetTimeoutOverridesAndParentDeadline(t *testing.T) {
	a, ps, _ := adaptiveFixture(t, 1)
	a.timeout = time.Second
	a.probe = func(ctx context.Context, _ C.ProxyAdapter, target AdaptiveTarget) AdaptiveProbeResult {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("unbounded probe")
		}
		remaining := time.Until(deadline)
		if target.Timeout == 3000 && (remaining < 2*time.Second || remaining > 3*time.Second) {
			t.Fatalf("override capped: %v", remaining)
		}
		if target.Timeout == 0 && remaining > time.Second {
			t.Fatal("default timeout ignored")
		}
		return AdaptiveProbeResult{OK: true}
	}
	a.probeTargets(context.Background(), ps[0], []AdaptiveTarget{{Timeout: 3000}, {}})
	a.probe = func(ctx context.Context, _ C.ProxyAdapter, _ AdaptiveTarget) AdaptiveProbeResult {
		deadline, _ := ctx.Deadline()
		if time.Until(deadline) > 100*time.Millisecond {
			t.Fatal("parent deadline extended")
		}
		return AdaptiveProbeResult{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	a.probeTargets(ctx, ps[0], []AdaptiveTarget{{Timeout: 3000}})
}

func TestAdaptiveServicesUseIndependentPoliciesAndHistory(t *testing.T) {
	a, ps, store := adaptiveFixture(t, 1)
	// Golden key from the pre-MIHOMO-5 policy: optional defaults must not
	// discard ratings earned under the identical response policy.
	if a.storageKey != "adaptive:38437e3f1943ddda8d7ad20c8dfa1e52591c70229d2709ac" {
		t.Fatal("default policy no longer restores legacy history")
	}
	whitelist := false
	a.probe = fakeAdaptiveProbe(a, &whitelist, nil)
	a.check(context.Background(), ps)
	b, err := newAdaptiveHealth("other-service", a.options, a.timeout, time.Minute, store)
	if err != nil {
		t.Fatal(err)
	}
	if a.storageKey == b.storageKey || len(b.records[adaptiveNormal]) != 0 {
		t.Fatal("services shared history")
	}
	changed := a.options
	changed.Targets = []AdaptiveTarget{{URL: "https://payload.test", BodyRegex: "ready"}}
	b, err = newAdaptiveHealth("test", changed, a.timeout, time.Minute, store)
	if err != nil || a.storageKey == b.storageKey {
		t.Fatal("changed policy reused history")
	}
}
