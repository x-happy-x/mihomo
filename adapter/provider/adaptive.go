package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/component/profile/cachefile"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
	"golang.org/x/sync/errgroup"
)

const (
	adaptiveNormal    = "normal"
	adaptiveWhitelist = "whitelist"
	adaptiveUnknown   = "unknown"
	adaptiveOffline   = "offline"
	adaptiveRecordTTL = 30 * 24 * time.Hour
)

// AdaptiveHealthOptions is opt-in and local to a provider. NetworkKey separates
// history for different uplinks; it is a label, not a network control setting.
type AdaptiveHealthOptions struct {
	Enable        bool             `provider:"enable,omitempty" json:"enable"`
	NetworkKey    string           `provider:"network-key,omitempty" json:"networkKey"`
	Confirmations int              `provider:"confirmations,omitempty" json:"confirmations"`
	Concurrency   int              `provider:"concurrency,omitempty" json:"concurrency"`
	Targets       []AdaptiveTarget `provider:"targets,omitempty" json:"targets"`
	DirectAllowed []AdaptiveTarget `provider:"direct-allowed,omitempty" json:"directAllowed"`
	DirectGlobal  []AdaptiveTarget `provider:"direct-global,omitempty" json:"directGlobal"`
}

type AdaptiveRecord struct {
	Name      string    `json:"name"`
	OK        float64   `json:"ok"`
	Fail      float64   `json:"fail"`
	AvgMS     float64   `json:"avgMs"`
	LastMS    int64     `json:"lastMs"`
	Checks    uint64    `json:"checks"`
	LastOK    time.Time `json:"lastOk"`
	LastCheck time.Time `json:"lastCheck"`
}

func (r AdaptiveRecord) rate() float64 { return (r.OK + 1) / (r.OK + r.Fail + 2) }
func (r AdaptiveRecord) score(now time.Time) float64 {
	if r.Checks == 0 || now.Sub(r.LastCheck) > adaptiveRecordTTL {
		return .9
	}
	latency := r.AvgMS
	if latency <= 0 {
		latency = 800
	}
	return r.rate() * 1000 / (latency + 100)
}
func (r AdaptiveRecord) stable(now time.Time) bool {
	return r.Checks >= 3 && r.rate() >= .7 && r.LastMS > 0 && now.Sub(r.LastCheck) <= 7*24*time.Hour
}
func (r AdaptiveRecord) record(name string, ok bool, ms int64, now time.Time) AdaptiveRecord {
	r.Name = name
	r.OK *= .9
	r.Fail *= .9
	r.Checks++
	r.LastCheck = now
	if ok {
		r.OK++
		r.LastMS = max(1, ms)
		r.LastOK = now
		if r.AvgMS <= 0 {
			r.AvgMS = float64(r.LastMS)
		} else {
			r.AvgMS = r.AvgMS*.7 + float64(r.LastMS)*.3
		}
	} else {
		r.Fail++
		r.LastMS = -1
	}
	return r
}

type AdaptiveNodeResult struct {
	Mode   string                `json:"mode"`
	At     time.Time             `json:"at"`
	OK     bool                  `json:"ok"`
	Probes []AdaptiveProbeResult `json:"probes"`
}

type AdaptiveNodeSummary struct {
	Name        string         `json:"name"`
	Record      AdaptiveRecord `json:"record"`
	SuccessRate float64        `json:"successRate"`
	Score       float64        `json:"score"`
	Stable      bool           `json:"stable"`
}

type AdaptiveSnapshot struct {
	Mode             string                           `json:"mode"`
	Observed         string                           `json:"observed"`
	Pending          int                              `json:"pending"`
	CheckedAt        time.Time                        `json:"checkedAt"`
	DirectAllowed    []AdaptiveProbeResult            `json:"directAllowed"`
	DirectGlobal     []AdaptiveProbeResult            `json:"directGlobal"`
	Rankings         map[string][]AdaptiveNodeSummary `json:"rankings"`
	Results          map[string]AdaptiveNodeResult    `json:"results"`
	PersistenceError string                           `json:"persistenceError,omitempty"`
}

type adaptiveStore interface {
	GetStorage(string) []byte
	SetStorage(string, []byte)
}
type adaptiveProbeFunc func(context.Context, C.ProxyAdapter, AdaptiveTarget) AdaptiveProbeResult

