# Lazy backend startup execution plan

Plan revision: R1.1 (2026-10-24)
Requirement revision: user request of 2026-10-24, implement on the RekiDunois fork, write both plans before implementation, test with curl permitted, then open a PR.
Target: `RekiDunois/mcp-proxy`, branch `feat/lazy-backend-start`, baseline `a0e6dfb620020a729338e8c993aaa45667211de4` (master).
Relevant runtime: Go 1.25.5 from go.mod; mark3labs/mcp-go v1.1.0. Cloud Linux only.
Proposed review status: pending. No implementation or formal test result is claimed.

## Requirement and boundary

Add opt-in on-demand backend startup to the existing shared HTTP MCP proxy. A configured backend is started/connected on its first authenticated request and reused by concurrent and subsequent clients. No backend process starts just because its lazy route is configured. Existing configurations stay eager. The unit of sharing is one configured backend in one proxy process, not all processes with the same executable name, nor a cross-proxy lock.

The trigger is the first request to that MCP route, including initialize or tools/list, not only tools/call. A harness that eagerly connects every configured endpoint can activate all of them. HTTP/SSE downstreams can defer a connection but cannot launch a remote service. Shared transports do not make a backend's own application session state or database transactions multi-user safe.

Non-goals: idle unloading, offline catalog cache, generic lifecycle framework, application database locks, Nix integration, KB code changes, changes to authentication grants, running on the user's Mac, modifying upstream instead of the fork, automatic merge.

## Facts and decisions

All repository sources below refer to the baseline above.

