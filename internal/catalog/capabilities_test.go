package catalog

import (
    "os"
    "reflect"
    "testing"
)

func capabilityFixture(t *testing.T) []byte {
    t.Helper()
    data, err := os.ReadFile("testdata/capabilities.json")
    if err != nil { t.Fatal(err) }
    return data
}

func TestCatalogDeclaredCapabilitiesReachPublicRoutes(t *testing.T) {
    routes, err := ResolvePublicRoutes(capabilityFixture(t))
    if err != nil { t.Fatal(err) }
    if len(routes) != 9 { t.Fatalf("unexpected routes or synthesized variants: %d", len(routes)) }
    r := routes["opencode-zen/anonymous-preview"]
    want := []any{map[string]any{"type":"effort","values":[]any{"low","medium","high","xhigh","max"}}}
    if r.Capabilities["reasoning"] != true || !reflect.DeepEqual(r.Capabilities["reasoning_options"], want) {
        t.Fatalf("declarations lost: %#v", r.Capabilities)
    }
    if r.Limit["context"] != float64(1048576) || r.Limit["input"] != float64(524288) || r.Limit["output"] != float64(131072) { t.Fatal(r.Limit) }
    if !reflect.DeepEqual(r.Capabilities["input"], []any{"text","image","video"}) { t.Fatal(r.Capabilities) }
    for _, hidden := range []string{"paid","non-text","deprecated"} {
        if _, ok := routes["opencode-zen/"+hidden]; ok { t.Fatalf("metadata changed eligibility: %s", hidden) }
    }
    if routes["opencode-zen/unknown-package"].Unsupported == "" { t.Fatal("metadata bypassed package allowlist") }
    unknown := routes["opencode-zen/unknown-option"].Capabilities["reasoning_options"]
    if !reflect.DeepEqual(unknown, []any{map[string]any{"type":"future-option","values":[]any{"future"}}}) { t.Fatal(unknown) }
}

func TestCapabilityOverridePrecedenceAndExplicitClear(t *testing.T) {
    config := `{"providers":{"opencode":{"models":{
      "alias":{"modelID":"anonymous-preview","reasoning_options":[{"type":"effort","values":["medium","max"]}],"capabilities":{"input":["text"]},"limit":{"output":2048},"variants":[{"id":"explicit","settings":{"reasoningEffort":"max"}}]},
      "anonymous-preview":{"reasoning":true,"capabilities":{"reasoning":false}},
      "toggle-only":{"reasoning_options":[]},
      "budget-minimum":{"reasoning_options":[{"type":"budget_tokens","max":4096}]}
    }}}}`
    routes, err := ResolveRoutes([]byte(config), capabilityFixture(t))
    if err != nil { t.Fatal(err) }
    r := routes["opencode-zen/alias"]
    want := []any{map[string]any{"type":"effort","values":[]any{"medium","max"}}}
    if !reflect.DeepEqual(r.Capabilities["reasoning_options"],want) || r.Capabilities["reasoning"] != true { t.Fatal(r.Capabilities) }
    if r.Limit["context"] != float64(1048576) || r.Limit["input"] != float64(524288) || r.Limit["output"] != float64(2048) { t.Fatal(r.Limit) }
    if !reflect.DeepEqual(r.Capabilities["input"],[]any{"text"}) || !reflect.DeepEqual(r.Capabilities["output"],[]any{"text"}) { t.Fatal(r.Capabilities) }
    if routes["opencode-zen/anonymous-preview"].Capabilities["reasoning"] != false { t.Fatal("explicit false lost") }
    if got := routes["opencode-zen/toggle-only"].Capabilities["reasoning_options"].([]any); len(got) != 0 { t.Fatal("explicit empty list did not clear options") }
    budget := routes["opencode-zen/budget-minimum"].Capabilities["reasoning_options"].([]any)[0].(map[string]any)
    if _, stale := budget["min"]; stale { t.Fatal("options were recursively merged instead of replaced") }
    v := routes["opencode-zen/alias/variant/explicit"]
    if v.Settings["reasoningEffort"] != "max" || !reflect.DeepEqual(v.Capabilities, r.Capabilities) { t.Fatal(v) }
    // Resolving again must not retain any mutations from a previously returned route.
    r.Capabilities["reasoning_options"].([]any)[0].(map[string]any)["values"] = []any{"mutated"}
    fresh, err := ResolvePublicRoutes(capabilityFixture(t))
    if err != nil || reflect.DeepEqual(fresh["opencode-zen/anonymous-preview"].Capabilities["reasoning_options"],r.Capabilities["reasoning_options"]) { t.Fatalf("mutable metadata leaked: %v",err) }
}

func TestNativeCapabilityMetadataPrecedesPublicSchema(t *testing.T) {
    routes := resolverTestRoutes(t, `{"providers":{}}`, `{"opencode":{"npm":"@ai-sdk/openai-compatible","api":"https://opencode.ai/inference/v1","models":{"m":{"reasoning":true,"reasoning_options":[{"type":"effort","values":["low"]}],"capabilities":{"reasoning":false,"reasoning_options":[],"input":["text","pdf"],"output":["text"]}}}}}`)
    caps := routes["opencode-zen/m"].Capabilities
    if caps["reasoning"] != false || len(caps["reasoning_options"].([]any)) != 0 || !reflect.DeepEqual(caps["input"],[]any{"text","pdf"}) { t.Fatal(caps) }
}

func TestMalformedCapabilityDocumentsFailClosed(t *testing.T) {
    for _, fragment := range []string{`"reasoning":"yes"`, `"reasoning_options":{}`, `"modalities":{"input":"image"}`} {
        data := []byte(`{"opencode":{"models":{"m":{`+fragment+`}}}}`)
        if _, err := ParseCatalog(data); err == nil { t.Fatalf("malformed metadata accepted: %s",fragment) }
    }
}
