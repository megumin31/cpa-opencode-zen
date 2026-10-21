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
	scenario := flag.String("scenario", "success", "upstream scenario: success, truncated, error")
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
	if scenario != "success" && scenario != "truncated" && scenario != "error" {
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
		for _, client := range protocols {
			if !matrix && upstream != client {
				continue
			}
			model := "opencode/" + upstream + "/space-bunny-free"
			path, body := clientRequest(client, model)
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			label := upstream + " -> " + client
			if upstream == "anthropic" && client == "gemini" {
				if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "Claude-to-Gemini response conversion is not supported safely") {
					results = append(results, fmt.Errorf("%s: expected explicit unsupported conversion rejection, got %d %s", label, recorder.Code, recorder.Body.String()))
				} else {
					fmt.Println("PASS:", label, "explicit unsupported conversion rejection")
				}
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
