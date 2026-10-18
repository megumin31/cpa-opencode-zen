// Package catalog resolves the connected catalog and Console overrides without runtime state.
package catalog

// Console/config inheritance follows the OpenCode CLI v2.0.29 native loader
// (Wp, Al, Ls, WI, ip and Mx), inspected from the official release. The resolver
// keeps the connected opencode catalog baseline plus explicitly configured
// Console providers. It never binds the key to every public catalog provider. It does not infer a wire protocol from a model name.
//
// Unsupported packages and endpoints are retained with Unsupported populated;
// callers MUST NOT expose or execute those routes. Invalid documents fail the
// entire resolution. Explicit variants are supported. Catalog reasoning_options
// are not synthesized: that requires provider-specific, model-dependent recipes
// outside this adapter's audited protocol allowlist.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/megumin31/cpa-opencode-zen/internal/buildinfo"
)

// Route is a resolved provider/model execution contract. Unsupported routes must not execute.
type Route struct {
	Alias, ProviderID, Canonical, ModelKey, ModelID, Package, Protocol, BaseURL string
	Settings, Body, Compatibility, Capabilities                                 map[string]any
	Headers                                                                     map[string]string
	Limit                                                                       map[string]any
	Unsupported                                                                 string
	legacyUnsupported                                                           string
}

// Variant is an explicitly configured override for a catalog model.
type Variant struct {
	ID       string            `json:"id"`
	Settings map[string]any    `json:"settings"`
	Headers  map[string]string `json:"headers"`
	Body     map[string]any    `json:"body"`
}

// Model is the normalized, JSON-backed catalog model metadata.
type Model struct {
	ID               string            `json:"id"`
	ModelID          string            `json:"modelID"`
	Package          string            `json:"package"`
	Settings         map[string]any    `json:"settings"`
	Headers          map[string]string `json:"headers"`
	Body             map[string]any    `json:"body"`
	Compatibility    map[string]any    `json:"compatibility"`
	Capabilities     map[string]any    `json:"capabilities"`
	Reasoning        *bool             `json:"reasoning"`
	ReasoningOptions []any             `json:"reasoning_options"`
	Limit            map[string]any    `json:"limit"`
	Variants         []Variant         `json:"variants"`
	Disabled         bool              `json:"disabled"`
	Status           string            `json:"status"`
}

// Provider is a normalized catalog provider and its model definitions.
type Provider struct {
	ID            string            `json:"id"`
	Canonical     string            `json:"canonical"`
	Package       string            `json:"package"`
	Settings      map[string]any    `json:"settings"`
	Headers       map[string]string `json:"headers"`
	Body          map[string]any    `json:"body"`
	Compatibility map[string]any    `json:"compatibility"`
	Models        map[string]Model  `json:"models"`
}

// Required record values are objects. encoding/json otherwise accepts null
// into a struct, which would silently turn malformed model entries into routes.
func (v *Variant) UnmarshalJSON(data []byte) error {
	type plain Variant
	var value plain
	if err := resolverDecode(data, &value); err != nil {
		return err
	}
	*v = Variant(value)
	return nil
}
func (m *Model) UnmarshalJSON(data []byte) error {
	type plain Model
	var value plain
	if err := resolverDecode(data, &value); err != nil {
		return err
	}
	*m = Model(value)
	return nil
}
func (p *Provider) UnmarshalJSON(data []byte) error {
	type plain Provider
	var value plain
	if err := resolverDecode(data, &value); err != nil {
		return err
	}
	*p = Provider(value)
	return nil
}

