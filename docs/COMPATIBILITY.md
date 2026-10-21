# Compatibility and verification boundaries

## Evidence and pinned versions

Official OpenCode CLI 2.0.29, build `3a69bae8459b1aa9b122456a55d9535b23125f33`, was statically inspected. npm package SHA-256: `14b5a6638a1a093ac9d484599c844b10c29b445d04844b682ccc668ce26530a2`. CLIProxyAPI v8.0.29 / `5fc580b1d72d87a7bcfe5cfb6f479c1aea5625b3` is pinned in both modules.

- https://opencode.ai/v2/install
- https://registry.npmjs.org/@opencode/cli-linux-x64/-/cli-linux-x64-2.0.29.tgz
- https://github.com/router-for-me/CLIProxyAPI/tree/5fc580b1d72d87a7bcfe5cfb6f479c1aea5625b3

## Implemented behavior

- API-key files only. Config discovery `/console/api/v2/config` uses Bearer and optional explicit organization. No OAuth endpoints or credential rotation.
- Credential-isolated one-minute discovery cache, context-aware serialized refresh, bounded 30-second discovery, 404 means no Console overrides while preserving the connected opencode baseline, invalid-key cache invalidation, same-key stale data only for transient discovery failure. Failed public catalog attempts are retried on later refresh.
- Canonical provider/model inheritance; modelID lookup/fallback, package/baseURL override rules, credential stripping, recursive body merge, header merging, explicit variants, limits/capabilities, unique aliases. Unknown package fails closed.
- Native service-key authentication: OpenAI Bearer; Anthropic x-api-key; Google x-goog-api-key. Outgoing `User-Agent` is fixed to `opencode/latest/2.0.29/cli` (not an official client attestation). Protected auth, organization, session, host/cookie/transport/user-agent overrides rejected.
- Native Chat, Responses, Anthropic Messages, and Gemini requests stream upstream for both streamed and aggregate clients. Fragmented UTF-8/CRLF/multiline SSE, tools, reasoning fields, signed history, reported usage, and terminal/error handling have mock fixtures.
- Audited native provider-option mapping, GPT-5 defaults, Anthropic thinking budget/cache limit/empty-text/signature behavior and dynamic beta headers, Gemini thinking budget/options. Agent generation policy is separate and is not injected.
- Configured body overlay is last, followed only by safety validation and derived headers. Unsupported settings/compatibility values fail before inference.
- Joined stream workers and close-once stream tracking. Pre-quiesce closes active host streams before joining; actual host hot reload with blocked downstream is verified. Direct host ShutdownAll has the separate pinned-host limitation described below.
- x-opencode-session and affinity headers always present, using plugin-owned scoped IDs as described in README.
- Preserve HTTP error statuses; no inference replay. Local cooldown scoped to the credential and final upstream model: fixed three minutes for 429, upstream Retry-After for 503. No fake token count or fabricated usage; unsupported token counting returns 501.
- Optional isolated-test guard matches exact final model/endpoint pairs after body overlays and before inference POST. It is disabled when omitted and does not alter normal protocol payloads. Optional `strict_text_only: true` additionally restricts final native Chat Completions JSON to bounded plain-text messages and a single 1–32 output-token limit, plus only native JSON/wildcard-Accept/auth/organization/session headers. Unknown fields, multimodal content, tools and other extensions fail before POST. It validates without rewriting; all other native upstream protocols are rejected by this profile. Neither mode proves current price, entitlement, or account billing.

## Protocol conversion boundaries

Mock text/tool/usage tests exercise native and available cross-protocol pairs. This does not certify every experimental option, multimodal schema, or provider-specific extension as lossless.

- Native same-protocol requests bypass SDK conversion, including Responses (no Codex defaults).
- CPA registers provider-side Responses under `codex`. Cross-protocol use restores explicit sampling/token limits/service tier, preserves explicit sequential tools, removes unsolicited Codex parallel/reasoning defaults, and rejects unsupported stop/top-k requests. Bare aggregated Responses results are wrapped in the terminal event shape expected by SDK response converters.
- Claude upstream to Gemini client is explicitly rejected before inference: the pinned converter's response contract and signed-history fidelity are not sufficiently verified. Other Gemini paths are not globally disabled.
- OpenAI Chat upstream to Gemini client has a pinned SDK bug when tool delta and finish_reason share one valid SSE frame. `go run ./cmd/sdkrepro` reproduces it using SDK alone. The plugin privately splits that record into equivalent tool and finish records before conversion. This is a local workaround, not an upstream patch.
- Required provider signatures are never synthesized. In particular `skip_thought_signature_validator` placeholders are rejected. Native Anthropic would sometimes downgrade unsigned thinking to text; this plugin rejects required unsigned history to avoid silently changing it.

## Explicit differences and limitations

