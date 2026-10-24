# Independent plan and test-design review

Reviewer: independent review agent, 2026-10-24. Review only; no product/test implementation or test execution by this reviewer.

- Requirement revision: user request 2026-10-24 to implement on the RekiDunois fork in the cloud, write execution/test plans first, test, then submit a PR. Curl is permitted, not required.
- Execution plan: R1.1, SHA-256 `d340440985c13d774bbcef6b9692049ee316fce0e33d928be5d492d44dba139c`.
- Test plan: T1.1, SHA-256 `3a5b7e9545b50a9e2a1dedf556936238f74d6c29de1e034ce029c4abd4ca4de5`.
- Target baseline: `a0e6dfb620020a729338e8c993aaa45667211de4`, `RekiDunois/mcp-proxy`, branch `feat/lazy-backend-start`.
- Runtime/upstream: Go 1.25.5; mark3labs/mcp-go v1.1.0.

Rules read: [Plan Reviewer Rule](https://app.notion.com/p/3e80101a85f3817a94cbc6eca1a76a5c), [Test Engineer Rule](https://app.notion.com/p/3d80101a85f380d8a8f5ce528141e253), and [Project Consensus](https://app.notion.com/p/3d60101a85f3815a8c43f271f2b4d91d).

## Execution-plan gates

- P1 Requirement & Reality Grounding: PASS. Deferred construction is necessary because `newMCPClient/buildRawClient` constructs stdio immediately; `http.go:startHTTPServer` currently mounts only after connection. `connect` publishes its raw pointer before catalog completion, so the proposed full-attempt readiness gate has a real basis. Reviewed mcp-go v1.1.0 `client/transport/stdio.go:Close`: it waits for process exit and returns cleanup/timeout errors. The plan correctly does not treat uncertain cleanup as permission to replace.
- P2 Solution Validity & Ownership: PASS. Configuration, transport lifecycle and route/readiness changes remain in their existing producer owners. Per-backend sharing, first-request activation, one in-flight attempt, gateway-owned context and unchanged forwarding meet the stated scope without per-harness bridge processes or catalog caching.
- P3 Impact, Safety & Counterexample: PASS. Authentication precedes activation; health checks cannot start a backend; request cancellation does not cancel shared startup; lazy replacement closes old ownership first; shutdown and failed required catalog are covered by the ownership invariant. Default eager behavior, effective panicIfInvalid handling, autoReconnect policy and remote-service limitations are explicit. No new credentials, user-machine state, Nix or application-database changes are authorized here.
- P4 Executability: PASS. Change points, implementation sequence, failure/cooldown strategy and cleanup ownership are sufficiently frozen. Internal mutex/channel representation remains an implementation choice.

Gate review completeness: COMPLETE.
Critical assumptions: none unresolved for plan approval.
Critical unresolved impacts: none.
Required modifications: none.
Reused prior Plan Gate PASS: none; this is the first independent complete review of the canonical candidates above.
Plan conclusion: APPROVED.

This approves the design for implementation, not implementation correctness, test results or merge readiness. Per-backend lifecycle coordination is internal design; it does not become a new public API or authorize a general lifecycle framework.

Implementation handoff note: after lazy cleanup removes a raw client, the existing `probe` nil-client error must not be interpreted as evidence of a live downstream by `startPingTask`. This follows the already-approved failure/recovery semantics in R1.1 sections 5–6; it is not a new goal or test obligation. Preserve terminal cleanup failure even though a repeated SDK Close may return nil after its once-only cleanup has run.

## Test-design and recipe gates

- Gate 1, must-test scope/design: APPROVED. T1–T5 cover opt-in compatibility, dormant/auth behavior, concurrent first-use reuse with actual calls, directly changed wait/failure/shutdown lifecycle, activated recovery policy, and the narrow affected eager paths. Existing root/e2e fixtures and ordinary assertions suffice. There is no second curl/Codex certification, full-repository regression, idle cache or KB exercise.
- Gate 2, recipe: APPROVED for implementation handoff. Commands, isolation, toolchain/cache locations, logs, independent script boundaries and bounded retry policy are explicit. T2/T4 are distinct Go tests in one invocation, so one assertion failure does not skip the other. All four named existing T5 tests were verified in source. New T1–T4 test functions are explicitly implementation deliverables; they do not exist at baseline. Their presence and nonzero selection must be confirmed before formal execution as already specified, not presumed completed now.
- Gate 3, actual results: NOT EXECUTED / NOT REVIEWED. No test PASS is asserted.

Evidence limits: existing `test/e2e/helpers_test.go:buildBinary` invokes ordinary `go build`. Thus `go test -race` on the e2e package does not by itself instrument the separately compiled proxy. Root T3 directly exercises the lazy lifecycle under race detection; do not describe the e2e run as proof of race instrumentation inside its child proxy. This is a reporting boundary, not an additional test requirement.

T4's startup-marker and owned-PID observation is limited to the specified single-process lifecycle; do not turn it into general process provenance certification or global process scanning. Test-result review will use the declared assertions and outputs rather than invent another proof system.

Next owner: implementation/test author implements R1.1/T1.1, records actual results and any deviations, then hands implementation/results to an independent reviewer before the publishing owner pushes and opens the PR. No automatic merge.

Pointer update: T1.1 clarified the race boundary already recorded above. Commands, scope and expected behavior are unchanged; execution gates and test-design/recipe approvals are reused without new execution.