// ParseCatalog accepts the public models.opencode.ai/api.json provider map and
// the native v2 provider map form (package/settings/capabilities). The public
// schema is heterogeneous: provider.api and model.provider.api are independent,
// npm may be overridden per model, and experimental modes are model aliases.
func ParseCatalog(data []byte) (map[string]Provider, error) {
	entries, keys, err := resolverOrderedObject(data)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if entries == nil {
		return nil, fmt.Errorf("catalog: expected provider object")
	}
	out := make(map[string]Provider, len(entries))
	for _, key := range keys {
		raw := entries[key]
		var p Provider
		if err := resolverDecode(raw, &p); err != nil {
			return nil, fmt.Errorf("catalog provider %q: %w", key, err)
		}
		var wire struct {
			NPM    string          `json:"npm"`
			API    string          `json:"api"`
			Models json.RawMessage `json:"models"`
		}
		if err := resolverDecode(raw, &wire); err != nil {
			return nil, fmt.Errorf("catalog provider %q: %w", key, err)
		}
		if p.ID == "" {
			p.ID = key
		}
		if p.ID == "" {
			return nil, fmt.Errorf("catalog: empty provider ID")
		}
		// Native Xm excludes superseded catalog providers before registration.
		if p.ID == "azure-cognitive-services" || p.ID == "google-vertex-anthropic" {
			continue
		}
		p.Settings = resolverCleanSettings(p.Settings)
		if p.Package == "" {
			p.Package = resolverPackage(wire.NPM, p.ID, "")
		} else {
			p.Package = resolverPackage(p.Package, p.ID, "")
		}
		if wire.API != "" && p.Package != "@opencode/ai/providers/cloudflare-workers-ai" {
			p.Settings = Merge(map[string]any{"baseURL": wire.API}, p.Settings)
		}
		modelEntries, modelKeys, err := resolverOrderedObject(wire.Models)
		if err != nil && len(wire.Models) != 0 {
			return nil, fmt.Errorf("catalog provider %q models: %w", key, err)
		}
		p.Models = make(map[string]Model, len(modelEntries))
		for _, modelKey := range modelKeys {
			m, modes, err := resolverCatalogModel(modelEntries[modelKey], modelKey, p, wire.NPM)
			if err != nil {
				return nil, fmt.Errorf("catalog %s/%s: %w", p.ID, modelKey, err)
			}
			// Native ip emits base then modes in Object.values order; Xm filters
			// before bE creates a Map. Last surviving ID wins, including a
			// generated mode colliding with a separately listed model.
			if !resolverCatalogModelVisible(p.ID, m) {
				continue
			}
			p.Models[m.ID] = m
			for _, modeID := range resolverSortedKeys(modes) {
				mode := modes[modeID]
				copy := resolverCloneModel(m)
				copy.ID = m.ID + "-" + modeID
				copy.Headers = Headers(nil, mode.Headers)
				copy.Body = Merge(nil, mode.Body)
				p.Models[copy.ID] = copy
			}
		}
		out[p.ID] = p
	}
	return out, nil
}

func resolverCatalogModel(raw []byte, key string, provider Provider, providerNPM string) (Model, map[string]Variant, error) {
	var m Model
	if err := resolverDecode(raw, &m); err != nil {
		return m, nil, err
	}
	var wire struct {
		Provider struct {
			NPM   string `json:"npm"`
			API   string `json:"api"`
			Shape string `json:"shape"`
		} `json:"provider"`
		ToolCall   *bool `json:"tool_call"`
		Modalities struct {
			Input  []string `json:"input"`
			Output []string `json:"output"`
		} `json:"modalities"`
		Interleaved  any `json:"interleaved"`
		Experimental struct {
			Modes map[string]struct {
				Provider Variant `json:"provider"`
			} `json:"modes"`
		} `json:"experimental"`
	}
	if err := resolverDecode(raw, &wire); err != nil {
		return m, nil, err
	}
	if m.ID == "" {
		m.ID = key
	}
	if m.ID == "" {
		return m, nil, fmt.Errorf("empty model ID")
	}
	if m.ModelID == "" {
		m.ModelID = m.ID
	}
	if m.Package != "" {
		m.Package = resolverPackage(m.Package, provider.ID, wire.Provider.Shape)
	} else {
		npm := providerNPM
		if wire.Provider.NPM != "" {
			npm = wire.Provider.NPM
		}
		resolved := resolverPackage(npm, provider.ID, wire.Provider.Shape)
		if wire.Provider.NPM != "" || (resolved != "" && resolved != provider.Package) {
			m.Package = resolved
		}
	}
	effectivePackage := m.Package
	if effectivePackage == "" {
		effectivePackage = provider.Package
	}
	m.Settings = resolverCleanSettings(m.Settings)
	defaults := map[string]any{}
	if wire.Provider.API != "" {
		defaults["baseURL"] = wire.Provider.API
	}
	if effectivePackage == "@opencode/ai/providers/openai-compatible" {
		defaults["provider"] = provider.ID
	}
	m.Settings = Merge(defaults, m.Settings)
	caps := map[string]any{"tools": false, "input": []any{}, "output": []any{}}
	if wire.ToolCall != nil {
		caps["tools"] = *wire.ToolCall
	}
	if wire.Modalities.Input != nil {
		caps["input"] = resolverStrings(wire.Modalities.Input)
	}
	if wire.Modalities.Output != nil {
		caps["output"] = resolverStrings(wire.Modalities.Output)
	}
	caps = resolverReasoning(caps, m)
	m.Capabilities = resolverCapabilities(caps, m.Capabilities)
	if field, ok := wire.Interleaved.(string); ok {
		m.Compatibility = resolverShallow(map[string]any{"reasoningField": field}, m.Compatibility)
	}
	if value, ok := wire.Interleaved.(map[string]any); ok {
		if field, ok := value["field"].(string); ok {
			m.Compatibility = resolverShallow(map[string]any{"reasoningField": field}, m.Compatibility)
		}
	}
	// Mx enables the record; native Xm applies catalog status/modality filtering
	// before registration. Console may still explicitly add a fresh override.
	m.Disabled = false
	modes := make(map[string]Variant, len(wire.Experimental.Modes))
	for id, value := range wire.Experimental.Modes {
		modes[id] = value.Provider
	}
	return m, modes, nil
}

