package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/megumin31/cpa-opencode-zen/internal/catalog"
	"github.com/megumin31/cpa-opencode-zen/internal/fault"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// ForwardToHost adapts protocol wire frames to CPA's executor chunk contract.
// CPA v8.0.29 Chat and Gemini handlers own SSE framing and consume JSON
// records. Chat also owns the final [DONE] marker. Responses and Claude
// handlers consume full wire frames, including their protocol terminal events.
func ForwardToHost(ctx context.Context, r io.Reader, route catalog.Route, output translator.Format, original, request []byte, emit func([]byte) error) error {
	if output != translator.FormatGemini && output != translator.FormatOpenAI {
		return Forward(ctx, r, route, output, original, request, emit)
	}
	return Forward(ctx, r, route, output, original, request, func(frame []byte) error {
		return readSSE(ctx, bytes.NewReader(frame), func(event sseEvent) error {
			// Forward has already validated upstream completion before returning.
			// Do not give the host a sentinel that it would wrap as a second DONE.
			if output == translator.FormatOpenAI && bytes.Equal(bytes.TrimSpace(event.Data), []byte("[DONE]")) {
				return nil
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, event.Data); err != nil {
				return fault.New(502, "invalid host JSON stream payload")
			}
			return emit(compact.Bytes())
		})
	})
}
