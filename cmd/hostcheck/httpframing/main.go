// httpframing verifies the final HTTP bytes, not just executor payloads. It loads
// the actual C-shared plugin into the pinned, unmodified CPA native host and
// exercises all four real SDK HTTP handlers against a loopback-only upstream.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"
)

const fakeKey = "httpframing-fake-key"

var protocols = []string{"chat", "responses", "anthropic", "gemini"}

func main() {
	plugin := flag.String("plugin", "", "absolute path to versioned native plugin library")
	matrix := flag.Bool("matrix", false, "also exercise all cross-protocol conversions")
	scenario := flag.String("scenario", "success", "upstream scenario: success, truncated, error, blocked, empty-feedback, responses-tools, responses-tools-error, responses-tools-cancel, responses-tools-incomplete")
	flag.Parse()
	if *plugin == "" {
		fmt.Fprintln(os.Stderr, "-plugin is required")
		os.Exit(2)
	}
	watchdog := time.AfterFunc(60*time.Second, func() { fmt.Fprintln(os.Stderr, "FAIL: httpframing timed out"); os.Exit(1) })
	defer watchdog.Stop()
	if err := runScenario(*plugin, *matrix, *scenario); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func run(pluginPath string, matrix bool) error { return runScenario(pluginPath, matrix, "success") }

func runScenario(pluginPath string, matrix bool, scenario string) error {
	if scenario != "success" && scenario != "truncated" && scenario != "error" && scenario != "blocked" && scenario != "empty-feedback" && scenario != "responses-tools" && scenario != "responses-tools-error" && scenario != "responses-tools-cancel" && scenario != "responses-tools-incomplete" {
		return fmt.Errorf("unknown scenario %q", scenario)
	}
	gin.SetMode(gin.ReleaseMode)
	tmp, err := os.MkdirTemp("", "opencode-httpframing-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	pluginDir := filepath.Join(tmp, "plugins")
	if err = os.Mkdir(pluginDir, 0700); err != nil {
		return err
	}
	binary, err := os.ReadFile(pluginPath)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(pluginDir, filepath.Base(pluginPath)), binary, 0700); err != nil {
		return err
	}
	catalog := filepath.Join(tmp, "catalog.json")
	if err = os.WriteFile(catalog, []byte(`{}`), 0600); err != nil {
		return err
	}
	var mu sync.Mutex
	var failures []error
	var cancelRequest context.CancelFunc
	upstreamCancelled := make(chan struct{}, 1)
	calls := make(map[string]int)
	var mock *httptest.Server
	mock = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reject := func(s string) {
			mu.Lock()
			failures = append(failures, errors.New(s))
			mu.Unlock()
			http.Error(w, s, 400)
		}
		if r.Host != strings.TrimPrefix(mock.URL, "http://") || r.Method == http.MethodConnect {
			reject("non-mock destination")
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/console/api/v2/config" {
			if r.Header.Get("Authorization") != "Bearer "+fakeKey {
				reject("bad discovery key")
				return
			}
			providers := map[string]any{}
			for _, p := range protocols {
				s, _ := protocolSpecFor(p)
				providers[p] = map[string]any{"package": "aisdk:" + s.packageName, "settings": map[string]any{"baseURL": mock.URL + "/" + p + s.basePath}, "models": map[string]any{"space-bunny-free": map[string]any{}}}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"providers": providers})
			return
		}
		for _, p := range protocols {
			s, _ := protocolSpecFor(p)
			if r.Method == http.MethodPost && (r.URL.RequestURI() == "/"+p+s.path || (p == "anthropic" && r.URL.RequestURI() == "/"+p+s.path+"?beta=true")) {
				if r.Header.Get(s.authHeader) != s.authValue {
					reject("bad inference key")
					return
				}
				raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
				if !json.Valid(raw) {
					reject("invalid request JSON")
					return
				}
				mu.Lock()
				calls[p]++
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, scenarioStream(p, scenario, s.stream))
				w.(http.Flusher).Flush()
				if scenario == "responses-tools-cancel" {
					mu.Lock()
					stop := cancelRequest
					mu.Unlock()
					timer := time.AfterFunc(100*time.Millisecond, stop)
					defer timer.Stop()
					<-r.Context().Done()
					upstreamCancelled <- struct{}{}
				}
				return
			}
		}
		reject("unexpected mock path: " + r.URL.RequestURI())
	}))
	defer mock.Close()
	oldTransport := http.DefaultTransport
	transport := oldTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != strings.TrimPrefix(mock.URL, "http://") {
			return nil, errors.New("non-mock dial blocked")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	http.DefaultTransport = transport
	defer func() { transport.CloseIdleConnections(); http.DefaultTransport = oldTransport }()
	var node yaml.Node
	yamlText := fmt.Sprintf("enabled: true\nallow_loopback_http: true\nrequest_timeout_seconds: 5\ncatalog_file: %q\nproxy_url: %q\nallowed_destinations:\n  - %q\n", catalog, mock.URL, mock.URL)
	if err = yaml.Unmarshal([]byte(yamlText), &node); err != nil {
		return err
	}
	enabled := true
	cfg := &config.Config{AuthDir: tmp, Plugins: config.PluginsConfig{Enabled: true, Dir: pluginDir, Configs: map[string]config.PluginInstanceConfig{"opencode": {Enabled: &enabled, Raw: *node.Content[0]}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	host := pluginhost.New()
	defer host.ShutdownAll()
	host.ApplyConfig(ctx, cfg)
	if len(host.RegisteredPlugins()) != 1 {
		return errors.New("native plugin not registered")
	}
	raw, _ := json.Marshal(map[string]string{"type": "opencode", "key": fakeKey, "server": mock.URL + "/console"})
	auth, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "opencode", FileName: "fake.json", RawJSON: raw})
	if err != nil || !handled || auth == nil {
		return fmt.Errorf("auth parse: %v", err)
	}
	models := host.ModelsForAuth(ctx, auth)
	if models.Err != nil || len(models.Models) != 4 {
		return fmt.Errorf("discovery: %d models, %v", len(models.Models), models.Err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	host.RegisterExecutors(manager, nil)
	auth.Status = coreauth.StatusActive
	if auth.Metadata == nil {
		auth.Metadata = map[string]any{}
	}
	auth.Metadata["disable_cooling"] = true
	if _, err = manager.Register(ctx, auth); err != nil {
		return err
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, models.Models)
	defer registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
	base.SetPluginHost(host)
	base.SetModelRouterHost(host)
	router := gin.New()
	router.POST("/v1/chat/completions", openai.NewOpenAIAPIHandler(base).ChatCompletions)
	router.POST("/v1/responses", openai.NewOpenAIResponsesAPIHandler(base).Responses)
	router.POST("/v1/messages", claude.NewClaudeCodeAPIHandler(base).ClaudeMessages)
	router.POST("/v1beta/models/*action", gemini.NewGeminiAPIHandler(base).GeminiHandler)
	var results []error
	for _, upstream := range protocols {
		if strings.HasPrefix(scenario, "responses-tools") && upstream != "responses" {
			continue
		}
		if (scenario == "blocked" || scenario == "empty-feedback") && upstream != "gemini" {
			continue
		}
		for _, client := range protocols {
			if strings.HasPrefix(scenario, "responses-tools") && client != "gemini" {
				continue
			}
			if !matrix && upstream != client {
				continue
			}
			model := "opencode/" + upstream + "/space-bunny-free"
			path, body := clientRequest(client, model)
			requestCtx, stopRequest := context.WithCancel(ctx)
			mu.Lock()
			cancelRequest = stopRequest
			mu.Unlock()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(requestCtx)
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			stopRequest()
			label := upstream + " -> " + client
			if upstream == "anthropic" && client == "gemini" {
				if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "Claude-to-Gemini response conversion is not supported safely") {
					results = append(results, fmt.Errorf("%s: expected explicit unsupported conversion rejection, got %d %s", label, recorder.Code, recorder.Body.String()))
				} else {
					fmt.Println("PASS:", label, "explicit unsupported conversion rejection")
				}
				continue
			}
			if scenario == "responses-tools-cancel" {
				select {
				case <-upstreamCancelled:
				case <-time.After(3 * time.Second):
					results = append(results, errors.New("cancelled client left upstream request open"))
				}
				if err := validateCancelled(recorder.Body.String()); err != nil {
					results = append(results, fmt.Errorf("cancelled tools: %w; body=%q", err, recorder.Body.String()))
				} else {
					fmt.Println("PASS: responses -> gemini cancellation closes upstream without success")
				}
				continue
			}
			if scenario == "responses-tools" || scenario == "responses-tools-incomplete" {
				if recorder.Code != 200 {
					results = append(results, fmt.Errorf("tools HTTP %d: %s", recorder.Code, recorder.Body.String()))
					continue
				}
				if scenario == "responses-tools-incomplete" && !strings.Contains(recorder.Body.String(), `"finishReason":"MAX_TOKENS"`) {
					results = append(results, fmt.Errorf("tools incomplete lost MAX_TOKENS: %s", recorder.Body.String()))
					continue
				}
				if err := validateToolGemini(recorder.Body.String()); err != nil {
					results = append(results, fmt.Errorf("tools: %w; body=%q", err, recorder.Body.String()))
				} else {
					fmt.Println("PASS: responses -> gemini consecutive tools, trailing text, final usage and single terminal")
				}
				continue
			}
			if scenario == "blocked" && client == "gemini" {
				if err := validateBlockedGemini(recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String()); err != nil {
					results = append(results, fmt.Errorf("%s blocked: %w; body=%q", label, err, recorder.Body.String()))
				} else {
					fmt.Println("PASS:", label, "blocked promptFeedback preserved without success")
				}
				continue
			}
			if scenario == "blocked" && !strings.Contains(recorder.Body.String(), "Gemini blocked the response") {
				results = append(results, fmt.Errorf("%s blocked: missing explicit blocked-conversion error; body=%q", label, recorder.Body.String()))
				continue
			}
			if scenario != "success" {
				if err := validateFailure(client, recorder.Code, recorder.Body.String()); err != nil {
					results = append(results, fmt.Errorf("%s %s: %w; body=%q", label, scenario, err, recorder.Body.String()))
				} else {
					fmt.Println("PASS:", label, scenario, "visible failure without synthesized success")
				}
				continue
			}
			if recorder.Code != 200 {
				results = append(results, fmt.Errorf("%s: HTTP %d: %s", label, recorder.Code, recorder.Body.String()))
				continue
			}
			if !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
				results = append(results, fmt.Errorf("%s: wrong content type", label))
				continue
			}
			if err := validateSSE(client, recorder.Body.String()); err != nil {
				results = append(results, fmt.Errorf("%s: %w; body=%q", label, err, recorder.Body.String()))
			} else {
				fmt.Println("PASS:", label, "strict JSON SSE, content and completion")
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	results = append(results, failures...)
	for _, p := range protocols {
		if strings.HasPrefix(scenario, "responses-tools") && p != "responses" {
			continue
		}
		if (scenario == "blocked" || scenario == "empty-feedback") && p != "gemini" {
			continue
		}
		if calls[p] == 0 {
			results = append(results, fmt.Errorf("no real native upstream request for %s", p))
		}
	}
	return errors.Join(results...)
}

func clientRequest(protocol, model string) (string, string) {
	switch protocol {
	case "chat":
		return "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"stream":true}`, model)
	case "responses":
		return "/v1/responses", fmt.Sprintf(`{"model":%q,"input":"hello","stream":true}`, model)
	case "anthropic":
		return "/v1/messages", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"max_tokens":32,"stream":true}`, model)
	default:
		return "/v1beta/models/" + model + ":streamGenerateContent?alt=sse", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`
	}
}

// Parse every event data field strictly. A double SSE prefix, duplicate [DONE],
// unfinished frame, error event or silent missing terminal marker must fail.
func validateSSE(protocol, body string) error {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	if !strings.HasSuffix(body, "\n\n") {
		return errors.New("unterminated SSE frame")
	}
	done, terminal, content := 0, 0, false
	for _, frame := range strings.Split(body, "\n\n") {
		var data []string
		event := ""
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case line == "", strings.HasPrefix(line, ":"), strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "retry:"):
			default:
				return fmt.Errorf("unexpected SSE field %q", line)
			}
		}
		if len(data) == 0 {
			continue
		}
		payload := strings.Join(data, "\n")
		if done > 0 || (terminal > 0 && (protocol == "responses" || protocol == "anthropic")) {
			return errors.New("event data after terminal marker")
		}
		if payload == "[DONE]" {
			if protocol != "chat" || terminal != 1 {
				return errors.New("unexpected or premature [DONE]")
			}
			done++
			continue
		}
		if !json.Valid([]byte(payload)) {
			return fmt.Errorf("non-JSON event data %q", payload)
		}
		j := gjson.Parse(payload)
		if j.Get("error").Exists() || j.Get("type").String() == "error" || event == "error" {
			return fmt.Errorf("error event: %s", payload)
		}
		switch protocol {
		case "chat":
			content = content || j.Get("choices.0.delta.content").String() == "OK"
			if j.Get("choices.0.finish_reason").String() != "" {
				terminal++
			}
		case "responses":
			content = content || j.Get("delta").String() == "OK" || j.Get("response.output.0.content.0.text").String() == "OK"
			if j.Get("type").String() == "response.completed" {
				terminal++
			}
			if event != "" && event != j.Get("type").String() {
				return errors.New("response event/type mismatch")
			}
		case "anthropic":
			content = content || j.Get("delta.text").String() == "OK"
			if j.Get("type").String() == "message_stop" {
				terminal++
			}
			if event != j.Get("type").String() {
				return errors.New("Claude event/type mismatch")
			}
		case "gemini":
			content = content || j.Get("candidates.0.content.parts.0.text").String() == "OK"
			if j.Get("candidates.0.finishReason").String() != "" {
				terminal++
			}
		}
	}
	if !content || terminal != 1 {
		return fmt.Errorf("content=%v terminal markers=%d", content, terminal)
	}
	wantDone := 0
	if protocol == "chat" {
		wantDone = 1
	}
	if done != wantDone {
		return fmt.Errorf("[DONE] count=%d want=%d", done, wantDone)
	}
	return nil
}