// ErrInvalid identifies a malformed catalog separately from malformed Console config.
var ErrInvalid = errors.New("invalid model catalog")

// ResolveRoutes combines the connected catalog baseline and explicit Console providers.
func ResolveRoutes(configJSON, catalogJSON []byte) (map[string]Route, error) {
	var config struct {
		Providers json.RawMessage `json:"providers"`
	}
	if err := resolverDecode(configJSON, &config); err != nil {
		return nil, fmt.Errorf("Console config: %w", err)
	}
	providers, providerKeys, err := resolverOrderedObject(config.Providers)
	if err != nil {
		return nil, fmt.Errorf("Console config: providers: %w", err)
	}
	catalog, err := ParseCatalog(catalogJSON)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	// W6 seeds catalog providers before Wp applies Console overrides. Only the
	// opencode integration is connected by this plugin's service key; arbitrary
	// catalog providers (including opencode-go) are not implicitly connected.
	if _, exists := providers[buildinfo.ProviderID]; !exists {
		if _, available := catalog[buildinfo.ProviderID]; available {
			providers[buildinfo.ProviderID] = json.RawMessage(`{}`)
			providerKeys = append(providerKeys, buildinfo.ProviderID)
		}
	}
	// Wp reads and mutates the same registry in Object.entries order. Keep
	// provider metadata separate from the target's own model collection: a new
	// canonical alias inherits only the models explicitly added to that alias.
	for _, providerID := range providerKeys {
		var override Provider
		if err := resolverDecode(providers[providerID], &override); err != nil {
			return nil, fmt.Errorf("Console provider %q: %w", providerID, err)
		}
		var wire struct {
			Models json.RawMessage `json:"models"`
		}
		if err := resolverDecode(providers[providerID], &wire); err != nil {
			return nil, err
		}
		var modelKeys []string
		if len(wire.Models) != 0 {
			_, modelKeys, err = resolverOrderedObject(wire.Models)
			if err != nil {
				return nil, fmt.Errorf("Console provider %q models: %w", providerID, err)
			}
		}
		if providerID == "" {
			return nil, fmt.Errorf("Console config: empty provider ID")
		}
		canonical := override.Canonical
		if canonical == "" {
			canonical = providerID
		}
		target := catalog[providerID]
		inherited, exists := catalog[canonical]
		if !exists {
			inherited = target
		}
		p := inherited
		p.ID = providerID
		if override.Canonical != "" {
			p.Canonical = override.Canonical
		}
		if override.Package != "" {
			p.Package = resolverPackage(override.Package, canonical, "")
		}
		p.Settings = Merge(resolverCleanSettings(inherited.Settings), resolverCleanSettings(override.Settings))
		p.Headers = Headers(inherited.Headers, override.Headers)
		p.Body = Merge(inherited.Body, override.Body)
		// Console does not apply a provider-level compatibility override.
		p.Compatibility = resolverShallow(nil, inherited.Compatibility)
		p.Models = make(map[string]Model, len(target.Models)+len(override.Models))
		for key, model := range target.Models {
			p.Models[key] = resolverCloneModel(model)
		}
		catalog[providerID] = p
		for _, modelKey := range modelKeys {
			mo := override.Models[modelKey]
			if modelKey == "" {
				return nil, fmt.Errorf("Console %s: empty model key", providerID)
			}
			lookup := mo.ModelID
			if lookup == "" {
				lookup = modelKey
			}
			// Refresh the source lookup per model: when canonical == providerID,
			// earlier remaps in this very object are already visible.
			// Wp captures K.get before creating a previously absent provider.
			// Such a missing canonical source stays absent for this transform.
			var source map[string]Model
			if exists {
				source = catalog[canonical].Models
			}
			base, found := source[lookup]
			if !found {
				base, found = source[modelKey]
			}
			if !found {
				base, found = p.Models[modelKey]
			}
			if !found {
				base = Model{ID: modelKey, ModelID: modelKey, Capabilities: map[string]any{"tools": false, "input": []any{}, "output": []any{}}}
			}
			m := resolverCloneModel(base)
			m.ID = modelKey
			if mo.ModelID != "" {
				m.ModelID = mo.ModelID
			}
			if m.ModelID == "" {
				m.ModelID = modelKey
			}
			if override.Package != "" {
				m.Package = ""
			}
			if mo.Package != "" {
				m.Package = resolverPackage(mo.Package, canonical, "")
			}
			if _, changed := override.Settings["baseURL"]; changed {
				delete(m.Settings, "baseURL")
			}
			m.Settings = Merge(resolverCleanSettings(m.Settings), resolverCleanSettings(mo.Settings))
			m.Headers = Headers(m.Headers, mo.Headers)
			m.Body = Merge(m.Body, mo.Body)
			m.Compatibility = resolverShallow(m.Compatibility, mo.Compatibility)
			// Public-schema metadata is accepted alongside the normalized native
			// capabilities object. Explicit native fields have the final say.
			m.Capabilities = resolverCapabilities(resolverReasoning(m.Capabilities, mo), mo.Capabilities)
			m.Limit = resolverShallow(m.Limit, mo.Limit)
			m.Variants, err = resolverVariants(m.Variants, mo.Variants)
			if err != nil {
				return nil, fmt.Errorf("Console %s/%s: %w", providerID, modelKey, err)
			}
			m.Disabled = mo.Disabled
			p.Models[modelKey] = m
		}
		catalog[providerID] = p
	}
	routes := make(map[string]Route)
	for _, providerID := range providerKeys {
		p := catalog[providerID]
		for _, modelKey := range resolverSortedKeys(p.Models) {
			m := p.Models[modelKey]
			if m.Disabled {
				continue
			}
			r := resolverRoute(p, m)
			if err := resolverAddRoute(routes, r); err != nil {
				return nil, err
			}
			for _, v := range m.Variants {
				// "default" is reserved by the native resolver and selects the
				// unmodified model even if a variant with this ID is present.
				variant := r
				variant.Alias += "/variant/" + url.PathEscape(v.ID)
				if v.ID != "default" {
					settings, headers, body, unsupported := resolverLegacyOptions(r.Package, v.Settings, v.Headers, v.Body)
					variant.Settings = Merge(r.Settings, resolverModelSettings(settings))
					variant.Body = Merge(r.Body, body)
					variant.Headers = Headers(r.Headers, headers)
					if variant.legacyUnsupported == "" && unsupported != "" {
						variant.legacyUnsupported = "variant: " + unsupported
					}
					resolverFinishRoute(&variant)
				}
				if err := resolverAddRoute(routes, variant); err != nil {
					return nil, err
				}
			}
		}
	}
	return routes, nil
}

