package plugin

import (
    "context"
    "encoding/json"
    "io"
    "math"
    "net/http"
    "os"
    "reflect"
    "strings"
    "testing"
    "time"

    "github.com/megumin31/cpa-opencode-zen/internal/catalog"
    wire "github.com/megumin31/cpa-opencode-zen/internal/protocol"
    "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func modelMetadataFixture(t *testing.T) []byte {
    t.Helper()
    raw, err := os.ReadFile("../catalog/testdata/capabilities.json")
    if err != nil { t.Fatal(err) }
    return raw
}

func metadataByID(models []pluginapi.ModelInfo) map[string]pluginapi.ModelInfo {
    out := map[string]pluginapi.ModelInfo{}
    for _, m := range models { out[m.ID] = m }
    return out
}

func TestModelInfoUsesOnlyDeclaredCapabilities(t *testing.T) {
    routes, err := catalog.ResolvePublicRoutes(modelMetadataFixture(t))
    if err != nil { t.Fatal(err) }
    models := metadataByID(modelInfos(defaultConfig(), routes))
    if len(models) != 8 { t.Fatalf("eligibility changed: %d",len(models)) }
    m := models["opencode-zen/anonymous-preview"]
    want := &pluginapi.ThinkingSupport{Levels:[]string{"low","medium","high","xhigh","max"}}
    if !reflect.DeepEqual(m.Thinking,want) { t.Fatalf("effort declaration: %#v",m.Thinking) }
    if m.ContextLength != 1048576 || m.InputTokenLimit != 524288 || m.OutputTokenLimit != 131072 || m.MaxCompletionTokens != 131072 { t.Fatal(m) }
    if !reflect.DeepEqual(m.SupportedInputModalities,[]string{"text","image","video"}) || !reflect.DeepEqual(m.SupportedOutputModalities,[]string{"text"}) { t.Fatal(m) }
    toggle := models["opencode-zen/toggle-only"].Thinking
    if !reflect.DeepEqual(toggle,&pluginapi.ThinkingSupport{ZeroAllowed:true}) { t.Fatalf("toggle conflated with effort/auto/budget: %#v",toggle) }
    budget := models["opencode-zen/budget-minimum"]
    if !reflect.DeepEqual(budget.Thinking,&pluginapi.ThinkingSupport{Min:1024}) || budget.ContextLength != 0 || budget.InputTokenLimit != 0 || budget.OutputTokenLimit != 64000 { t.Fatal(budget) }
    maximum := models["opencode-zen/budget-maximum"]
    if !reflect.DeepEqual(maximum.Thinking,&pluginapi.ThinkingSupport{Max:81920,ZeroAllowed:true}) || !reflect.DeepEqual(maximum.SupportedOutputModalities,[]string{"text","audio"}) { t.Fatal(maximum) }
    if !reflect.DeepEqual(models["opencode-zen/reasoning-unspecified"].Thinking,&pluginapi.ThinkingSupport{}) { t.Fatal("reasoning:true invented controls") }
    for _, name := range []string{"metadata-missing","reasoning-disabled","unknown-option"} {
        if models["opencode-zen/"+name].Thinking != nil { t.Fatalf("invented thinking for %s",name) }
    }
    missing := models["opencode-zen/metadata-missing"]
    if missing.ContextLength != 0 || missing.InputTokenLimit != 0 || missing.OutputTokenLimit != 0 || len(missing.SupportedInputModalities) != 0 || len(missing.SupportedOutputModalities) != 0 { t.Fatal("missing metadata filled with guesses",missing) }
    // Returned SDK arrays are independent of the cached catalog metadata.
    m.Thinking.Levels[0] = "changed"
    m.SupportedInputModalities[0] = "changed"
    again := metadataByID(modelInfos(defaultConfig(),routes))[m.ID]
    if again.Thinking.Levels[0] != "low" || again.SupportedInputModalities[0] != "text" { t.Fatal("ModelInfo mutated cached metadata") }
}

func TestMetadataIntegerDoesNotInventLimits(t *testing.T) {
    for _, value := range []any{nil,-1,0,1.5,math.Inf(1),math.NaN(),math.Exp2(63),"4096",json.Number("9223372036854775808")} {
        if integer(value) != 0 { t.Fatalf("invalid declared limit: %#v",value) }
    }
    for _, value := range []any{4096,int64(4096),float64(4096),json.Number("4096")} {
        if integer(value) != 4096 { t.Fatal(value) }
    }
}

func TestThinkingOptionsDoNotInventUnknownModes(t *testing.T) {
    caps := map[string]any{"reasoning_options":[]any{
        map[string]any{"type":"effort","values":[]any{"none","high","high","max"}},
        map[string]any{"type":"future-type","values":[]any{"auto"}},
    }}
    want := &pluginapi.ThinkingSupport{ZeroAllowed:true,Levels:[]string{"none","high","max"}}
    if got := thinkingInfo(caps); !reflect.DeepEqual(got,want) { t.Fatalf("%#v",got) }
}

