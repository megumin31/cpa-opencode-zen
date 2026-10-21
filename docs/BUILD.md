# Build and verification

## Toolchain and dependency preparation

The main module and `cmd/hostcheck` require Go 1.26 or later; CI pins Go 1.26.9.
Native plugin builds require CGO, a C compiler, Linux, and a matching native host
architecture. The complete gate also needs Python 3, Bash, GNU Make and GNU
`timeout`. CI uses native Linux amd64 and arm64 runners, not cross-compiled
executables run on the wrong architecture.

Populate **both** module caches before working offline:

```sh
go mod download
go -C cmd/hostcheck mod download
go mod verify
go -C cmd/hostcheck mod verify
```

These commands are dependency preparation, not part of the offline gate.
`make check-offline` sets `GOPROXY=off`, `GOSUMDB=off`, `GOTOOLCHAIN=local`,
`CGO_ENABLED=1`, and `-mod=readonly`. A missing compiler or cached module is a
failure rather than permission to fetch it silently. Tests and harnesses use
synthetic local services and fake keys; these settings are not a kernel-level
network sandbox.

## One complete gate

Run from the repository root:

```sh
make check-offline
```

The entry point is `scripts/check-offline.sh`, shared by local Make and CI. It
runs these stages serially and stops at the first failed command:

1. Main-module ordinary tests (`go test ./... -count=1`).
2. Nested host-module ordinary tests (`go -C cmd/hostcheck test ./... -count=1`).
3. Python fixture tests under `tools/officialwire`.
4. Syntax checks for every project shell script and directory-preflight fixtures.
5. Vet for both Go modules.
6. Race tests for both Go modules.
7. `go run ./cmd/sdkrepro`, which explicitly asserts the known pinned SDK behavior.
8. A fresh root-package C-shared build and native host harness build.
9. Real native-host `--quiesce`, `--cancel-first`, `--reconfigure`, and `--reenable` smoke tests against loopback.
10. Real native plugin plus SDK HTTP-handler framing/completion matrix (`httpframing -matrix`), covering success, truncation, upstream error, blocked/empty Gemini feedback and Responses tool sequences (success/error/incomplete/cancellation), including the explicitly unsupported Claude-to-Gemini rejection.
11. Read-only preflight of the generated plugin directory.

`go test ./...` does not traverse nested modules, execute `main` commands, run
Python fixtures, or load a native plugin. Those stages must remain explicit.
Each Go test command has a five-minute timeout; the native harness retains its
own 30-second watchdog and an outer 45-second timeout with a five-second forced
termination grace. Compilation happens before the native execution timeout.
`TEST_TIMEOUT` and `HOSTCHECK_TIMEOUT` are Make overrides for slower test hosts.

The `--reconfigure` mode changes the same loaded library's config while an actual
host callback is blocked by downstream backpressure. It requires ApplyConfig to
return, the old upstream to end, and the old stream to report reconfiguration
without first canceling the caller or draining its output. Only afterward does
it drain the terminal notification and verify fresh discovery plus aggregate and
streamed requests. The library file, identity and version stay unchanged.

The `--reenable` mode first finishes all requests, disables the idle plugin, then
re-enables the exact same library file. It verifies a second native init, correct
registration/auth/discovery, fresh aggregate/stream requests, and clean shutdown
while the caller context remains alive. This covers Go shared-object state that
may survive `dlclose`/`dlopen` because of ELF `NODELETE`. It does not trigger or
hide the separate active-request disable defect.

The public-catalog snapshot diagnostic is optional. To exercise an existing,
trusted local snapshot as well, set `OPENCODE_TEST_PUBLIC_CATALOG` to its absolute
path. The default gate does not fetch a catalog. Official-client capture requires
a separately audited binary and is opt-in; see
[`tools/officialwire`](../tools/officialwire/README.md).

Individual targets are available for focused work:

```sh
make test       # Both modules, Python fixtures, shell/preflight fixtures
make race       # Both modules
make vet        # Both modules
make sdkrepro   # Execute the pinned SDK reproducer
make hostcheck  # Fresh build plus all four passing native-host lifecycle modes
```

## Build metadata and ABI

`internal/buildinfo` is the single Go source for `ProviderID` and `Version`.
`cmd/buildmeta` exposes them to Make and CI without regular-expression parsing
of source files:

```sh
go run ./cmd/buildmeta
go run ./cmd/buildmeta --field provider
go run ./cmd/buildmeta --field version
go run ./cmd/buildmeta --field filename
```