func scenarioStream(protocol, scenario, stream string) string {
	if strings.HasPrefix(scenario, "responses-tools") {
		tools := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_alpha\",\"call_id\":\"call_alpha\",\"name\":\"alpha\",\"arguments\":\"{\\\"value\\\":1}\"}}\n\nevent: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_beta\",\"call_id\":\"call_beta\",\"name\":\"beta\",\"arguments\":\"{\\\"value\\\":2}\"}}\n\n"
		stream = strings.ReplaceAll(stream, `"output_index":0`, `"output_index":2`)
		result := strings.Replace(stream, "event: response.output_item.added", tools+"event: response.output_item.added", 1)
		if scenario == "responses-tools-incomplete" {
			result = strings.ReplaceAll(result, "response.completed", "response.incomplete")
			result = strings.Replace(result, `"status":"completed","model"`, `"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"model"`, 1)
		}
		if scenario == "responses-tools-cancel" {
			result = strings.Split(result, "event: response.completed")[0]
		}
		if scenario == "responses-tools-error" {
			result = strings.Split(result, "event: response.completed")[0] + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"message\":\"synthetic failure after tools and text\"}}\n\n"
		}
		return result
	}
	if scenario == "blocked" {
		return "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\",\"safetyRatings\":[{\"category\":\"HARM_CATEGORY_DANGEROUS_CONTENT\",\"probability\":\"HIGH\"}]}}\n\n"
	}
	if scenario == "empty-feedback" {
		return "data: {\"promptFeedback\":{}}\n\n"
	}
	if scenario == "success" {
		return stream
	}
	var prefix string
	switch protocol {
	case "chat":
		prefix = strings.Split(stream, "\n\n")[0] + "\n\n"
	case "responses":
		prefix = strings.Split(stream, "event: response.output_text.done")[0]
	case "anthropic":
		prefix = strings.Split(stream, "event: content_block_stop")[0]
	case "gemini":
		prefix = strings.Replace(stream, `,"finishReason":"STOP"`, "", 1)
	}
	if scenario == "error" {
		prefix += "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"message\":\"synthetic upstream failure\"}}\n\n"
	}
	return prefix
}

