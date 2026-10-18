# Architecture and dependency boundaries

## Package layout

- `abi.go` is the root `package main` and the sole C-facing boundary. It owns C
  allocation, host callback ABI adaptation, exported symbols and RPC envelopes.
  Build it with `go build -buildmode=c-shared .`.
- `internal/plugin` owns the Go engine, lifecycle and request orchestration,
  authentication/discovery, transport, sessions, cooldown and stream workers.
- `internal/catalog` owns catalog/config parsing and route resolution.
- `internal/protocol` owns request/response shaping, native protocol semantics,
  aggregation and SDK compatibility adaptations.
- `internal/transport` owns validated destination/proxy policy and per-generation
  client pooling, without owning the plugin lifecycle.
- `internal/diagnostics` owns bounded upstream error parsing and redaction.
- `internal/fault` supplies the shared status-bearing error contract.
- `internal/buildinfo` supplies provider identity and release version without
  importing the engine or CPA runtime.
- `cmd/buildmeta` is a small metadata CLI for build tooling; `cmd/sdkrepro`
  explicitly reproduces a pinned public SDK behavior.
- `cmd/hostcheck` remains its own module. Its namespace permits testing the
  pinned CPA native host internals without changing the production dependency
  graph or vendoring/patching CPA.
- `tools/officialwire` contains opt-in Python official-binary capture tools and
  their offline fixture tests. It is not part of the plugin binary.

The intended dependency direction is ABI adapter → plugin engine → focused
internal helpers. Helpers do not call back into the engine, and C types and C
allocation stay out of the helper packages. Shared value/error/metadata types
belong at a lower layer rather than creating package cycles.

## Runtime and test ownership

The engine owns mutable runtime state. Host callbacks are supplied through a
small Go interface at construction, keeping tests independent of C storage.
Request/stream work belongs to its admitted runtime generation so shutdown or
reconfiguration can cancel and join that work before replacing its resources.
Tests for pure behavior live beside their owning helper package; lifecycle and
cross-component behavior stay with the engine. The root retains ABI-specific
coverage rather than being the implicit home of every test.

The native harness complements Go unit tests. It loads the actual built shared
library through the pinned host, exercises registration, fake authentication,
discovery, aggregate/stream results and error propagation, and checks bounded
quiescence/cancellation and same-instance reconfiguration under host callback
backpressure. Reconfiguration must retire the old stream before fresh requests
are verified, without preemptively canceling the caller. Pure Go test success
alone is not evidence that the C ABI or native loader works.

## Stable boundaries and limits

Provider identity, configured routing semantics, plugin version and C ABI
exports are unchanged by the package layout. Release metadata comes from
`internal/buildinfo` through `cmd/buildmeta`, not a build script's interpretation
of a source file.

This split does not fix the pinned host's known disable/unload behavior or prove
live upstream compatibility. The strict known-host regression and existing
protocol limitations remain explicit. See [build verification](BUILD.md),
[compatibility](COMPATIBILITY.md), and the
[host lifecycle proposal](HOST-LIFECYCLE-PROPOSAL.md).

## Configuration and cancellation contract

`Engine.New` fixes the clock, random source, logger, HTTP factory, and stream host
for an instance. Configuration YAML is parsed and validated before touching the
live generation. A successful reconfigure stops admission, cancels the old
root/requests, closes host stream bridges to release independent callback
backpressure, joins workers and their cancellation callbacks, and closes that
generation's HTTP pool before publishing the new immutable configuration.
Requests capture that generation once; they never reload configuration halfway
through credential validation, route preparation, guard checks, or sending.

Request deadlines and `request.complete` also close their own stream bridge;
HTTP cancellation alone does not release an independently blocked host callback.
The stream host must allow `Close` to release a concurrent `Emit`. No engine/map
lock is held across those host callbacks. Admission is stopped before waiting,
so new worker increments cannot race a zero-count shutdown wait.

Route discovery, host-proxy association, request cancellation, HTTP connections,
and active stream IDs belong to a configuration generation. Session affinity
and credential-and-upstream-model Retry-After gates belong to the Engine lifetime, so a
configuration update neither resets a live conversation nor erases an upstream
cooldown. Session retention is bounded by a 10,000-entry LRU; eviction can change
an evicted scope's later ID. Terminal shutdown clears these stores. Quiescence
allows a later valid configure for host rollback; terminal Engine shutdown does
not.

The ABI creates a fresh Engine for each successful initialization after shutdown.
It rejects a second initialization while a host binding remains active, joins
all callbacks before clearing host pointers, and serializes init/shutdown. This
is necessary even after `dlclose`: Go ELF shared libraries can remain mapped
with `NODELETE`, so Go package initialization is not a plugin-load lifecycle.

Request completion uses CPA's external request ID. The pinned host creates a
fresh UUID per request and completes it through `sync.Once`; arbitrary reuse of
an ID across outstanding requests is outside that contract. Stream-close
ownership is additionally generation-specific, so an old worker cannot close a
new generation's stream. A new configuration retires in-flight work rather than
drain it under partially changed policy.
