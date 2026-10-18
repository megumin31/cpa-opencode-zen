# Model capability metadata and request controls

The source is the live [OpenCode catalog](https://models.opencode.ai/api.json), or
the configured trusted `catalog_file`. Metadata describes upstream declarations;
it is not proof of account entitlement or successful inference. No model-family
or model-name heuristics supply missing declarations.

## Catalog → Console override → CPA

The resolver retains:

- `reasoning` and the full `reasoning_options` list, including unknown future
  option types, in the normalized route's capabilities
- `limit.context`, `limit.input`, and `limit.output`
- `modalities.input` / `modalities.output`, normalized to capabilities
  `input` / `output`

Console model metadata overrides the inherited catalog model. `limit` is merged
by key. `reasoning_options` is replaced as a complete list; an explicit `[]`
clears inherited options. Public-schema `reasoning` / `reasoning_options` fields
are also accepted in the plugin's Console JSON input; fields in the normalized
`capabilities` object take precedence over those top-level fields. An explicit
`reasoning:false` suppresses exported thinking controls, even when inherited
options remain in the raw metadata. Missing/null option lists do not erase an
inherited list. Explicit variants inherit metadata but remain explicit: reasoning
options never generate additional model aliases or variant routes.

CPA SDK v8.0.29 has real fields for all four token limits and input/output
modalities, plus `ThinkingSupport`:

| Declaration | SDK field |
| --- | --- |
| `limit.context` | `ContextLength` |
| `limit.input` | `InputTokenLimit` |
| `limit.output` | `OutputTokenLimit`, `MaxCompletionTokens` |
| input/output modalities | `SupportedInputModalities`, `SupportedOutputModalities` |
| effort option values | `Thinking.Levels`, preserving declared order and values |
| toggle option / explicit effort `none` | `Thinking.ZeroAllowed` |
| budget_tokens min/max | `Thinking.Min` / `Thinking.Max`, only if explicitly supplied |

Token limits already had a mapping before this change; this change adds validation
against negative, fractional, overflowing or nonnumeric counts and regression
coverage. Missing limits remain zero/unset. Context does not imply an input limit;
output does not imply a reasoning-budget maximum. The new fields are reasoning
options and modality metadata that previously did not reach CPA correctly.

A boolean `reasoning:true` alone produces an empty Thinking declaration. It does
not invent effort levels, disable support, automatic budget selection or bounds.
`DynamicAllowed` is not inferred from reasoning support. Unknown option types are
retained internally but do not fabricate SDK controls. The SDK cannot distinguish
an absent numeric bound from zero or separately express a budget option with no
bounds. Complete option declarations remain in the resolved catalog; no imaginary
SDK fields or misleading effort substitutes are added.

## What clients can actually see

The deployed CPA v8.0.30 host and pinned v8.0.29 SDK preserve these fields in the
model registry. Their model-list serializers expose different projections:

- OpenAI `/v1/models`: `context_length`, `max_completion_tokens` and
  `supported_parameters` when populated; no `thinking`, detailed input/output
  token limits or modality lists
- Gemini model list: `inputTokenLimit`, `outputTokenLimit`,
  `supportedInputModalities`, `supportedOutputModalities`
- Neither list serializes detailed reasoning levels

Consequently, a client using only OpenAI model discovery cannot discover all
registered capability fields from this plugin alone. This patch does not modify
CPA or promise that a client will automatically show a reasoning selector.
`cmd/hostcheck/model_metadata_test.go` checks the real host registry and list
projection, including these omissions. The v8.0.30 registry and plugin adapter
sources are byte-identical to v8.0.29 for this behavior.

References: [SDK schema](https://github.com/router-for-me/CLIProxyAPI/blob/v8.0.29/sdk/pluginapi/types.go#L147-L201),
[deployed-host serializer](https://github.com/router-for-me/CLIProxyAPI/blob/v8.0.30/internal/registry/model_registry.go#L1719-L1836).

## Reasoning controls on the wire

Use the actual request parameter for the selected upstream protocol:

- Chat Completions: `reasoning_effort`
- Responses: `reasoning.effort`
- Anthropic: `thinking.type`, `thinking.budget_tokens`, and `output_config.effort`
- Gemini: `generationConfig.thinkingConfig.thinkingBudget` / `thinkingLevel`

The plugin's existing native settings use `reasoningEffort` for Chat/Responses,
`thinking` / `effort` for Anthropic, and `thinkingConfig` for Gemini. Configured
provider/model/variant Settings override caller parameters; the final configured
Body overlay wins over both. Existing native budget normalization may reduce a
budget against the request's output maximum. That behavior is not a catalog
budget declaration.

Offline tests verify every declared anonymous-model effort (`low`, `medium`,
`high`, `xhigh`, `max`) reaches the final Chat/Responses payload, including both
cross-conversion directions. Native toggle and numeric budget tests verify the
actual fields. Numeric budgets are never approximated as effort tiers; lossy
cross-protocol controls fail with 400 rather than silently changing the request.

A toggle declaration does not specify a universal Chat wire parameter. Some
providers use provider-specific fields. This change does not guess a toggle
recipe from a model's name or turn it into `reasoning_effort:none` automatically.
Use the provider's documented native body or configured override when supported.
Likewise, arbitrary model suffixes such as `(max)` are not applied by this plugin:
model lookup remains exact, and this executor has no generic ThinkingApplier.
Select effort in the request body, or use an explicitly configured variant.

## Modalities and the adapter boundary

SDK modality lists carry the catalog's complete model declarations, including
image/PDF/audio/video if declared. They are not an all-protocol transport matrix.
Native-format requests retain their native content representation. Cross-protocol
requests must pass the existing lossless-content checks; unsupported attachment
representations are rejected.

Existing offline coverage verifies inline image Chat→Claude/Gemini/Responses,
and rejection of remote image Chat→Gemini, audio Chat→Claude, and cross-protocol
`video_url`. This metadata patch implements no new audio/video endpoint,
encoder, output stream or multimodal translator, and does not claim successful
live multimodal inference. Output modality declarations similarly do not prove
that every downstream format can represent that output.

## Freshness and safety

Route refresh remains one minute; catalog refresh remains demand-driven every
five minutes. A failed catalog refresh keeps the last validated metadata and
retries after the existing backoff. Tests cover updated effort/limits, malformed
metadata retention and recovery. Public zero-price filtering, deprecated/non-text
filtering, allowed packages, trusted destinations and credential isolation are
unchanged. No cooldown or Beam-response repair is included in this patch.
