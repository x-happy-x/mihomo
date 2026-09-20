package route

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/common/yaml"

	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
	"github.com/metacubex/mihomo/tunnel"

	"github.com/metacubex/chi"
	"github.com/metacubex/chi/render"
	"github.com/metacubex/http"
	"github.com/samber/lo"
)

func proxyProviderRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/", getProviders)

	r.Route("/{providerName}", func(r chi.Router) {
		r.Use(parseProviderName, findProviderByName)
		r.Get("/", getProvider)
		r.Put("/", updateProvider)
		r.Get("/healthcheck", healthCheckProvider)
		r.Put("/proxies", replaceProviderProxies)
		r.Get("/proxies", getProviderProxies)
		r.Post("/proxies", addProviderProxy)
		r.Put("/proxies/{proxyName}", updateProviderProxy)
		r.Delete("/proxies/{proxyName}", deleteProviderProxy)
		r.Mount("/", proxyProviderProxyRouter())
	})
	return r
}

func proxyProviderProxyRouter() http.Handler {
	r := chi.NewRouter()
	r.Route("/{name}", func(r chi.Router) {
		r.Use(parseProxyName, findProviderProxyByName)
		r.Get("/", getProxy)
		r.Get("/healthcheck", getProxyDelay)
	})
	return r
}

func getProviders(w http.ResponseWriter, r *http.Request) {
	providers := tunnel.Providers()
	render.JSON(w, r, render.M{
		"providers": providers,
	})
}

func getProvider(w http.ResponseWriter, r *http.Request) {
	provider := r.Context().Value(CtxKeyProvider).(P.ProxyProvider)
	render.JSON(w, r, provider)
}

func updateProvider(w http.ResponseWriter, r *http.Request) {
	provider := r.Context().Value(CtxKeyProvider).(P.ProxyProvider)
	if err := provider.Update(); err != nil {
		render.Status(r, http.StatusServiceUnavailable)
		render.JSON(w, r, newError(err.Error()))
		return
	}
	render.NoContent(w, r)
}

func healthCheckProvider(w http.ResponseWriter, r *http.Request) {
	provider := r.Context().Value(CtxKeyProvider).(P.ProxyProvider)
	provider.HealthCheck()
	render.NoContent(w, r)
}

func parseProviderName(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := getEscapeParam(r, "providerName")
		ctx := context.WithValue(r.Context(), CtxKeyProviderName, name)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func findProviderByName(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.Context().Value(CtxKeyProviderName).(string)
		providers := tunnel.Providers()
		provider, exist := providers[name]
		if !exist {
			render.Status(r, http.StatusNotFound)
			render.JSON(w, r, ErrNotFound)
			return
		}

		ctx := context.WithValue(r.Context(), CtxKeyProvider, provider)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func findProviderProxyByName(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			name = r.Context().Value(CtxKeyProxyName).(string)
			pd   = r.Context().Value(CtxKeyProvider).(P.ProxyProvider)
		)
		proxy, exist := lo.Find(pd.Proxies(), func(proxy C.Proxy) bool {
			return proxy.Name() == name
		})

		if !exist {
			render.Status(r, http.StatusNotFound)
			render.JSON(w, r, ErrNotFound)
			return
		}

		ctx := context.WithValue(r.Context(), CtxKeyProxy, proxy)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func ruleProviderRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/", getRuleProviders)
	r.Route("/{name}", func(r chi.Router) {
		r.Use(parseRuleProviderName, findRuleProviderByName)
		r.Put("/", updateRuleProvider)
	})
	return r
}

func getRuleProviders(w http.ResponseWriter, r *http.Request) {
	ruleProviders := tunnel.RuleProviders()
	render.JSON(w, r, render.M{
		"providers": ruleProviders,
	})
}

func updateRuleProvider(w http.ResponseWriter, r *http.Request) {
	provider := r.Context().Value(CtxKeyProvider).(P.RuleProvider)
	if err := provider.Update(); err != nil {
		render.Status(r, http.StatusServiceUnavailable)
		render.JSON(w, r, newError(err.Error()))
		return
	}
	render.NoContent(w, r)
}

func parseRuleProviderName(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := getEscapeParam(r, "name")
		ctx := context.WithValue(r.Context(), CtxKeyProviderName, name)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func findRuleProviderByName(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.Context().Value(CtxKeyProviderName).(string)
		providers := tunnel.RuleProviders()
		provider, exist := providers[name]
		if !exist {
			render.Status(r, http.StatusNotFound)
			render.JSON(w, r, ErrNotFound)
			return
		}

		ctx := context.WithValue(r.Context(), CtxKeyProvider, provider)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// fileBackedProvider is the part of a provider that owns a local file. The
// concrete proxy provider satisfies it through its fetcher.
type fileBackedProvider interface {
	Vehicle() P.Vehicle
}

// replaceProviderProxies rewrites the proxy list of a file-backed provider and
// reloads it, so a dashboard can manage those entries without touching the main
// config, which usually carries anchors and comments a rewrite would destroy.
//
// Only providers whose vehicle is a local file are eligible: anything fetched
// from elsewhere would be overwritten on its next update anyway.
func replaceProviderProxies(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Proxies []map[string]any `json:"proxies"`
	}{}
	if err := render.DecodeJSON(r.Body, &req); err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, ErrBadRequest)
		return
	}

	provider := r.Context().Value(CtxKeyProvider).(P.ProxyProvider)
	_, vehicle, err := fileProviderProxies(provider)
	if err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, newError(err.Error()))
		return
	}
	if err = writeFileProviderProxies(provider, vehicle, req.Proxies); err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, newError(err.Error()))
		return
	}
	render.NoContent(w, r)
}