func resolverRoute(p Provider, m Model) Route {
	pkg := m.Package
	if pkg == "" {
		pkg = p.Package
	}
	providerSettings, providerHeaders, providerBody, providerUnsupported := resolverLegacyOptions(p.Package, p.Settings, p.Headers, p.Body)
	modelSettings, modelHeaders, modelBody, modelUnsupported := resolverLegacyOptions(pkg, m.Settings, m.Headers, m.Body)
	alias := "opencode-zen/" + url.PathEscape(m.ID)
	if p.ID != buildinfo.ProviderID {
		alias = "opencode/" + url.PathEscape(p.ID) + "/" + url.PathEscape(m.ID)
	}
	r := Route{
		Alias:      alias,
		ProviderID: p.ID, Canonical: p.Canonical, ModelKey: m.ID, ModelID: m.ModelID, Package: pkg,
		Settings: Merge(resolverCleanSettings(providerSettings), resolverModelSettings(modelSettings)),
		Headers:  Headers(providerHeaders, modelHeaders), Body: Merge(providerBody, modelBody),
		Compatibility: resolverShallow(p.Compatibility, m.Compatibility),
		Capabilities:  resolverShallow(nil, m.Capabilities), Limit: resolverShallow(nil, m.Limit),
	}
	if providerUnsupported != "" {
		r.legacyUnsupported = "provider: " + providerUnsupported
	} else if modelUnsupported != "" {
		r.legacyUnsupported = "model: " + modelUnsupported
	}
	resolverFinishRoute(&r)
	return r
}

