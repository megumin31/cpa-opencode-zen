<div align="right">
  <strong>English</strong> | <a href="README_zh.md">简体中文</a>
</div>

# OpenCode Plugin for CLIProxyAPI (CPA)

A high-performance C-shared plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) that integrates OpenCode's LLM ecosystem — including free-tier models and Console workspace models — into standard OpenAI-compatible and Anthropic-compatible endpoints.

---

## Features

- **Public Free Model Discovery**: Public mode exposes OpenCode models explicitly priced at zero, subject to supported protocols and trusted destinations.
- **Automated Gatekeeper Adaptation**:
  - Automatically synthesizes compliant OpenCode EID session identifiers (`ses_<12hex><14base62>`).
  - Transparently manages gateway-required fingerprint tools while isolating and preserving downstream client tool calls.
  - Native streaming adaptation so both streaming and non-streaming downstream clients work reliably.
- **Full Cross-Protocol Compatibility**: Compatible with OpenAI Chat Completions (`/v1/chat/completions`) and OpenAI Responses API (`/v1/responses`), with automatic protocol and reasoning control normalization.
- **Declared Model Capabilities**: Registers upstream-declared reasoning options, token limits, and input/output modalities without inventing missing values. See [capability metadata, API visibility and request limits](docs/MODEL-CAPABILITIES.md).
- **Console Workspace & Service Key**: Connect your OpenCode Console service keys for access to paid/custom models with session continuity and prompt caching.

---

## Available Free Models

The plugin selects free candidates from OpenCode’s public catalog. Call them with the `opencode-zen/<model-id>` alias:

The upstream catalog changes over time; use CPA’s /v1/models for the actual list. One standalone script request using public on 2026-10-17 returned HTTP 403 FreeTierError with an official-client-only message. That script did not use this plugin’s complete request construction, session headers, or transport; it does not establish whether actual plugin requests succeed. Live plugin end-to-end inference remains unverified. Zero catalog pricing does not guarantee anonymous access: upstream may rate-limit, retire a model, or require account authentication.

---

## Installation

### 1. Download Pre-built Binary

Pushing a version tag automatically builds both architectures and publishes a
verified [Release](https://github.com/megumin31/cpa-opencode-zen/releases) with `opencode-<tag>-linux-amd64.tar.gz` and
`opencode-<tag>-linux-arm64.tar.gz`, checksums and a completion manifest after
both native gates pass. Choose your architecture, verify the checksums, and
extract the archive without renaming its `.so`. See [release instructions and
failure/retry behavior](docs/RELEASE.md).

Every commit automatically produces Linux binaries via GitHub Actions for both `x86_64` (`amd64`) and `arm64` (`aarch64`):
1. Go to the [Actions](https://github.com/megumin31/cpa-opencode-zen/actions) tab.
2. Select the latest successful run of **Build Linux plugins**.
3. Download `opencode-linux-amd64-<commit>` or `opencode-linux-arm64-<commit>` under **Artifacts**.
4. Extract `opencode-v0.1.0.so` and place it into your CPA `plugins/` directory:
   ```bash
   cp opencode-v0.1.0.so /path/to/cliproxyapi/plugins/
   chmod 700 /path/to/cliproxyapi/plugins/opencode-v0.1.0.so
   ```

### 2. Configure CPA

In your CPA `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    opencode:
      enabled: true
      request_timeout_seconds: 0
      allowed_destinations:
        - https://opencode.ai/console
        - https://opencode.ai/inference
```

Optional: set `worker_endpoint` to your trusted HTTPS Worker origin and
`worker_token` to its entry token to route Console, catalog and inference through
one Worker. Omit/empty `worker_endpoint` keeps existing direct access. See
[Custom endpoint / 自定义端点](docs/CUSTOM-ENDPOINT.md). No automatic direct fallback.

### 3. Add OpenCode Auth File

Place an auth JSON file in your CPA `auth-dir` (default: `~/.cli-proxy-api/opencode.json`):

```json
{
  "type": "opencode",
  "key": "public",
  "server": "https://opencode.ai/console",
  "label": "OpenCode Service"
}
```

> **Public mode**: Set `"key"` to the exact string `"public"` to skip the authenticated Console config endpoint and discover from the public catalog. Inference still sends public as its authentication value. Input/output prices must both be explicitly zero, with every other listed cost component also zero; missing or unrecognized prices are excluded.
>
> Real account keys continue to use Console discovery. A 401/403 clears that credential’s cached routes and fails, without falling back to public. Credential caches remain isolated. These rules apply in both direct and Worker modes.

### 4. Restart CPA

```bash
systemctl --user restart cliproxyapi
# or restart your CPA binary directly
```

Verify that models are registered:
```bash
curl http://localhost:8317/v1/models -H "Authorization: Bearer <your-cpa-api-key>" | jq '.data[].id'
```

---

## Usage Examples

### Chat Completions (`/v1/chat/completions`)

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <your-cpa-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "opencode-zen/cat-jam-free",
    "messages": [{"role": "user", "content": "Write a quicksort in Go."}],
    "stream": false
  }'
```

### Tool Calling

Client tools are fully passed through and executed by the model without interfering with the internal gateway gatekeeper:

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <your-cpa-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "opencode-zen/space-bunny-free",
    "messages": [{"role": "user", "content": "Calculate 123 * 456 using your calc tool."}],
    "tools": [{
      "type": "function",
      "function": {
        "name": "calc",
        "description": "Calculate math expressions",
        "parameters": {
          "type": "object",
          "properties": {"expr": {"type": "string"}},
          "required": ["expr"]
        }
      }
    }],
    "tool_choice": "auto"
  }'
```

---

## Building from Source

Requires Go **1.26+** (CI pins 1.26.9), a C compiler, Python 3, GNU Make,
Bash, and GNU `timeout`. Native-host checks run on Linux with CGO enabled.

```bash
# Prepare both module caches once while dependency downloads are allowed.
go mod download
go -C cmd/hostcheck mod download

# Complete offline verification, including the real native host against mocks.
make check-offline

# Build only; the C ABI entry point remains the repository root package.
make build
# Development binary: build/opencode-v0.0.0.so (build_tag=dev)

# Print provider, version, and library filename from the shared Go constants.
make metadata
```

`check-offline` runs ordinary tests, vet, and race tests for both Go modules;
Python fixtures; shell/preflight tests; the SDK reproducer; and bounded native
host `--quiesce`, `--cancel-first`, same-instance `--reconfigure`, and idle
`--reenable` checks. It disables module/network toolchain
downloads and uses fake loopback services. It does not establish live-service
access, pricing, or entitlement.

The pinned CPA host's known disable-path failure remains a separate strict
`make hostcheck-known-bug` target. It is deliberately excluded from the passing
gate and is never ignored. See [build and verification details](docs/BUILD.md).

The repository separates the root C ABI shim from the Go plugin engine and its
internal catalog, protocol, diagnostics, and error packages. Developer-only
Python capture tools live in `tools/officialwire`; `cmd/hostcheck` stays an
independent, version-pinned Go module because it exercises CPA's internal host.
See [architecture and dependency boundaries](docs/ARCHITECTURE.md).

---

## License

[AGPL-3.0](LICENSE)
