package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/metacubex/mihomo/common/yaml"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
)

// Preserve all Proxy capabilities while keeping only a digest of the effective
// connection config. Unlike historical address identity, admission must not
// confuse credentials/transports hosted on the same IP. Names are presentation.
type healthIdentifiedProxy struct {
	C.Proxy
	healthKey string
}

func identifyHealthProxy(p C.Proxy, mapping map[string]any) (C.Proxy, error) {
	connection := make(map[string]any, len(mapping))
	for key, value := range mapping {
		if key != "name" {
			connection[key] = value
		}
	}
	// yaml.v3 sorts map keys; effective nested transport options are included.
	data, err := yaml.Marshal(connection)
	if err != nil {
		return nil, fmt.Errorf("cannot identify proxy connection settings")
	}
	hash := sha256.Sum256(data)
	return &healthIdentifiedProxy{Proxy: p, healthKey: hex.EncodeToString(hash[:])}, nil
}

func healthProxyKey(p C.Proxy) string {
	if identified, ok := p.(*healthIdentifiedProxy); ok {
		return identified.healthKey
	}
	return ""
}

func (bp *baseProvider) adaptiveHealthState() *adaptiveHealth { return bp.healthCheck.adaptive }

// ResolveAdaptiveDependencies runs before providers are started. This first
// version supports a base provider followed by independent service providers;
// rejecting chains also makes the source/child lock ordering unambiguous.
func ResolveAdaptiveDependencies(providers map[string]P.ProxyProvider) error {
	states := map[string]*adaptiveHealth{}
	for name, pd := range providers {
		if pd, ok := pd.(interface{ adaptiveHealthState() *adaptiveHealth }); ok {
			states[name] = pd.adaptiveHealthState()
		}
	}
	for name, a := range states {
		if a == nil || a.options.DependsOn == "" {
			continue
		}
		source := states[a.options.DependsOn]
		if source == nil {
			return fmt.Errorf("adaptive provider %s depends-on %s: source missing or adaptive disabled", name, a.options.DependsOn)
		}
		if source == a || source.options.DependsOn != "" {
			return fmt.Errorf("adaptive provider %s: depends-on requires an independent base provider (no self-reference, cycles or chains)", name)
		}
	}
	for _, a := range states {
		if a != nil && a.options.DependsOn != "" {
			a.source = states[a.options.DependsOn]
		}
	}
	return nil
}

// Caller may hold the child mutex. Sources cannot depend on children. Require
// the latest raw success as well as availability: a source in failure grace
// must not spend requests on secondary checks.
func (a *adaptiveHealth) dependencyAllows(p C.Proxy, mode string) bool {
	if a.options.DependsOn == "" {
		return true
	}
	source := a.source
	key := healthProxyKey(p)
	if source == nil || key == "" {
		return false
	}
	source.mu.RLock()
	defer source.mu.RUnlock()
	if mode == "" || source.mode != mode || source.observed != mode || time.Since(source.checkedAt) > source.freshness {
		return false
	}
	for _, candidate := range source.byHealthKey[key] {
		r, exists := source.latest[candidate]
		if exists && r.Mode == mode && r.OK && r.Available && time.Since(r.At) <= source.freshness {
			return true
		}
	}
	return false
}

// Skip is visible through the API but must not become a failed service sample.
func (a *adaptiveHealth) skipDependent(p C.Proxy, mode string) {
	a.mu.Lock()
	a.latest[p] = AdaptiveNodeResult{Mode: mode, At: time.Now(), Skipped: "dependency-not-ready"}
	a.mu.Unlock()
	a.revision.Add(1)
}