func resolverFinishRoute(r *Route) {
	// A variant may change its endpoint, but cannot hide invalid inherited
	// legacy options merely by forcing endpoint/protocol recalculation.
	defer func() {
		if r.legacyUnsupported != "" {
			r.Unsupported = r.legacyUnsupported
		}
	}()
	r.Unsupported = ""
	spec, ok := resolverProtocols[r.Package]
	if !ok {
		r.Protocol = ""
		r.Unsupported = fmt.Sprintf("unsupported provider package %q; no protocol inference", r.Package)
	} else {
		r.Protocol = spec.protocol
	}
	r.BaseURL = ""
	if v, exists := r.Settings["baseURL"]; exists {
		var valid bool
		r.BaseURL, valid = v.(string)
		if !valid {
			r.Unsupported = "settings.baseURL must be a string"
			return
		}
	} else if ok {
		r.BaseURL = spec.baseURL
	}
	if r.BaseURL == "" {
		if r.Unsupported == "" {
			r.Unsupported = "provider requires an explicit baseURL"
		}
		return
	}
	if strings.Contains(r.BaseURL, "${") {
		r.Unsupported = "baseURL contains unresolved provider variables"
		return
	}
	u, err := url.Parse(r.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		r.Unsupported = "baseURL must be an absolute HTTP(S) URL without credentials or fragment"
	}
}

// resolverLegacyOptions implements native d$/PE for the intersection of the
// audited CE set and this adapter's supported packages. SDK aliases have already
// been normalized. Native explicit /chat, /responses and compatible extension
// packages are not in CE: interpreting their extraBody would invent behavior,
// so legacy fields on those packages fail closed instead.
//
// The native ordering is WI(settings.headers, explicitHeaders) and
// Ls(settings.extraBody, explicitBody), independently at provider/model/variant
// scope, BEFORE the provider/model/variant layers are combined.
func resolverLegacyOptions(pkg string, settings map[string]any, headers map[string]string, body map[string]any) (map[string]any, map[string]string, map[string]any, string) {
	settings = resolverCleanSettings(settings)
	headers = Headers(nil, headers)
	body = Merge(nil, body)
	legacyHeaders, hasHeaders := settings["headers"]
	legacyBody, hasBody := settings["extraBody"]
	if !hasHeaders && !hasBody {
		return settings, headers, body, ""
	}
	switch pkg {
	case "@opencode/ai/providers/openai", "@opencode/ai/providers/openai-compatible", "@opencode/ai/providers/anthropic", "@opencode/ai/providers/google":
	default:
		return settings, headers, body, "legacy settings.headers/extraBody have no audited migration for this package"
	}
	if hasHeaders {
		raw, ok := legacyHeaders.(map[string]any)
		if !ok {
			return settings, headers, body, "settings.headers must be an object of string values"
		}
		values := make(map[string]string, len(raw))
		for key, value := range raw {
			text, ok := value.(string)
			if !ok {
				return settings, headers, body, "settings.headers must be an object of string values"
			}
			values[key] = text
		}
		headers = Headers(values, headers)
		delete(settings, "headers")
	}
	if hasBody {
		raw, ok := legacyBody.(map[string]any)
		if !ok {
			return settings, headers, body, "settings.extraBody must be an object"
		}
		body = Merge(raw, body)
		delete(settings, "extraBody")
	}
	return settings, headers, body, ""
}

