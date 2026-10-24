# Independent idle shutdown plan review

Reviewed 2026-10-24, canonical execution R1 and test T1 after consolidated corrections. Requirement: authorized idle backend shutdown and first-use lazy reactivation. Target baseline: `25a9b449286b00968838cfbc27c9969628745e6c`; dependency mcp-go v1.1.0. Review was read-only for product code and did not execute tests.

P1 Requirement & Reality Grounding: PASS.
P2 Solution Validity & Ownership: PASS.
P3 Impact, Safety & Counterexample: PASS.
P4 Executability: PASS.
Gate review completeness: COMPLETE.
Plan conclusion: APPROVED.

Source facts were checked in main.go/load, lazy.go activation, client.go connect/Close/forwarding/probe, http.go route/shutdown, and mcp-go v1.1.0 SSE dispatch and transport Close. The existing lazy coordinator owns activation; connectMu owns raw teardown/replacement. Forwarding leases cover actual asynchronous SSE work while GET streams do not pin residency. These are implementation choices, not new public contracts.

The complete review consolidated and verified these corrections in R1: configuration changes require restart, not nonexistent hot reload; reconnect checks activity under the activation lock; confirmed failed health cannot be reclassified as intentional idle; successful activation with no remaining waiter and probe completion rearm idle scheduling, with probes retaining the user deadline. T1 now bounds environment retries to one after recovery/owned-state cleanup, preserving parameters and blocked status on continued failure.

An additional independent read-only reviewer checked the plans and baseline. Its final timer concern referred to an earlier read; this reviewer directly reread the canonical R1 timer paragraph and verified that correction before approval. No critical assumptions or unresolved impacts remain. State/session loss is explicitly disclosed; arbitrary backend state migration, remote service termination, and new hot-reload behavior are outside scope.

Test design and Gate 1 required scope: PASS.
Test Gate 2 executable recipe and bounded scope: PASS.
Test Gate 3 results: NOT EXECUTED, pending implementation and recorded execution.

The scope remains the stated config/lifecycle race checks, real stdio idle/wake and long calls through both existing frontend transports, and named affected compatibility checks. Existing framework and evidence paths suffice; no new protocol matrix, general harness, or duplicate curl pass is required. Approval permits implementation only, not a test-pass or merge conclusion.