type adaptiveHealth struct {
	mu               sync.RWMutex
	runMu            sync.Mutex
	options          AdaptiveHealthOptions
	timeout          time.Duration
	freshness        time.Duration
	revision         atomic.Uint32
	mode             string
	observed         string
	pending          int
	checkedAt        time.Time
	allowed, global  []AdaptiveProbeResult
	records          map[string]map[string]AdaptiveRecord
	latest           map[C.Proxy]AdaptiveNodeResult
	proxies          []C.Proxy
	members          map[C.Proxy]bool
	store            adaptiveStore
	storageKey       string
	persistenceError string
	direct           C.ProxyAdapter
	probe            adaptiveProbeFunc
}

func newAdaptiveHealth(name string, options AdaptiveHealthOptions, timeout, interval time.Duration, store adaptiveStore) (*adaptiveHealth, error) {
	if options.Confirmations == 0 {
		options.Confirmations = 2
	}
	if options.Concurrency == 0 {
		options.Concurrency = 4
	}
	if options.Confirmations < 1 || options.Confirmations > 10 || options.Concurrency < 1 || options.Concurrency > 10 {
		return nil, fmt.Errorf("adaptive confirmations/concurrency must be 1..10")
	}
	if len(options.NetworkKey) > 128 {
		return nil, fmt.Errorf("adaptive network-key too long")
	}
	if options.Targets == nil {
		options.Targets = []AdaptiveTarget{
			{URL: "https://www.google.com/", ExpectedStatus: "200", MinBytes: 1024},
			{URL: "https://www.cloudflare.com/cdn-cgi/trace", ExpectedStatus: "200", MinBytes: 64},
		}
	}
	if options.DirectAllowed == nil {
		options.DirectAllowed = []AdaptiveTarget{{URL: "https://ya.ru", ExpectedStatus: "200-399"}}
	}
	if options.DirectGlobal == nil {
		options.DirectGlobal = []AdaptiveTarget{
			{URL: "https://www.gstatic.com/generate_204", ExpectedStatus: "204"},
			{URL: "https://cp.cloudflare.com/generate_204", ExpectedStatus: "204"},
		}
	}
	for _, targets := range [][]AdaptiveTarget{options.Targets, options.DirectAllowed, options.DirectGlobal} {
		if err := validateAdaptiveTargets(targets); err != nil {
			return nil, err
		}
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if interval <= 0 {
		interval = 300 * time.Second
	}
	// A changed probe policy must not inherit a rating earned under another one.
	payload, _ := json.Marshal(struct {
		Name    string
		Options AdaptiveHealthOptions
		Policy  string
	}{name, options, "all-targets-v2"})
	hash := sha256.Sum256(payload)
	a := &adaptiveHealth{
		options: options, timeout: timeout, freshness: max(2*interval, 2*timeout*time.Duration(len(options.Targets))),
		mode: adaptiveUnknown, observed: adaptiveUnknown,
		records: map[string]map[string]AdaptiveRecord{adaptiveNormal: {}, adaptiveWhitelist: {}}, latest: map[C.Proxy]AdaptiveNodeResult{}, members: map[C.Proxy]bool{},
		storageKey: "adaptive:" + hex.EncodeToString(hash[:24]), store: store,
		direct: outbound.NewDirect(), probe: probeAdaptive,
	}
	if store != nil {
		var saved map[string]map[string]AdaptiveRecord
		if data := store.GetStorage(a.storageKey); len(data) > 0 {
			if err := json.Unmarshal(data, &saved); err != nil {
				a.persistenceError = "invalid saved statistics; started fresh"
			} else {
				for _, mode := range []string{adaptiveNormal, adaptiveWhitelist} {
					for key, r := range saved[mode] {
						if r.Checks > 0 && r.OK >= 0 && r.Fail >= 0 && r.AvgMS >= 0 && time.Since(r.LastCheck) <= adaptiveRecordTTL {
							a.records[mode][key] = r
						}
					}
				}
			}
		}
	}
	return a, nil
}

func adaptiveIdentity(p C.Proxy) string {
	// Match KVN's address identity across subscription renames, but keep protocols
	// separate. Provider/network/policy scoping is part of the storage key.
	address := strings.ToLower(p.Addr())
	if address == "" {
		address = "name:" + p.Name()
	}
	key := sha256.Sum256([]byte(p.Type().String() + "|" + address))
	return hex.EncodeToString(key[:16])
}

func adaptiveClassify(allowed, global []AdaptiveProbeResult) string {
	for _, r := range global {
		if r.OK {
			return adaptiveNormal
		}
	}
	for _, r := range allowed {
		if r.OK {
			return adaptiveWhitelist
		}
	}
	return adaptiveOffline
}

// Returns a scope only when this cycle agrees with the confirmed mode. Pending
// transitions and offline cycles must never poison either historical ranking.
func (a *adaptiveHealth) observe(observed string, now time.Time) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if observed != a.observed {
		a.latest = map[C.Proxy]AdaptiveNodeResult{}
	}
	if observed == a.observed {
		a.pending++
	} else {
		a.observed = observed
		a.pending = 1
	}
	a.checkedAt = now
	if a.pending >= a.options.Confirmations {
		a.mode = observed
		a.pending = a.options.Confirmations
	}
	if observed != a.mode || (a.mode != adaptiveNormal && a.mode != adaptiveWhitelist) {
		return ""
	}
	return a.mode
}