func resolverAddRoute(routes map[string]Route, r Route) error {
	if _, ok := routes[r.Alias]; ok {
		return fmt.Errorf("duplicate Route alias %q", r.Alias)
	}
	routes[r.Alias] = r
	return nil
}

type resolverProtocol struct{ protocol, baseURL string }

// Exact package allowlist. Families needing custom signing, deployment URL
// templates, dynamic protocol dispatch, or different paths remain unsupported.
var resolverProtocols = map[string]resolverProtocol{
	"@opencode/ai/providers/openai":                      {"openai-response", "https://api.openai.com/v1"},
	"@opencode/ai/providers/openai/responses":            {"openai-response", "https://api.openai.com/v1"},
	"@opencode/ai/providers/openai/chat":                 {"openai", "https://api.openai.com/v1"},
	"@opencode/ai/providers/openai-compatible":           {"openai", ""},
	"@opencode/ai/providers/openai-compatible/responses": {"openai-response", ""},
	"@opencode/ai/providers/openai-compatible-responses": {"openai-response", ""},
	"@opencode/ai/providers/anthropic":                   {"claude", "https://api.anthropic.com/v1"},
	"@opencode/ai/providers/anthropic-compatible":        {"claude", ""},
	"@opencode/ai/providers/google":                      {"gemini", "https://generativelanguage.googleapis.com/v1beta"},
}

// Legacy SDK names are normalized by the same package-only aliases as native
// v2. Special provider remappings are retained as explicit packages, so a
// provider requiring custom handling cannot silently fall through to a generic
// protocol. There is intentionally no model-name-based mapping.
func resolverPackage(pkg, provider, shape string) string {
	pkg = strings.TrimPrefix(pkg, "aisdk:")
	pkg = strings.Replace(pkg, "@opencode-ai/ai/", "@opencode/ai/", 1)
	if pkg == "" {
		return ""
	}
	families := map[string]string{"alibaba": "alibaba", "alibaba-cn": "alibaba", "alibaba-coding-plan": "alibaba", "alibaba-coding-plan-cn": "alibaba", "alibaba-token-plan": "alibaba", "alibaba-token-plan-cn": "alibaba", "kimi-for-coding": "moonshot", "meta": "meta", "minimax": "minimax", "minimax-cn": "minimax", "minimax-coding-plan": "minimax", "minimax-cn-coding-plan": "minimax", "moonshotai": "moonshot", "moonshotai-cn": "moonshot", "zai-coding-plan": "zai-coding-plan", "zhipuai-coding-plan": "zai-coding-plan"}
	if family := families[provider]; family != "" {
		suffix := map[string]string{"@ai-sdk/openai-compatible": "chat", "@ai-sdk/anthropic": "messages", "@ai-sdk/openai": "responses"}[pkg]
		if suffix != "" {
			return "@opencode/ai/providers/" + family + "/" + suffix
		}
	}
	if pkg == "@ai-sdk/openai-compatible" {
		special := map[string]string{"baseten": "baseten", "cohere": "cohere/chat", "cloudflare-workers-ai": "cloudflare-workers-ai", "deepseek": "deepseek", "digitalocean": "digitalocean", "fireworks-ai": "fireworks", "google-vertex": "google-vertex/chat", "zai": "zai/chat", "zhipuai": "zai/chat"}
		if name := special[provider]; name != "" {
			return "@opencode/ai/providers/" + name
		}
	}
	if provider == "cloudflare-ai-gateway" && (pkg == "@ai-sdk/openai-compatible" || pkg == "@ai-sdk/openai" || pkg == "@ai-sdk/anthropic" || pkg == "ai-gateway-provider") {
		return "@opencode/ai/providers/cloudflare-ai-gateway"
	}
	if provider == "google-vertex" && pkg == "@ai-sdk/mistral" {
		return "@opencode/ai/providers/google-vertex/mistral"
	}
	if pkg == "@ai-sdk/azure" && shape == "completions" {
		return "@opencode/ai/providers/azure/chat"
	}
	aliases := map[string]string{"@ai-sdk/openai": "openai", "@ai-sdk/openai-compatible": "openai-compatible", "@ai-sdk/anthropic": "anthropic", "@ai-sdk/google": "google", "@ai-sdk/google-vertex": "google-vertex", "@ai-sdk/google-vertex/anthropic": "google-vertex/messages", "@ai-sdk/azure": "azure/responses", "@ai-sdk/amazon-bedrock": "amazon-bedrock", "@ai-sdk/alibaba": "alibaba/chat", "@ai-sdk/cerebras": "cerebras", "@ai-sdk/cohere": "cohere", "@ai-sdk/deepinfra": "deepinfra", "@ai-sdk/groq": "groq", "@ai-sdk/mistral": "mistral", "@ai-sdk/togetherai": "togetherai", "@ai-sdk/xai": "xai", "@ai-sdk/gateway": "vercel-ai-gateway", "@openrouter/ai-sdk-provider": "openrouter", "ai-gateway-provider": "cloudflare-ai-gateway", "venice-ai-sdk-provider": "venice"}
	if name := aliases[pkg]; name != "" {
		return "@opencode/ai/providers/" + name
	}
	return pkg
}

