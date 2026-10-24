# Idle shutdown implementation and result review

Reviewed 2026-10-24. Requirement: user-authorized idle shutdown and first-use lazy reactivation. Target repository: RekiDunois/mcp-proxy. Base SHA: `25a9b449286b00968838cfbc27c9969628745e6c`. Reviewed implementation Head SHA: `c3a7fe45dac58f5e0502d855b98dfc96c48d0fbe`. Canonical plans: execution R1, tests T1; plan approval: plan-review.md. Dependency remains mcp-go v1.1.0.

## Test Gate 3

PASS. All required checks have current applicable passing results in test-results.md. Adopted the executor's approved-method results; this reviewer did not rerun tests. Config and root lifecycle/race, real stdio idle/wake/long-call lifecycle, existing named lazy/eager compatibility and vet passed. Root race instruments product; e2e race instruments the driver only. No arbitrary-backend session-state, platform or remote-memory guarantee is inferred.

One test implementation correction was requested: parse the new readiness helper's JSON into a typed status rather than match a substring. The existing idle e2e cases were rerun after this correction and passed, as did vet. Initial results remain recorded. No product code changed; unchanged root and other compatibility results remain applicable. No additional case matrix or proof framework was required.

## Merge review

M1 Authority & Baseline: PASS.
M2 Implementation Conformance: PASS.
M3 Scope, Regression & Merge Readiness: PASS.
Review completeness: COMPLETE.
Defects: none outstanding.
Required modifications: none.
Merge conclusion: READY.

Reviewed the complete baseline-to-head diff, including new idle.go, root/e2e tests, fixture change and documentation; traced the production configuration, authenticated HTTP routing, forwarding, shared lazy activation, timer, probe/reconnect and Close/shutdown paths. The publication commit was checked to contain the frozen reviewed worktree.

Activity is acquired before resolving the raw client and released after forwarding. SSE asynchronous work has its own lease; persistent GET streams do not pin residency. Closing is published before teardown and new requests wait at its barrier. Idle eligibility requires no activity/probe and healthy ready state. Probe completion and canceled startup waiters cannot permanently lose the idle timer. Reconnect checks activity under the same activation lock; confirmed failure is not reset to intentional dormancy. Cleanup errors retain ownership and terminal replacement prohibition. Disabled idle preserves the existing paths. Remote close uses existing transport cleanup, not remote process termination.

Configuration is opt-in and requires lazyStart; documentation accurately states restart-only config changes and loss of backend in-memory/session/background state. The optional fixture delay is isolated to explicit test environment variables. No deployment, real user backend operation, business-state repair, external PR approval or merge was performed by this reviewer.

Adding this review record alone is a non-semantic documentation change: the implementation and Gate 3 results remain applicable without rerunning tests. Any later product/test/requirement change needs the corresponding impact review; READY does not authorize automatic merge.
