# Native HTTP stream framing regression

This harness loads the **actual compiled C-shared plugin** into the unmodified
CPA v8.0.29 native plugin host. It registers the parsed fake auth and discovered
models with the real CPA auth manager, then runs the real Chat Completions,
Responses, Claude Messages and Gemini HTTP handlers using Gin and HTTP recorders.
Only the upstream is mocked. Neither the native executor nor HTTP handlers are
reimplemented or replaced. All credentials are fake; the plugin uses an explicit
fail-closed loopback proxy and the host transport blocks non-mock destinations.

From the repository root, after building the plugin:

```sh
make build
go -C cmd/hostcheck run ./httpframing -plugin "$PWD/build/opencode-v0.0.0.so" -matrix
```

Use the actual versioned filename if the build sets a different version.
Add `-scenario truncated` or `-scenario error` to test premature EOF or an
explicit upstream SSE error after initial content. These runs require a visible
error and reject synthesized success terminals. The unsupported pair still must
be rejected explicitly.

`-matrix -scenario blocked` checks only Gemini upstream: native Gemini must
preserve `promptFeedback.blockReason: SAFETY` with no success candidates; the
other three client formats must report an explicit blocked-conversion error.
`-matrix -scenario empty-feedback` checks that empty prompt feedback is not
misclassified as a terminal response and produces a visible failure in all four
client formats, without successful completion.

The `responses-tools`, `responses-tools-error`, `responses-tools-incomplete`
and `responses-tools-cancel` scenarios scope the run to Responses → Gemini.
Two consecutive function calls must preserve distinct names, IDs and arguments
exactly once, followed by text. Completion retains exact usage and occurs only
after content; incomplete completion must use `MAX_TOKENS`. The error case must
not emit a success finish. Cancellation occurs after partial upstream SSE is
flushed, checks that upstream request cancellation propagates within three
seconds, and forbids synthesized completion. Use `-matrix` with these scenarios.

Without `-matrix`, only the four native protocol paths run. With `-matrix`, all
16 upstream/client combinations run: 15 supported streams must pass, and
Claude-to-Gemini must return the existing explicit HTTP 400 unsupported-conversion
error. That rejection is not counted as working translation.

Assertions inspect final HTTP response bytes and require:

- HTTP 200 and `text/event-stream` for supported combinations.
- Strict valid JSON in every SSE data record, excluding Chat's sole `[DONE]`.
- Preserved text, exactly one protocol completion, and correct named event types.
- No double `data:` prefix, duplicate/missing sentinel, error event or truncated frame.
- No Chat data after `[DONE]` or before its finish reason, nor named events after
  Responses completion or Claude stop. Gemini trailing usage remains allowed.
- An actual inference request reaching each mock upstream through the C ABI.

The synthetic Responses fixture includes the complete text event lifecycle, not
just a final snapshot. The Gemini fixture legitimately combines text, finish
reason and usage into one event, followed by EOF, without an OpenAI `[DONE]`.

Parser unit tests require no plugin. An optional Go integration test is available:

```sh
go -C cmd/hostcheck test ./httpframing
HTTPFRAMING_PLUGIN="$PWD/build/opencode-v0.0.0.so" \
  go -C cmd/hostcheck test ./httpframing -run TestNativeHTTPMatrix -count=1
```

This tests HTTP handlers and native bridging, not full CPA service startup,
authentication middleware, a live provider, or deployment. The executable has a
60-second watchdog and the mock inference requests have a 5-second timeout.