1. Stock CPA C ABI cannot deliver a cancelable context into a blocked C RPC. Stream cancellation works after handoff. `request_timeout_seconds` defaults to 0 (no total timeout); an explicit 1–3600 value bounds the entire execution, including pre-header and synchronous aggregation. Separate native defaults are a five-minute response-header timeout and a fifteen-minute incoming-body-idle timeout; provider settings can override or disable them. Shutdown cancels root contexts. Immediate client-disconnect cancellation before handoff is not guaranteed.
2. Host HTTP bridge has no no-redirect option and can leak nonstandard auth headers across redirects. Own no-redirect transport is therefore required; CPA raw-upstream HTTP logging callbacks are unavailable. Explicit host/plugin/auth proxy settings remain honored.
3. Public model catalog initially returned HTTP 403; an anonymous retry on 2026-10-15 later succeeded. Fully specified Console routes work without it; catalog-inherited routes require a trusted `catalog_file` when fetching remains unavailable. No alternate access route or fabricated fallback catalog was used. Cost/cache pricing tiers are not exported by the host ModelInfo schema; this plugin does not infer whether a model is free.
4. No catalog-generated reasoning variants are synthesized. Explicit provider/model variants are supported. Unimplemented settings and materially unsupported compatibility knobs fail closed.
5. Official sessions use parent session, fork session, or current session as affinity; the current-session header is separate. CPA does not reliably supply that hierarchy. This plugin uses honest caller/credential-scoped conversation IDs, or an explicitly request-scoped fallback. It does not fabricate `x-opencode-session-id`, parent, project or official client identity.
6. This is a provider adapter, not the OpenCode agent. No official system prompts, local tools, repository instructions, session orchestration, model selection, compaction, or client attestations are injected. No promise is made about upstream risk classification or acceptance.
7. Error RPC envelopes cannot carry Retry-After headers; local cooldown applies the fixed 429 policy or preserves the 503 restriction but clients do not receive that header through this ABI. Cooldowns survive in-process configuration reloads but are memory-local and reset on process restart. See [cooldown scope and safe upgrade](COOLDOWNS.md).
8. Successful real-service compatibility, current workspace models, free/billing eligibility, live rate limits, SSO, and third-party access acceptance remain unverified. The checked-in regression tests use fake keys and local mock HTTP only. Subsequent real-model testing exposed compatibility issues that remain under diagnosis; mock coverage does not establish upstream acceptance.

## Verified pinned host shutdown blocker

The real native host integration reproduces a lifecycle bug in stock CPA v8.0.29. When a client stops consuming stream output, host.stream.emit blocks after the host's 16-item bridge queue fills. ShutdownAll disables plugin callbacks before calling the C shutdown callback. The plugin then cannot call host.stream.close to release its blocked emit. Waiting for workers is necessary to avoid callbacks into freed C host memory, so shutdown waits until the request is canceled/drained.

This is a blocker for unconditional direct SDK unload with a stalled, uncanceled active stream. Cancel/drain active requests before direct unload. Normal CPA Service.Shutdown first closes its HTTP server, canceling ordinary request contexts before plugin unload, so this test does not prove normal application shutdown fails. The actual live config-disable path is also reproduced by `--disable`: ApplyConfig with enabled=false blocks until the uncanceled request is canceled/drained. Real version-replacement pre-quiesce runs while callbacks remain valid and is verified to complete under the same backpressure. Do not remove the worker join as a workaround.

Reproduce with the real official host, fake HTTP only:

```sh
go -C cmd/hostcheck run . /absolute/path/to/build          # fails on this known pinned-host bug
go -C cmd/hostcheck run . --quiesce /absolute/path/to/build # passes hot-reload quiesce
go -C cmd/hostcheck run . --cancel-first /absolute/path/to/build # passes service-like shutdown
go -C cmd/hostcheck run . --disable /absolute/path/to/build # reproduces live disable hazard
```

Both modes first verify actual native loading/auth/discovery, aggregate and streaming executor calls, and terminal SSE with a connection held open. The minimal host remedy is to pre-quiesce while callbacks are valid, or track and abort plugin-owned stream bridges before closing callback instances. No upstream core files are changed by this project.

### Mock response compatibility matrix

Rows are native upstream responses; columns are client response formats. Both stream and aggregate fixtures check text, tools, and reported usage. Input conversion is separately exercised by the same pair executor tests. This is fixture-level coverage, not a promise for arbitrary advanced fields.

| Upstream / client | Chat | Responses | Claude | Gemini |
| --- | --- | --- | --- | --- |
| Chat | Tested | Tested | Tested | Tested with local same-frame workaround |
| Responses | Tested with SDK wrapper | Tested native | Tested with SDK wrapper | Tested with SDK wrapper |
| Claude | Tested | Tested | Tested native | Explicitly rejected |
| Gemini | Tested | Tested | Tested | Tested native |

## Discovery alignment correction (2026-10-15)

The public catalog legitimately contains duplicate effective model IDs when an explicit model and a generated experimental mode share an alias. The official CLI enumerates JSON objects in JavaScript order, filters deprecated/non-text/superseded records, then constructs a last-write-wins model Map. The resolver now follows that order rather than rejecting the entire catalog.