func TestCapabilityMetadataRefreshAndErrorRetention(t *testing.T) {
    engine := newTestEngine(t)
    engine.current.config = defaultConfig()
    now := time.Unix(1800000000,0)
    engine.now = func() time.Time { return now }
    snapshot := string(modelMetadataFixture(t))
    calls := 0
    engine.clientFactory = func(string) (*http.Client,error) {
        return &http.Client{Transport:discoveryRoundTripFunc(func(r *http.Request) (*http.Response,error) {
            calls++
            if r.URL.String() != "https://models.opencode.ai/api.json" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Org-Id") != "" { t.Fatalf("unexpected metadata request: %v",r.URL) }
            return &http.Response{StatusCode:200,Header:make(http.Header),Body:io.NopCloser(strings.NewReader(snapshot)),Request:r},nil
        })},nil
    }
    read := func() pluginapi.ModelInfo {
        routes, err := engine.discover(engine.current,context.Background(),credential{Key:"public",Server:"https://opencode.ai/console"},"",false)
        if err != nil { t.Fatal(err) }
        return metadataByID(modelInfos(engine.current.config,routes))["opencode-zen/anonymous-preview"]
    }
    initial := read()
    if initial.Thinking == nil || initial.ContextLength != 1048576 || calls != 1 { t.Fatal(initial,calls) }
    snapshot = strings.ReplaceAll(snapshot,`"context":1048576`,`"context":2097152`)
    snapshot = strings.ReplaceAll(snapshot,`["low","medium","high","xhigh","max"]`,`["high","max"]`)
    now = now.Add(time.Minute)
    if m := read(); m.ContextLength != 1048576 || calls != 1 { t.Fatal("route TTL prematurely refreshed metadata",m,calls) }
    now = now.Add(4*time.Minute)
    if m := read(); m.ContextLength != 2097152 || !reflect.DeepEqual(m.Thinking.Levels,[]string{"high","max"}) || calls != 2 { t.Fatal("catalog TTL did not refresh metadata",m,calls) }
    valid := snapshot
    snapshot = `{"opencode":{"models":{"m":{"reasoning":"invalid"}}}}`
    now = now.Add(5*time.Minute)
    if m := read(); m.ContextLength != 2097152 || calls != 3 { t.Fatal("invalid refresh replaced validated metadata",m,calls) }
    snapshot = strings.ReplaceAll(valid,`["high","max"]`,`["medium"]`)
    now = now.Add(time.Minute)
    if m := read(); !reflect.DeepEqual(m.Thinking.Levels,[]string{"medium"}) || calls != 4 { t.Fatal("failed refresh never recovered",m,calls) }
}

func TestCatalogEffortsReachFinalOpenAIWireWithoutTierGuessing(t *testing.T) {
    routes, err := catalog.ResolvePublicRoutes(modelMetadataFixture(t))
    if err != nil { t.Fatal(err) }
    original := routes["opencode-zen/anonymous-preview"]
    for _, level := range []string{"low","medium","high","xhigh","max"} {
        for _, source := range []string{"openai","openai-response"} {
            for _, target := range []string{"openai","openai-response"} {
                t.Run(level+"/"+source+"/"+target,func(t *testing.T) {
                    r := original
                    r.Protocol = target
                    r.Package = "@opencode/ai/providers/openai-compatible"
                    if target == "openai-response" { r.Package = "@opencode/ai/providers/openai-compatible/responses" }
                    request := `{"messages":[{"role":"user","content":"synthetic"}],"reasoning_effort":"`+level+`"}`
                    if source == "openai-response" { request = `{"input":"synthetic","reasoning":{"effort":"`+level+`"}}` }
                    raw, targetURL, _, err := wire.Prepare(context.Background(),r,wire.FormatName(source),[]byte(request),wire.Options{})
                    if err != nil { t.Fatal(err) }
                    var body map[string]any
                    if err := json.Unmarshal(raw,&body); err != nil { t.Fatal(err) }
                    effort := body["reasoning_effort"]
                    if target == "openai-response" { effort = object(body["reasoning"])["effort"] }
                    if effort != level || body["model"] != "anonymous-preview" { t.Fatalf("selection lost: %s",raw) }
                    suffix := "/chat/completions"
                    if target == "openai-response" { suffix = "/responses" }
                    if !strings.HasSuffix(targetURL,suffix) { t.Fatal(targetURL) }
                })
            }
        }
    }
}