The JSON fields are `provider`, `version`, and `filename`. The library name is
computed from these constants. `make build` also writes
`build/plugin-metadata.json`; the independent native harness reads it from the
supplied build directory and asserts registration against the same identity and
version. For an externally built library, set `OPENCODE_PLUGIN_METADATA` to the
JSON output of `cmd/buildmeta` instead. No production constants are copied into
the nested module.

`make build` still builds the root package `.`
with `-buildmode=c-shared`; moving implementation into `internal/` does not move
the C ABI entry point or change the four exported host symbols.

For a read-only migration check, use:

```sh
make check-plugin-dir PLUGIN_DIR=/path/to/plugins
```

The shell helper can also run without Go installed if its intended release
filename is supplied explicitly:

```sh
bash scripts/check-plugin-directory.sh /path/to/plugins opencode-v0.1.0.so
```

Without that second argument it asks `cmd/buildmeta` for the filename. It scans
nested platform directories, rejects conflicting release/legacy names and
symlink libraries, and does not alter host configuration or files.

## Known host regression stays visible

```sh
make hostcheck-known-bug
```

This reproduces the pinned CPA v8.0.29 per-plugin disable failure with an active,
blocked request. The target must return a failure when the unsafe behavior is
observed; it is not an expected-failure assertion that turns the defect green.
Do not add it to the passing gate or suppress its exit status. See
[the lifecycle proposal](HOST-LIFECYCLE-PROPOSAL.md).

The manual GitHub Actions input `check_known_host_bug` runs the same strict
reproducer after the verified artifact upload. Selecting it is expected to make
the job fail while retaining the separate diagnostic log.

## CI artifact provenance

`.github/workflows/build-plugin.yml` keeps Linux amd64 and arm64 native jobs.
Publishing a Release reuses this same matrix and attaches architecture-specific
archives only after both jobs succeed; see [release automation](RELEASE.md).
`scripts/ci-build.sh` prepares and verifies both dependency caches, runs the
complete offline gate, and verifies:

- 64-bit little-endian ELF shared-object type and expected machine architecture
- The four C ABI exports: `cliproxy_plugin_init`, `cliproxyPluginCall`,
  `cliproxyPluginFree`, and `cliproxyPluginShutdown`
- The exact checked-out commit embedded by Go, with `vcs.modified=false`
- Unchanged module manifests for both modules
- SHA256 checksums, ELF/glibc symbol requirements, toolchain and runner metadata

Artifacts retain `LICENSE`, `SHA256SUMS`, `plugin-metadata.json`, `build-info.txt`,
`elf-report.txt`, and `build-metadata.json` beside the shared library. The small
plugin metadata file also lets the standalone native harness inspect an extracted
artifact directory without duplicating release values. Diagnostics are retained even
when a check fails. Do not use `-buildvcs=false` for a release/provenance build.
A detached source archive may need that option for local experiments; such a
build cannot satisfy CI's embedded-clean-revision requirement.

The same verifier can run locally after committing all source changes, without
creating a GitHub Actions run or inventing run identifiers:

```sh
GITHUB_ACTIONS=false \
GITHUB_SHA="$(git rev-parse HEAD)" \
TARGET_ARCH="$(go env GOHOSTARCH)" \
RUNNER_LABEL="local-linux-$(go env GOHOSTARCH)" \
CGO_ENABLED=1 GOFLAGS='-mod=readonly -trimpath -buildvcs=true' \
bash scripts/ci-build.sh
```

`GITHUB_SHA` must be the actual checked-out commit, `TARGET_ARCH` must match the
native Linux host, and `RUNNER_LABEL` must explicitly describe the build host.
The clean embedded revision, ELF, export, module and test requirements are the
same locally and in CI. With prepared caches, set `GOPROXY=off GOSUMDB=off
GOTOOLCHAIN=local` to keep dependency preparation offline as well. Use `GO` to
select a compiler other than `go` on `PATH`.

Only `GITHUB_ACTIONS=true` enables GitHub run provenance; it requires the actual
GitHub-provided repository, server URL, run ID and attempt values. Otherwise
`run_url` and `run_attempt` are JSON `null`; `repository` is also `null` unless
`GITHUB_REPOSITORY` is explicitly supplied. `GITHUB_STEP_SUMMARY` is optional and
is written only when a nonempty path is provided. Local artifacts remain under
`build/ci-artifact` and logs under `build/ci-logs`; the script does not upload them.

The gate validates local behavior only. Existing live-service limitations and
historical records under `docs/` remain separate evidence.