func resolverDecode(data []byte, dest any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("expected JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(dest); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func resolverSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func resolverStrings(values []string) []any {
	result := make([]any, len(values))
	for i, v := range values {
		result[i] = v
	}
	return result
}

// Clone copies JSON-compatible maps and arrays without sharing their mutable storage.
func Clone(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return Merge(nil, v)
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = Clone(item)
		}
		return result
	default:
		return value
	}
}

// Merge recursively combines JSON objects; right-side arrays and null values replace left values.
func Merge(left, right map[string]any) map[string]any {
	out := make(map[string]any, len(left)+len(right))
	for k, v := range left {
		out[k] = Clone(v)
	}
	for k, v := range right {
		if lm, ok := out[k].(map[string]any); ok {
			if rm, ok := v.(map[string]any); ok {
				out[k] = Merge(lm, rm)
				continue
			}
		}
		out[k] = Clone(v)
	}
	return out
}
func resolverShallow(left, right map[string]any) map[string]any {
	out := Merge(nil, left)
	for k, v := range right {
		out[k] = Clone(v)
	}
	return out
}
func resolverCleanSettings(settings map[string]any) map[string]any {
	out := Merge(nil, settings)
	delete(out, "apiKey")
	delete(out, "authToken")
	delete(out, "accessToken")
	return out
}
func resolverModelSettings(settings map[string]any) map[string]any {
	out := resolverCleanSettings(settings)
	for _, k := range []string{"chunkTimeout", "headerTimeout", "timeout", "transport"} {
		delete(out, k)
	}
	return out
}

// Headers merges case-insensitive names deterministically, preferring the right side.
func Headers(left, right map[string]string) map[string]string {
	out := make(map[string]string, len(left)+len(right))
	names := map[string]string{}
	for _, source := range []map[string]string{left, right} {
		for _, k := range resolverSortedKeys(source) {
			lower := strings.ToLower(k)
			if old, ok := names[lower]; ok {
				delete(out, old)
			}
			names[lower] = k
			out[k] = source[k]
		}
	}
	return out
}
func resolverCapabilities(base, override map[string]any) map[string]any {
	result := map[string]any{"tools": false, "input": []any{}, "output": []any{}}
	for _, source := range []map[string]any{base, override} {
		for _, k := range []string{"tools", "input", "output", "reasoning", "reasoning_options"} {
			if v, ok := source[k]; ok && v != nil {
				result[k] = Clone(v)
			}
		}
	}
	return result
}