func (a *adaptiveHealth) probeTargets(ctx context.Context, p C.ProxyAdapter, targets []AdaptiveTarget) []AdaptiveProbeResult {
	results := make([]AdaptiveProbeResult, 0, len(targets))
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		probeCtx, cancel := context.WithTimeout(ctx, a.timeout)
		results = append(results, a.probe(probeCtx, p, t))
		cancel()
	}
	return results
}

// A reachable CDN alone does not establish access to all required destinations.
// Missing/cancelled probes must not turn partial connectivity into a healthy node.
func adaptiveTargetsOK(results []AdaptiveProbeResult, expected int) bool {
	if expected == 0 || len(results) != expected {
		return false
	}
	for _, result := range results {
		if !result.OK {
			return false
		}
	}
	return true
}

func (a *adaptiveHealth) check(ctx context.Context, proxies []C.Proxy) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	allowed := a.probeTargets(ctx, a.direct, a.options.DirectAllowed)
	global := a.probeTargets(ctx, a.direct, a.options.DirectGlobal)
	if ctx.Err() != nil {
		return
	}
	scope := a.observe(adaptiveClassify(allowed, global), time.Now())
	a.mu.Lock()
	a.allowed, a.global = allowed, global
	a.mu.Unlock()
	a.revision.Add(1)
	ordered := a.order(proxies)
	// Check every node each round; low-ranked and new nodes cannot starve.
	resultsByProxy := make(map[C.Proxy]AdaptiveNodeResult)
	b := new(errgroup.Group)
	b.SetLimit(a.options.Concurrency)
	for _, p := range ordered {
		if ctx.Err() != nil {
			break
		}
		b.Go(func() error {
			results := a.probeTargets(ctx, p, a.options.Targets)
			if ctx.Err() != nil {
				return nil
			}
			ok := adaptiveTargetsOK(results, len(a.options.Targets))
			now := time.Now()
			a.mu.Lock()
			a.latest[p] = AdaptiveNodeResult{Mode: scope, At: now, OK: ok, Probes: results}
			resultsByProxy[p] = a.latest[p]
			a.mu.Unlock()
			a.revision.Add(1)
			return nil
		})
	}
	_ = b.Wait()
	if ctx.Err() != nil {
		return
	}
	// A long scan can span a network change. Do not attribute that batch to
	// either stable list unless direct control probes still agree at the end.
	endAllowed := a.probeTargets(ctx, a.direct, a.options.DirectAllowed)
	endGlobal := a.probeTargets(ctx, a.direct, a.options.DirectGlobal)
	if ctx.Err() != nil {
		return
	}
	endMode := adaptiveClassify(endAllowed, endGlobal)
	if endMode != adaptiveClassify(allowed, global) {
		a.observe(endMode, time.Now())
		scope = ""
	} else {
		// The controls at both ends are independent observations. A confirmed
		// first batch need not be discarded until the next full provider scan.
		scope = a.observe(endMode, time.Now())
	}
	a.mu.Lock()
	a.allowed, a.global = endAllowed, endGlobal
	a.checkedAt = time.Now()
	if scope != "" {
		for p, r := range resultsByProxy {
			r.Mode = scope
			if latest, exists := a.latest[p]; exists && latest.At == r.At {
				a.latest[p] = r
			}
			var ms int64
			for _, probe := range r.Probes {
				if probe.OK && (ms == 0 || probe.MS < ms) {
					ms = probe.MS
				}
			}
			key := adaptiveIdentity(p)
			a.records[scope][key] = a.records[scope][key].record(p.Name(), r.OK, ms, r.At)
		}
	}
	a.mu.Unlock()
	a.revision.Add(1)

	a.mu.Lock()
	// Old instances are no longer authoritative after a provider refresh.
	for id := range a.latest {
		if !a.members[id] {
			delete(a.latest, id)
		}
	}
	a.prune(time.Now())
	payload, err := json.Marshal(a.records)
	a.mu.Unlock()
	if err == nil && a.store != nil {
		a.store.SetStorage(a.storageKey, payload)
	}
}