func validateFailure(protocol string, status int, body string) error {
	if !strings.Contains(body, `"error"`) {
		return errors.New("missing visible error")
	}
	if status >= 400 {
		if !json.Valid([]byte(body)) {
			return errors.New("HTTP error is not JSON")
		}
		return nil
	}
	if status != 200 {
		return fmt.Errorf("unexpected HTTP status %d", status)
	}
	for _, frame := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n") {
		var data []string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(data) == 0 {
			continue
		}
		payload := strings.Join(data, "\n")
		if payload == "[DONE]" {
			return errors.New("failure falsely emitted [DONE]")
		}
		if !json.Valid([]byte(payload)) {
			return fmt.Errorf("non-JSON failure event %q", payload)
		}
		j := gjson.Parse(payload)
		if j.Get("type").String() == "response.completed" || j.Get("type").String() == "message_stop" || j.Get("choices.0.finish_reason").String() != "" || j.Get("candidates.0.finishReason").String() != "" {
			return errors.New("failure falsely emitted successful terminal marker")
		}
	}
	return nil
}

func validateBlockedGemini(status int, contentType, body string) error {
	if status != 200 || !strings.HasPrefix(contentType, "text/event-stream") {
		return errors.New("blocked native Gemini must remain SSE HTTP 200")
	}
	if !strings.HasSuffix(body, "\n\n") {
		return errors.New("unterminated blocked frame")
	}
	count := 0
	for _, frame := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		if !strings.HasPrefix(frame, "data: ") {
			return errors.New("blocked frame missing single data prefix")
		}
		payload := strings.TrimPrefix(frame, "data: ")
		if !json.Valid([]byte(payload)) {
			return errors.New("blocked data is not JSON")
		}
		j := gjson.Parse(payload)
		if j.Get("promptFeedback.blockReason").String() != "SAFETY" || j.Get("candidates").Exists() || j.Get("error").Exists() {
			return errors.New("blocked reason lost or invented completion/error")
		}
		count++
	}
	if count != 1 {
		return fmt.Errorf("blocked frames=%d want=1", count)
	}
	return nil
}