A manually connected opencode API key also enables the base opencode catalog without Console overrides. Config 404 has the same no-overrides meaning. Existing provider IDs retain their catalog models; new canonical alias providers start with only explicit models. opencode-go and unrelated catalog integrations are not implicitly connected. Exact endpoints still require the credential allowlist, and unsupported packages remain hidden.

An anonymously downloaded public snapshot (SHA256 `d89d121bb5511e41bd5e9ddf00873aab132849fd96df549f0d39f6e80d64a4d3`, 5,664,582 bytes) reproduced the old duplicate-ID failure. With the correction, offline parsing yields 228 retained providers and 84 opencode baseline candidates, of which 83 use supported packages and trusted destinations. These are snapshot counts, not account entitlement, free-model guarantees, or live-authentication results.

Catalog parse errors now identify the catalog stage separately from Console config parse failures. A missing catalog plus no configured routes reports unavailable catalog instead of silently suggesting that an account has no models.

Remaining discovery boundary: Console canonical references are still resolved from the catalog snapshot. Chained aliases referring to earlier Console-defined providers, same-provider sequential model remaps, and an existing-model fallback when a different canonical provider lacks that model are not yet fully aligned with the native mutable registry. These pre-existing advanced-override cases are outside this narrow collision/baseline correction; do not treat this release as full arbitrary-configuration parity.

Discovery emits a status-only diagnostic such as `stage=console_config http_status=200`. It contains no credential, header, body, organization, or model data. This records the exact Console HTTP stage without asserting that a 2xx proves entitlements or successful inference.

For zero-model investigations, discovery also emits aggregate counts at `stage=resolved_routes` (candidates, unsupported, blocked_destinations, exposed) and `stage=models_for_auth` (returned). These distinguish resolver/security filtering from subsequent CPA registration and client-side filtering without logging private route names, package strings, endpoint values, or response content. A positive returned count is the plugin's output to the host; it is not proof the host published those models.

`cmd/hostcheck/servicecheck/run.sh` additionally tests the unmodified CPA Service Builder, default auth file store, real watcher, and HTTP `/v1/models`. With the public snapshot and fake Console responses (empty providers or explicit opencode package), direct discovery and the actual endpoint both expose the same 83 model IDs. Disabling and re-enabling the generated fake auth file removes and restores all 83. Runtime network is confined to loopback; the unrelated unconditional CPA version-check CONNECT is rejected locally without forwarding. These cases did not reproduce the reported zero-model real-configuration result, which remains under diagnosis.

### Idle discovery boundary

The one-minute discovery interval is a cache expiry, not a guarantee that an idle
CPA instance polls Console every minute. In pinned CPA v8.0.29, the auto-refresh
scheduler excludes API-key auth when due; this plugin refreshes on discovery,
execution, or explicit reload. An idle Console configuration change may therefore
not update registered models until such an action occurs. This is deliberately
not worked around by labeling API keys as OAuth credentials.

### Executor streaming boundaries

CPA v8.0.29 handlers do not share one executor chunk format:

- Chat Completions consumes compact JSON records and adds its own SSE `data:`
  envelope. Its handler emits the final `[DONE]`; the plugin consumes the
  upstream sentinel after validating completion, rather than forwarding it.
- Gemini also consumes compact JSON records. `alt=sse` and `$alt=sse` are
  normalized to default mode, which adds SSE framing. Non-SSE alternate modes
  receive the same JSON records directly, matching CPA's native executor.
- Responses and Claude consume complete SSE frames, including their named
  events and protocol-specific terminal events. Those frames remain unchanged.

The plugin keeps wire parsing/conversion and upstream completion validation
separate from this final host-boundary adaptation. A valid upstream stream must
not be judged solely by HTTP 200 or by finding text inside a raw chunk. The
`httpframing` integration harness exercises the real native plugin and pinned
SDK HTTP handlers, then strictly parses their final SSE output.

Two pinned-SDK converter terminal gaps are adapted after validating upstream
completion: Gemini-to-Claude receives its required internal flush to emit
`message_stop`; Responses-to-Gemini gains `finishReason: STOP` on successful
text completion. Responses tool records never finish the whole response:
intermediate SDK finish flags are removed, and exactly one final reason is
emitted at response.completed or response.incomplete, preserving mapped
incomplete reasons and usage. Each SDK pending-tool record is drained immediately
through its pinned empty-reasoning path and checked before removing the synthetic
record, avoiding single-slot overwrites of consecutive tool calls without
reimplementing SDK tool-name/id mapping.

Gemini promptFeedback block reasons are preserved for native Gemini output.
Cross-protocol conversions fail closed with an explicit error because the pinned
converters cannot safely preserve that refusal. Empty/unspecified feedback is
not proof of completion. Errors, truncation and cancellation do not gain success
markers.