- Delay raw construction, not merely initialize: `client.go:newMCPClient/buildRawClient` calls NewStdioMCPClient; [mcp-go v1.1.0 client/stdio.go](https://github.com/mark3labs/mcp-go/blob/v1.1.0/client/stdio.go) starts the stdio transport in that constructor.
- Route owner is `http.go:startHTTPServer`: currently mountRoute runs only after addToMCPServer succeeds. Lazy routes must mount before connecting, inside the existing recover/log/auth middleware order; auth rejects before activation.
- Connection/catalog owner is `client.go:connect`: connectMu serializes but repeated connect rebuilds; raw pointer publication precedes tools registration. Therefore non-nil pointer is not readiness. Share completion of the entire connection/catalog operation, not individual calls to connect.
- Lifecycle context is the gateway context: `connect` passes it to Start; using an HTTP request context would tie a shared SSE/stdio session to one caller. Only the caller's waiting is cancelable by its request.
- Cleanup before replacement: `connect` currently initializes a new transport before closing old. For the lazy lifecycle, close old before constructing new. [mcp-go v1.1.0 transport/stdio.go Close](https://github.com/mark3labs/mcp-go/blob/v1.1.0/client/transport/stdio.go) waits for process exit, escalates termination and returns failure if unable to reap. Do not start a replacement after uncertain cleanup; preserve that failure rather than discarding ownership.
- Readiness owner is `http.go` healthReport: healthUnknown currently excludes clients because they have no mounted route. A dormant lazy route is mounted and serviceable, so count it without starting it. A failed activation reports degraded rather than silently appearing healthy.
- Existing keepalive/autoReconnect are in `client.go:startPingTask`, startup retry is in `http.go`. No keepalive or retries while dormant; activation enables applicable retry policy. Existing eager behavior remains unchanged.

## Chosen design and ownership

1. Add `MCPClientConfigV2.LazyStart` as a per-backend boolean, default false, without a new global inheritance option. Validate lazyStart together with effective inherited panicIfInvalid as a config error: runtime activation must not unexpectedly terminate the gateway, and fail-fast startup cannot be fulfilled when startup is deferred. Document it beside those options.
2. `newMCPClient` parses/configures lazy clients without constructing a raw transport. Eager constructor path remains unchanged. Register the lazy Client for shutdown and construct its local Server/route without touching the downstream.
3. A small per-client lazy lifecycle coordinates activation under a mutex with one in-flight attempt completion channel and stored outcome. First authenticated route request begins the attempt using gateway context; concurrent waiters observe the same result. Successful completion is after required tools catalog and existing best-effort optional catalogs. A request waiting for initialization can leave independently; it cannot cancel the shared attempt.
4. On a successful attempt, forward the request into the existing MCP HTTP handler. Keep existing endpoint paths, descriptors, tool filters, headers, protocol behavior and business request forwarding. Do not replay tools/call automatically. A failed attempt yields HTTP 503 with generic backend-unavailable text; detailed errors are logged with existing URL redaction. Route stays mounted. Initial failures are eligible for later request retry, paced by reconnectInterval to avoid request storms. Concurrent waiters receive their attempt's failure rather than silently triggering individual attempts.
5. After activation, autoReconnect true retains background recovery intent: initial failures retry at reconnectInterval after activation (except existing permanent OAuth failure); healthy-client failure recovery follows existing pingInterval/threshold. autoReconnect false has no background retry; initial failed activation can be retried by a later request after cooldown. Once it has connected successfully, failure follows the existing non-reconnecting degraded policy, not an implicit request-triggered restart.
6. For lazy clients, serialize activation, reconnect and cleanup with the existing connection lock plus the small activation coordinator; ensure only one path owns a connection attempt. Close/forget failed initialize or required-catalog transport before admitting another attempt. Close previous lazy transport before building replacement. Treat nonzero process exit as reaped (existing Close behavior); on other uncertain close errors retain a terminal cleanup failure and do not construct a replacement. No extra process manager or generic state framework.
7. Shutdown marks closed, cancels gateway work, wakes startup waiters and closes owned raw transport. A transport created concurrently must be closed before attempt completion if shutdown has won. No successful readiness publication after closed. Avoid waiting for connectMu while holding the mutex used to publish/close raw state. Keep idempotent Close.
8. Readiness counts dormant lazy routes, reports activated failed backends degraded, and never starts a backend itself. Eager startup grace, unavailable and existing health semantics remain.

## Invariants and representative counterexamples

- One configured lazy stdio backend has at most one live owned process. Concurrent initialize requests join one attempt; reconnect waits for cleanup, so no old/new heavy-model overlap.
- No request is forwarded before catalog initialization completes. Early raw pointer publication is insufficient.
- A canceled first caller does not kill other callers' startup; gateway cancellation does stop it.
- Failed catalog or shutdown-during-startup does not leave a newly created raw client unowned.
- Unauthorized requests and health checks cannot activate dormant backends.
- Eager configurations are not silently converted to lazy, and lazy does not override fail-fast semantics.

These are design constraints for this change, not requests for exhaustive boundary matrices.

## Alternatives and impact

Keeping a per-harness stdio-to-HTTP bridge would still create per-client processes and does not meet the chosen direct HTTP gateway setup. An offline catalog cache could defer until tools/call but introduces stale-descriptor behavior and is unnecessary for first-request activation. Selected approach reuses the existing handlers and configured backend identity.

Intended changes: optional lazy activation, dormant readiness, activation failure HTTP response and cleanup-before-replacement for lazy backends. Preserved: default eager behavior, existing auth/token inheritance, endpoint routing, supported transports, forwarding and filter semantics, keepalive intent and fail-fast for eager backends. Prohibited: bypass auth, alter user data, bind to user environments, swallow cleanup uncertainty then duplicate a process, add unrelated harness/config management. No data migration; removing lazyStart restores eager behavior on next proxy restart.

## Implementation sequence and handoff

- Freeze these dual plans and obtain independent plan review before core implementation.
- Implement per-backend config/validation, deferred constructor and lifecycle coordination, then HTTP route/readiness wiring.
- Update CONFIGURATION.md, USAGE.md and affected CLAUDE.md lifecycle descriptions; document trigger, failure/retry semantics and limitations rather than promise arbitrary backend session safety.
- Add narrowly scoped tests using existing root Go tests/e2e fixture and follow test-plan.md; record actual tools, commands and results separately without claiming unexecuted success.
- Independent review of implementation/results, then normal commit/push and PR to the user's fork by publishing owner. No force push or automatic merge.

No critical unresolved product decision is intentionally deferred. Toolchain availability is an execution prerequisite, not evidence of test failure. Internal mutex/channel representation is implementation detail, not a public API contract.