func TestCatalogNativeToggleBudgetAndConfiguredPrecedence(t *testing.T) {
    engine := newTestEngine(t)
    engine.testConfig(t,"http://127.0.0.1:18080")
    for _, mode := range []string{"enabled","disabled"} {
        extra := `"type":"`+mode+`"`
        if mode == "enabled" { extra += `,"budget_tokens":2048` }
        body,err := engine.prepareSemanticFixture(t,"claude","claude",`{"messages":[{"role":"user","content":"synthetic"}],"max_tokens":8192,"thinking":{`+extra+`}}`,nil)
        if err != nil || object(body["thinking"])["type"] != mode { t.Fatal(body,err) }
        if mode == "enabled" && object(body["thinking"])["budget_tokens"] != float64(2048) { t.Fatal(body) }
    }
    body,err := engine.prepareSemanticFixture(t,"gemini","gemini",`{"contents":[{"role":"user","parts":[{"text":"synthetic"}]}],"generationConfig":{"maxOutputTokens":8192,"thinkingConfig":{"thinkingBudget":2048}}}`,nil)
    if err != nil || object(object(body["generationConfig"])["thinkingConfig"])["thinkingBudget"] != float64(2048) { t.Fatal(body,err) }
    body,err = engine.prepareSemanticFixture(t,"openai","openai",`{"messages":[{"role":"user","content":"synthetic"}],"reasoning_effort":"low"}`,func(r *catalog.Route) {
        r.Settings = map[string]any{"reasoningEffort":"high"}
        r.Body = map[string]any{"reasoning_effort":"max"}
    })
    if err != nil || body["reasoning_effort"] != "max" { t.Fatal("configured precedence changed",body,err) }
    for _, raw := range []string{
        `{"messages":[{"role":"user","content":"synthetic"}],"thinking":{"type":"enabled","budget_tokens":2048}}`,
        `{"messages":[{"role":"user","content":"synthetic"}],"thinking":{"type":"disabled"}}`,
    } {
        if _,err := engine.prepareSemanticFixture(t,"claude","openai",raw,nil); err == nil { t.Fatal("lossy budget/toggle conversion invented an effort") }
    }
}

// Capture the complete executor's outbound HTTP request, not just the prepared
// map. All traffic is intercepted by a fake RoundTripper; no service is called.
func TestCatalogEffortSelectionReachesOutboundRequest(t *testing.T) {
    engine := newTestEngine(t)
    engine.current.config = defaultConfig()
    snapshot := string(modelMetadataFixture(t))
    inference, discovery := 0,0
    wantLevel := ""
    engine.clientFactory = func(string) (*http.Client,error) {
        return &http.Client{Transport:discoveryRoundTripFunc(func(r *http.Request) (*http.Response,error) {
            response := snapshot
            if r.URL.String() == "https://models.opencode.ai/api.json" {
                discovery++
                if r.Header.Get("Authorization") != "" { t.Error("catalog received credentials") }
            } else {
                inference++
                if r.Method != "POST" || r.URL.String() != "https://opencode.ai/inference/openai/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer public" { t.Fatalf("unexpected inference target: %s %v",r.Method,r.URL) }
                var body map[string]any
                if err := json.NewDecoder(r.Body).Decode(&body); err != nil { t.Fatal(err) }
                if body["reasoning_effort"] != wantLevel || body["model"] != "anonymous-preview" { t.Fatalf("outbound lost declared effort: %#v",body) }
                response = nativeFixture("openai")
            }
            return &http.Response{StatusCode:200,Header:make(http.Header),Body:io.NopCloser(strings.NewReader(response)),Request:r},nil
        })},nil
    }
    c := credential{Type:providerID,Key:"public",Server:"https://opencode.ai/console"}
    for _, level := range []string{"xhigh","max"} {
        wantLevel = level
        for _, source := range []string{"openai","openai-response"} {
            payload := `{"messages":[{"role":"user","content":"synthetic"}],"reasoning_effort":"`+level+`"}`
            if source == "openai-response" { payload = `{"input":"synthetic","reasoning":{"effort":"`+level+`"}}` }
            req := rpcExecutorRequest{ExecutorRequest:pluginapi.ExecutorRequest{Model:"opencode-zen/anonymous-preview",Format:source,SourceFormat:source,Payload:[]byte(payload),StorageJSON:rawJSON(t,c)}}
            if _,err := engine.execute(rawJSON(t,req),false); err != nil { t.Fatal(source,level,err) }
        }
    }
    if inference != 4 || discovery != 1 { t.Fatalf("unexpected request counts: inference=%d discovery=%d",inference,discovery) }
    // A registered capability does not silently create suffix aliases.
    req := rpcExecutorRequest{ExecutorRequest:pluginapi.ExecutorRequest{Model:"opencode-zen/anonymous-preview(max)",Format:"openai",SourceFormat:"openai",Payload:[]byte(clientRequest("openai")),StorageJSON:rawJSON(t,c)}}
    if _,err := engine.execute(rawJSON(t,req),false); err == nil || inference != 4 { t.Fatal("unconfigured suffix executed an inference",err,inference) }
}
