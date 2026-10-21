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
// CPA v8.0.29's Gemini handler owns SSE framing (GetAlt normalizes alt=sse
// to empty); its native executor supplies JSON records, also for non-SSE alt.
// Other handlers consume complete wire frames and must remain unchanged.
func ForwardToHost(ctx context.Context, r io.Reader, route catalog.Route, output translator.Format, original, request []byte, emit func([]byte) error) error {
	if output != translator.FormatGemini {
		return Forward(ctx, r, route, output, original, request, emit)
	}
	return Forward(ctx, r, route, output, original, request, func(frame []byte) error {
		return readSSE(ctx, bytes.NewReader(frame), func(event sseEvent) error {
			var compact bytes.Buffer
			if err := json.Compact(&compact, event.Data); err != nil {
				return fault.New(502, "invalid Gemini stream payload")
			}
			return emit(compact.Bytes())
		})
	})
}
