# Retry-After scope

HTTP 429 and 503 inference responses with a valid Retry-After restriction create
a local gate for the credential fingerprint and **final upstream model**. A
status code alone does not establish an account-wide quota or outage, so both
statuses use the same model scope. No undocumented error-string or header
heuristics promote a restriction to account scope.

The credential fingerprint still covers the key, Console server, organization,
and credential proxy. Model identity comes from the final serialized request,
after provider/model/variant overlays. For Gemini it comes from the URL model,
with the optional `models/` prefix normalized just as request preparation does.
Public aliases, configured aliases, reasoning labels and explicit variants that
resolve to the same actual model therefore share a gate. Changes to reasoning
options alone cannot bypass it. Actual upstream model IDs remain case-sensitive;
the plugin does not guess that arbitrary suffixes in real model IDs are aliases.
Unknown client aliases still fail route lookup instead of creating a new scope.

Retry-After-Ms takes precedence when valid, including fractional milliseconds;
otherwise Retry-After accepts nonnegative integer seconds or an HTTP date. The
server's full deadline is retained. Local checks do not refresh or extend it,
and a shorter concurrent upstream response cannot shorten a longer restriction.
The plugin neither sleeps, retries inference, rotates credentials nor switches
models. An explicit request for an unrelated model or distinct credential is
checked against its own gate. SSE errors carried by a successful HTTP response
continue to have no Retry-After side effects.

## Reload and upgrade

In-process configuration reload and quiesce/resume retain the same Engine's
model-scoped gates and absolute deadlines. Terminal shutdown, replacement of a
shared library, or process restart constructs a new Engine and does **not**
persist the old gates. There is no state export, automatic migration, or seed
configuration in this change.

All scope/deadline tests use synthetic credentials, injected clocks and fake
HTTP transports; no live provider inference is needed to validate this behavior.
