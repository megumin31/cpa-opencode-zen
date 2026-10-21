package plugin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type geminiRecordHost struct {
	mu      sync.Mutex
	chunks  [][]byte
	message string
	done    chan struct{}
	once    sync.Once
}

func (h *geminiRecordHost) Emit(_ string, b []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.chunks = append(h.chunks, bytes.Clone(b))
	return nil
}
func (h *geminiRecordHost) Close(_, message string) error {
	h.mu.Lock()
	h.message = message
	h.mu.Unlock()
	h.once.Do(func() { close(h.done) })
	return nil
}

func TestGeminiExecutorEmitsHostJSONRecords(t *testing.T) {
	for _, source := range []string{"gemini", "openai"} {
		for _, alt := range []string{"", "json"} {
			t.Run(source+"/alt="+alt, func(t *testing.T) {
				host := &geminiRecordHost{done: make(chan struct{})}
				engine := New(Dependencies{Streams: host})
				t.Cleanup(engine.Shutdown)
				var server *httptest.Server
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/api/v2/config") {
						io.WriteString(w, `{"providers":{"p":{"package":"`+protocolPackage(source)+`","settings":{"baseURL":"`+server.URL+`/inference"},"models":{"m":{}}}}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, nativeFixture(source))
				}))
				defer server.Close()
				engine.testConfig(t, server.URL)
				req := rpcExecutorRequest{StreamID: "gemini", ExecutorRequest: pluginapi.ExecutorRequest{Model: "opencode/p/m", Format: "gemini", SourceFormat: "gemini", Alt: alt, Payload: []byte(clientRequest("gemini")), StorageJSON: rawJSON(t, testCredential(server.URL))}}
				if _, err := engine.execute(rawJSON(t, req), true); err != nil {
					t.Fatal(err)
				}
				engineFixtureWait(t, host.done, "Gemini stream close")
				host.mu.Lock()
				defer host.mu.Unlock()
				if host.message != "" || len(host.chunks) == 0 {
					t.Fatalf("chunks=%d error=%s", len(host.chunks), host.message)
				}
				for _, b := range host.chunks {
					if !json.Valid(b) {
						t.Fatalf("executor emitted non-JSON: %q", b)
					}
				}
				all := string(bytes.Join(host.chunks, nil))
				for _, want := range []string{"hello", "weather", "finishReason"} {
					if !strings.Contains(all, want) {
						t.Fatalf("missing %s", want)
					}
				}
			})
		}
	}
}