func (a *adaptiveHealth) prune(now time.Time) {
	for _, records := range a.records {
		keys := make([]string, 0, len(records))
		for key, r := range records {
			if now.Sub(r.LastCheck) > adaptiveRecordTTL {
				delete(records, key)
			} else {
				keys = append(keys, key)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return records[keys[i]].LastCheck.After(records[keys[j]].LastCheck) })
		for i := 500; i < len(keys); i++ {
			delete(records, keys[i])
		}
	}
}

func (a *adaptiveHealth) order(proxies []C.Proxy) []C.Proxy {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := append([]C.Proxy(nil), proxies...)
	now := time.Now()
	records := a.records[a.mode]
	sort.SliceStable(out, func(i, j int) bool {
		left, right := records[adaptiveIdentity(out[i])], records[adaptiveIdentity(out[j])]
		if left.stable(now) != right.stable(now) {
			return left.stable(now)
		}
		return left.score(now) > right.score(now)
	})
	return out
}

func (a *adaptiveHealth) setProxies(proxies []C.Proxy) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.proxies = append([]C.Proxy(nil), proxies...)
	a.members = map[C.Proxy]bool{}
	for _, p := range proxies {
		a.members[p] = true
	}
	for p := range a.latest {
		if !a.members[p] {
			delete(a.latest, p)
		}
	}
	a.revision.Add(1)
}

func (a *adaptiveHealth) managed(p C.Proxy) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.members[p]
}

func (a *adaptiveHealth) alive(p C.Proxy) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	r, ok := a.latest[p]
	// Rankings survive restarts, but availability must be measured again.
	return a.members[p] && ok && r.OK && r.Mode != "" && r.Mode == a.mode && a.observed == a.mode &&
		time.Since(r.At) <= a.freshness && time.Since(a.checkedAt) <= a.freshness
}

func (a *adaptiveHealth) testProxy(ctx context.Context, p C.Proxy) {
	a.mu.RLock()
	mode, observed, checkedAt := a.mode, a.observed, a.checkedAt
	a.mu.RUnlock()
	if mode != observed || (mode != adaptiveNormal && mode != adaptiveWhitelist) || time.Since(checkedAt) > a.freshness {
		return
	}
	results := make([]AdaptiveProbeResult, len(a.options.Targets))
	var wg sync.WaitGroup
	for i, target := range a.options.Targets {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = a.probe(ctx, p, target) }()
	}
	wg.Wait()
	ok := adaptiveTargetsOK(results, len(a.options.Targets))
	if ctx.Err() != nil && !ok {
		return
	}
	a.mu.Lock()
	if a.mode == mode && a.observed == observed && a.checkedAt == checkedAt {
		a.latest[p] = AdaptiveNodeResult{Mode: mode, At: time.Now(), OK: ok, Probes: results}
	}
	a.mu.Unlock()
	a.revision.Add(1)
}

func (a *adaptiveHealth) snapshot() *AdaptiveSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s := &AdaptiveSnapshot{Mode: a.mode, Observed: a.observed, Pending: a.pending, CheckedAt: a.checkedAt,
		DirectAllowed: append([]AdaptiveProbeResult(nil), a.allowed...), DirectGlobal: append([]AdaptiveProbeResult(nil), a.global...),
		Rankings: map[string][]AdaptiveNodeSummary{}, Results: map[string]AdaptiveNodeResult{}, PersistenceError: a.persistenceError}
	now := time.Now()
	for _, mode := range []string{adaptiveNormal, adaptiveWhitelist} {
		rows := []AdaptiveNodeSummary{}
		for _, p := range a.proxies {
			r := a.records[mode][adaptiveIdentity(p)]
			rows = append(rows, AdaptiveNodeSummary{Name: p.Name(), Record: r, SuccessRate: r.rate(), Score: r.score(now), Stable: r.stable(now)})
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Stable != rows[j].Stable {
				return rows[i].Stable
			}
			return rows[i].Score > rows[j].Score
		})
		s.Rankings[mode] = rows
	}
	for _, p := range a.proxies {
		if r, ok := a.latest[p]; ok {
			s.Results[p.Name()] = r
		}
	}
	return s
}

func (hc *HealthCheck) configureAdaptive(name string, options AdaptiveHealthOptions) error {
	if !options.Enable {
		return nil
	}
	a, err := newAdaptiveHealth(name, options, hc.timeout, hc.interval, cachefile.Cache())
	if err != nil {
		return err
	}
	if a.persistenceError != "" {
		log.Warnln("[Adaptive health] %s: %s", name, a.persistenceError)
	}
	a.setProxies(hc.proxies)
	hc.adaptive = a
	return nil
}