// Reasoning options are declarations, not provider request recipes. Keep their
// type and exact values (including unknown future option types) without creating
// variants, guessing budgets, or conflating a toggle with an effort tier.
func resolverReasoning(base map[string]any, m Model) map[string]any {
	result := resolverShallow(nil, base)
	if m.Reasoning != nil {
		result["reasoning"] = *m.Reasoning
	}
	if m.ReasoningOptions != nil {
		result["reasoning_options"] = Clone(m.ReasoningOptions)
	}
	return result
}
func resolverCloneModel(m Model) Model {
	m.Settings = Merge(nil, m.Settings)
	m.Headers = Headers(nil, m.Headers)
	m.Body = Merge(nil, m.Body)
	m.Compatibility = resolverShallow(nil, m.Compatibility)
	m.Capabilities = resolverShallow(nil, m.Capabilities)
	if m.Reasoning != nil {
		value := *m.Reasoning
		m.Reasoning = &value
	}
	if m.ReasoningOptions != nil {
		m.ReasoningOptions = Clone(m.ReasoningOptions).([]any)
	}
	m.Limit = resolverShallow(nil, m.Limit)
	m.Variants = append([]Variant(nil), m.Variants...)
	return m
}
func resolverVariants(base, overrides []Variant) ([]Variant, error) {
	result := make([]Variant, 0, len(base)+len(overrides))
	index := map[string]int{}
	for _, source := range [][]Variant{base, overrides} {
		for _, v := range source {
			if v.ID == "" {
				return nil, fmt.Errorf("empty variant ID")
			}
			var previous Variant
			i, exists := index[v.ID]
			if exists {
				previous = result[i]
			}
			merged := Variant{ID: v.ID, Settings: Merge(resolverCleanSettings(previous.Settings), resolverCleanSettings(v.Settings)), Headers: Headers(previous.Headers, v.Headers), Body: Merge(previous.Body, v.Body)}
			if exists {
				result[i] = merged
			} else {
				index[v.ID] = len(result)
				result = append(result, merged)
			}
		}
	}
	return result, nil
}

// resolverOrderedObject mirrors JSON.parse plus Object.keys enumeration: array
// index keys first in numeric order, then other keys in original insertion order.
// Repeated JSON member names keep their first position and last value.
func resolverOrderedObject(data []byte) (map[string]json.RawMessage, []string, error) {
	var values map[string]json.RawMessage
	if err := resolverDecode(data, &values); err != nil {
		return nil, nil, err
	}
	if values == nil {
		return nil, nil, fmt.Errorf("expected object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if _, err := decoder.Token(); err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	var indexes, others []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, nil, fmt.Errorf("expected object key")
		}
		var ignored json.RawMessage
		if err := decoder.Decode(&ignored); err != nil {
			return nil, nil, err
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		n, err := strconv.ParseUint(key, 10, 32)
		if err == nil && n < 4294967295 && strconv.FormatUint(n, 10) == key {
			indexes = append(indexes, key)
		} else {
			others = append(others, key)
		}
	}
	sort.Slice(indexes, func(i, j int) bool {
		a, _ := strconv.ParseUint(indexes[i], 10, 32)
		b, _ := strconv.ParseUint(indexes[j], 10, 32)
		return a < b
	})
	return values, append(indexes, others...), nil
}

// Xm/XK in the official 2.0.29 catalog registration pipeline.
func resolverCatalogModelVisible(provider string, m Model) bool {
	if m.Status == "deprecated" {
		return false
	}
	for _, direction := range []string{"input", "output"} {
		modalities, _ := m.Capabilities[direction].([]any)
		hasText := len(modalities) == 0
		for _, value := range modalities {
			if text, ok := value.(string); ok && strings.HasPrefix(text, "text") {
				hasText = true
			}
		}
		if !hasText {
			return false
		}
	}
	if provider == "amazon-bedrock" {
		for _, model := range []string{"amazon.nova-2-lite-v1:0", "anthropic.claude-fable-5", "anthropic.claude-fable-5-1", "anthropic.claude-haiku-4-5-20251001-v1:0", "anthropic.claude-opus-4-1-20250805-v1:0", "anthropic.claude-opus-4-5-20251101-v1:0", "anthropic.claude-opus-4-7", "anthropic.claude-opus-4-8", "anthropic.claude-opus-5", "anthropic.claude-sonnet-4-5-20250929-v1:0", "anthropic.claude-sonnet-5", "deepseek.r1-v1:0", "mistral.pixtral-large-2502-v1:0"} {
			if m.ModelID == model {
				return false
			}
		}
	}
	return true
}