func validateToolGemini(body string) error {
	if err := validateSSE("gemini", body); err != nil {
		return err
	}
	counts := map[string]int{}
	finished := false
	usage := false
	for _, frame := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			j := gjson.Parse(strings.TrimPrefix(line, "data: "))
			parts := j.Get("candidates.0.content.parts").Array()
			if finished && len(parts) > 0 {
				return errors.New("content after finishReason")
			}
			for _, p := range parts {
				if f := p.Get("functionCall"); f.Exists() {
					name := f.Get("name").String()
					counts[name]++
					want := int64(1)
					if name == "beta" {
						want = 2
					}
					if (name != "alpha" && name != "beta") || f.Get("args.value").Int() != want || f.Get("id").String() != "call_"+name {
						return fmt.Errorf("lost tool identity/arguments: %s", f.Raw)
					}
				}
			}
			if j.Get("candidates.0.finishReason").String() != "" {
				finished = true
				if counts["alpha"] != 1 || counts["beta"] != 1 {
					return errors.New("finish before both tools")
				}
			}
			if j.Get("usageMetadata.totalTokenCount").Exists() {
				usage = j.Get("usageMetadata.promptTokenCount").Int() == 10 && j.Get("usageMetadata.candidatesTokenCount").Int() == 1 && j.Get("usageMetadata.totalTokenCount").Int() == 11
			}
		}
	}
	if counts["alpha"] != 1 || counts["beta"] != 1 || !usage {
		return fmt.Errorf("tools=%v exact usage=%v", counts, usage)
	}
	return nil
}

func validateCancelled(body string) error {
	if strings.Contains(body, "[DONE]") {
		return errors.New("cancelled stream emitted DONE")
	}
	records := 0
	for _, frame := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if !json.Valid([]byte(raw)) {
				return errors.New("cancelled stream contains non-JSON data")
			}
			j := gjson.Parse(raw)
			if j.Get("candidates.0.finishReason").String() != "" || j.Get("type").String() == "response.completed" || j.Get("type").String() == "message_stop" || j.Get("choices.0.finish_reason").String() != "" {
				return errors.New("cancelled stream emitted successful completion")
			}
			records++
		}
	}
	if records == 0 {
		return errors.New("cancellation did not exercise a stream with emitted data")
	}
	return nil
}