// fileProviderProxies reads the entries a file-backed provider currently holds.
// The provider API deliberately does not expose proxy settings, so a caller
// that only wants to add or remove one entry should not have to send the
// others back: that would mean reading everyone's credentials first.
func fileProviderProxies(provider P.ProxyProvider) ([]map[string]any, P.Vehicle, error) {
	if provider.VehicleType() != P.File {
		return nil, nil, errors.New("Must be a file-backed provider")
	}
	backed, ok := provider.(fileBackedProvider)
	if !ok {
		return nil, nil, errors.New("Provider does not expose its file")
	}
	vehicle := backed.Vehicle()

	buf, err := os.ReadFile(vehicle.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, vehicle, nil // a provider may legitimately start empty
		}
		return nil, nil, err
	}
	holder := struct {
		Proxies []map[string]any `yaml:"proxies"`
	}{}
	if err = yaml.Unmarshal(buf, &holder); err != nil {
		return nil, nil, err
	}
	return holder.Proxies, vehicle, nil
}

// writeFileProviderProxies validates every entry, writes the file and reloads.
func writeFileProviderProxies(provider P.ProxyProvider, vehicle P.Vehicle, proxies []map[string]any) error {
	// Validate before writing: a provider left holding a broken file keeps
	// failing to reload, with no way back through the API.
	for i, mapping := range proxies {
		proxy, err := adapter.ParseProxy(mapping)
		if err != nil {
			return fmt.Errorf("proxy %d: %w", i, err)
		}
		_ = proxy.Close()
	}
	buf, err := yaml.Marshal(map[string]any{"proxies": proxies})
	if err != nil {
		return err
	}
	if err = vehicle.Write(buf); err != nil {
		return err
	}
	return provider.Update()
}

func addProviderProxy(w http.ResponseWriter, r *http.Request) {
	proxy := map[string]any{}
	if err := render.DecodeJSON(r.Body, &proxy); err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, ErrBadRequest)
		return
	}
	name, _ := proxy["name"].(string)
	if name == "" {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, newError("Proxy name is required"))
		return
	}

	provider := r.Context().Value(CtxKeyProvider).(P.ProxyProvider)
	proxies, vehicle, err := fileProviderProxies(provider)
	if err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, newError(err.Error()))
		return
	}
	for _, existing := range proxies {
		if existingName, _ := existing["name"].(string); existingName == name {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, newError(fmt.Sprintf("Proxy %q already exists", name)))
			return
		}
	}

	if err = writeFileProviderProxies(provider, vehicle, append(proxies, proxy)); err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, newError(err.Error()))
		return
	}
	render.NoContent(w, r)
}

func deleteProviderProxy(w http.ResponseWriter, r *http.Request) {
	name := getEscapeParam(r, "proxyName")
	provider := r.Context().Value(CtxKeyProvider).(P.ProxyProvider)

	proxies, vehicle, err := fileProviderProxies(provider)
	if err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, newError(err.Error()))
		return
	}

	kept := make([]map[string]any, 0, len(proxies))
	for _, existing := range proxies {
		if existingName, _ := existing["name"].(string); existingName != name {
			kept = append(kept, existing)
		}
	}
	if len(kept) == len(proxies) {
		render.Status(r, http.StatusNotFound)
		render.JSON(w, r, ErrNotFound)
		return
	}

	if err = writeFileProviderProxies(provider, vehicle, kept); err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, newError(err.Error()))
		return
	}
	render.NoContent(w, r)
}

// sensitiveProxyFields marks settings that must not leave the core. The match
// is a substring so "encryption-key", "private-key" and "auth-key" are all
// covered; over-redacting is harmless here, since an omitted field simply
// keeps its stored value on the next update.
var sensitiveProxyFields = []string{"password", "secret", "token", "key", "auth", "uuid", "psk"}

func isSensitiveProxyField(name string) bool {
	lower := strings.ToLower(name)
	for _, marker := range sensitiveProxyFields {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// redactProxy drops the settings a caller must not read back. What is left is
// enough to show an entry in an editor; the parts that are not are merged from
// the stored copy when the entry is updated.
func redactProxy(proxy map[string]any) map[string]any {
	redacted := make(map[string]any, len(proxy))
	for key, value := range proxy {
		if isSensitiveProxyField(key) {
			continue
		}
		redacted[key] = value
	}
	return redacted
}

func getProviderProxies(w http.ResponseWriter, r *http.Request) {
	provider := r.Context().Value(CtxKeyProvider).(P.ProxyProvider)
	proxies, _, err := fileProviderProxies(provider)
	if err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, newError(err.Error()))
		return
	}

	redacted := make([]map[string]any, 0, len(proxies))
	for _, proxy := range proxies {
		redacted = append(redacted, redactProxy(proxy))
	}
	render.JSON(w, r, render.M{"proxies": redacted})
}

// updateProviderProxy merges the given settings into a stored entry. Fields the
// caller leaves out keep their stored value, which is how an editor changes a
// transport without ever having seen the encryption key.
func updateProviderProxy(w http.ResponseWriter, r *http.Request) {
	patch := map[string]any{}
	if err := render.DecodeJSON(r.Body, &patch); err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, ErrBadRequest)
		return
	}

	name := getEscapeParam(r, "proxyName")
	provider := r.Context().Value(CtxKeyProvider).(P.ProxyProvider)
	proxies, vehicle, err := fileProviderProxies(provider)
	if err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, newError(err.Error()))
		return
	}

	index := -1
	for i, existing := range proxies {
		if existingName, _ := existing["name"].(string); existingName == name {
			index = i
			break
		}
	}
	if index < 0 {
		render.Status(r, http.StatusNotFound)
		render.JSON(w, r, ErrNotFound)
		return
	}

	merged := make(map[string]any, len(proxies[index])+len(patch))
	for key, value := range proxies[index] {
		merged[key] = value
	}
	for key, value := range patch {
		// An empty string means "leave as is": an editor cannot show a redacted
		// field, so a blank input must not wipe the stored value.
		if text, ok := value.(string); ok && text == "" {
			continue
		}
		merged[key] = value
	}

	if newName, _ := merged["name"].(string); newName != name {
		if newName == "" {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, newError("Proxy name is required"))
			return
		}
		for i, existing := range proxies {
			if i == index {
				continue
			}
			if existingName, _ := existing["name"].(string); existingName == newName {
				render.Status(r, http.StatusBadRequest)
				render.JSON(w, r, newError(fmt.Sprintf("Proxy %q already exists", newName)))
				return
			}
		}
	}

	updated := make([]map[string]any, len(proxies))
	copy(updated, proxies)
	updated[index] = merged

	if err = writeFileProviderProxies(provider, vehicle, updated); err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, newError(err.Error()))
		return
	}
	render.NoContent(w, r)
}
