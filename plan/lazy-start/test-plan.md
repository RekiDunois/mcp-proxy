# Lazy backend startup test plan and recipe

Revision: T1.1 (2026-10-24). Requirement and baseline: execution-plan.md R1.1, fork master `a0e6dfb620020a729338e8c993aaa45667211de4`.
Proposed review status: pending. Gate 1 (scope), Gate 2 (recipe) await independent review; Gate 3 (results) not executed.

## Scope and sufficient checks

The objective is on-demand activation and reuse of a configured backend, preserving directly affected eager behavior. Standard Go assertions and the existing compiled-binary e2e fixture are sufficient; no new general fixture framework, custom evidence parser or real user knowledge base. Curl is permitted by the user, not mandatory: existing e2e HTTP MCP clients already issue real initialize/catalog/tool requests, so do not add a second curl pass proving the same behavior. No real Codex/eval, model download, browser, credentials or DB is needed.

### T1 Configuration compatibility

Input: per-backend lazyStart omitted, explicit true/false, and lazyStart true with inherited panicIfInvalid true. Expected: omitted/false remains eager, true opts in, incompatible effective combination fails validation before startup. Check in existing config_test.go with `TestLazyStartOptions`; use existing load/validate functions. No exhaustive option matrix.

### T2 First use and shared normal operation

Input: compiled proxy with one auth-protected lazy stdio backend using the existing stdio-server fixture; one disabled backend remains disabled. Before access, query readiness and send unauthorized MCP initialize. Then two independent MCP clients connect concurrently, list tools, and each calls echo with a distinct marker and pid. Expected: readiness succeeds while dormant; no fixture startup before authorized access; unauthorized request is 401; both callers receive their own echo marker, complete catalog and same pid; one fixture startup. Check new `TestLazyStdioSharedStartup` in test/e2e/lazy_test.go. Extend existing fixture with an optional startup marker file only to observe the directly required absence/single start (no separate collector). Use exclusive t.TempDir and existing freeAddr/build/start helpers.

### T3 Activation failure and waiting lifecycle

Input: root tests reuse the existing raw downstream helpers to gate initialize/tools-list, fail required catalog once, cancel one waiter while another remains, and initiate shutdown during a blocked attempt. Expected: no request crosses the activation gate before the catalog completes; canceled waiter exits but shared attempt succeeds for remaining caller; failed catalog is unavailable, its transport is closed, route remains and later activation can succeed; shutdown returns waiting callers and leaves no successful late publication. Check new `TestLazyActivationLifecycle` in root lazy_test.go with bounded channels and existing testing/httptest facilities. These scenarios cover the changed lifecycle only, not every protocol failure or catalog shape.

### T4 Activated recovery without duplicate live processes

Input: lazy stdio fixture after activation with autoReconnect true, force only its recorded process to fail through the existing pid/kill mechanism; observe recovery. A separate autoReconnect false case checks the directly different policy. Expected: true recovers with one replacement process and old PID gone before replacement startup; false stays degraded and does not create a replacement merely due to a later request. Proxy shutdown closes the active fixture. Check `TestLazyStdioReconnect` in e2e/lazy_test.go. Reuse fixture startup marker PID records and existing process-liveness helpers; no global process-name scans. Initial failure cooldown/background retry is covered in `TestLazyActivationLifecycle` using existing raw HTTP failure fixture, not another stdio installation.

### T5 Affected eager compatibility and static correctness

Input: unchanged eager config and existing supported transport fixtures. Run existing `TestStdioServerOverHTTP`, `TestAutoReconnectMountsBackendThatStartsLater`, `TestAutoReconnectRebuildsDroppedBackend`, and root `TestCatalogTimeoutBoundsHungToolsList`. Expected: existing assertions pass; no lazy option required. Run go vet on affected root/e2e packages. This is the small direct compatibility surface touched, not full repository regression. Root lazy lifecycle tests run under race detection once. The e2e driver uses race detection, but its existing build helper builds ordinary child binaries; no product race-coverage claim is made for those child processes.

## Environment and preparation

Cloud Linux disposable workspace `/workspace/shared/mcp-proxy-lazy-20261024`, branch `feat/lazy-backend-start`. No user Mac, real KB, OAuth store or existing backend process. Go must satisfy go.mod (1.25.5), with C compiler for the race detector. The cloud `/usr/bin/go` is not a Go compiler; use a verified official Go distribution under `/tmp/mcp-proxy-go/go` if no existing compiler is available. Dependency downloads use the ordinary Go module mechanism. No global installation or config changes.

Preparation, before any formal run (does not claim test results):

```bash
set -euo pipefail
cd /workspace/shared/mcp-proxy-lazy-20261024
export PATH=/tmp/mcp-proxy-go/go/bin:$PATH
export GOPATH=/tmp/mcp-proxy-go/gopath GOCACHE=/tmp/mcp-proxy-go/build-cache
export RUN_DIR=$(mktemp -d /tmp/mcp-proxy-lazy-results.XXXXXX)
printf '%s\n' "$RUN_DIR"
go version > "$RUN_DIR/toolchain.txt" 2>&1
command -v cc >> "$RUN_DIR/toolchain.txt"
git rev-parse HEAD > "$RUN_DIR/revision.txt"
git diff --stat >> "$RUN_DIR/revision.txt"
```

The executor records RUN_DIR once and uses it in each independent shell. E2e helpers build binaries once per test invocation and use exclusive temporary directories/ports; existing t.Cleanup owns process termination. Test tokens are fixed dummy values from existing fixture. Test-only fixture controls must be opt-in and unset for normal fixture behavior. Product code receives no test-only endpoint or lifecycle bypass.

## Commands and evidence

Each following block is a separate shell invocation/script, starts with its own strict mode, and writes a distinct log. A failed block does not prevent running another independent block. Use the same exported absolute RUN_DIR value and toolchain PATH from preparation in every invocation; do not create a new result directory per block. Tests use deterministic bounded waits and report relevant assertion diagnostics, not arbitrary sleeps as success criteria.

T1:
```bash
set -euo pipefail
cd /workspace/shared/mcp-proxy-lazy-20261024
export PATH=/tmp/mcp-proxy-go/go/bin:$PATH
export GOPATH=/tmp/mcp-proxy-go/gopath GOCACHE=/tmp/mcp-proxy-go/build-cache
: "${RUN_DIR:?set to preparation output}"
go test -count=1 -run '^TestLazyStartOptions$' . 2>&1 | tee "$RUN_DIR/T1.log"
```

T2 and T4 run together to reuse the compiled e2e binary; each Go test is independently reported even when the other fails:
```bash
set -euo pipefail
cd /workspace/shared/mcp-proxy-lazy-20261024
export PATH=/tmp/mcp-proxy-go/go/bin:$PATH
export GOPATH=/tmp/mcp-proxy-go/gopath GOCACHE=/tmp/mcp-proxy-go/build-cache
: "${RUN_DIR:?set to preparation output}"
go test -race -count=1 -v -run '^TestLazyStdio(SharedStartup|Reconnect)$' ./test/e2e 2>&1 | tee "$RUN_DIR/T2-T4.log"
```

T3:
```bash
set -euo pipefail
cd /workspace/shared/mcp-proxy-lazy-20261024
export PATH=/tmp/mcp-proxy-go/go/bin:$PATH
export GOPATH=/tmp/mcp-proxy-go/gopath GOCACHE=/tmp/mcp-proxy-go/build-cache
: "${RUN_DIR:?set to preparation output}"
go test -race -count=1 -v -run '^TestLazyActivationLifecycle$' . 2>&1 | tee "$RUN_DIR/T3.log"
```

T5 compatibility uses two separate scripts so a root failure cannot suppress e2e checks:
```bash
set -euo pipefail
cd /workspace/shared/mcp-proxy-lazy-20261024
export PATH=/tmp/mcp-proxy-go/go/bin:$PATH
export GOPATH=/tmp/mcp-proxy-go/gopath GOCACHE=/tmp/mcp-proxy-go/build-cache
: "${RUN_DIR:?set to preparation output}"
go test -count=1 -v -run '^TestCatalogTimeoutBoundsHungToolsList$' . 2>&1 | tee "$RUN_DIR/T5-root.log"
```
```bash
set -euo pipefail
cd /workspace/shared/mcp-proxy-lazy-20261024
export PATH=/tmp/mcp-proxy-go/go/bin:$PATH
export GOPATH=/tmp/mcp-proxy-go/gopath GOCACHE=/tmp/mcp-proxy-go/build-cache
: "${RUN_DIR:?set to preparation output}"
go test -count=1 -v -run '^(TestStdioServerOverHTTP|TestAutoReconnectMountsBackendThatStartsLater|TestAutoReconnectRebuildsDroppedBackend)$' ./test/e2e 2>&1 | tee "$RUN_DIR/T5-e2e.log"
```

T5 vet separately:
```bash
set -euo pipefail
cd /workspace/shared/mcp-proxy-lazy-20261024
export PATH=/tmp/mcp-proxy-go/go/bin:$PATH
export GOPATH=/tmp/mcp-proxy-go/gopath GOCACHE=/tmp/mcp-proxy-go/build-cache
: "${RUN_DIR:?set to preparation output}"
go vet . ./test/e2e 2>&1 | tee "$RUN_DIR/T5-vet.log"
```

The named new tests are implementation deliverables, not present at baseline. Before execution verify they exist and the commands select nonzero intended tests; missing tests are not PASS. Save test command exit status in the execution record. A compilation error or unavailable Go/race prerequisite is recorded as blocked/unable-to-judge, not a business failure. Assertions describe the expected behaviors above, not current output.

## Failure handling, retries and completion

All formal attempts remain in separate attempt-suffixed logs; no overwriting failures. Stop an affected case on product failure and repair the implementation, then rerun only affected cases after impact review. No unchanged re-run seeking a better result. External module-download failure may retry once after connectivity is restored, with unchanged dependency versions; persistent failure stops affected runs. A loopback port allocation race may retry the affected test once with a new exclusive port and directory after old owned processes exit. Do not escalate to user state or unrelated processes.

Record concise results in `plan/lazy-start/test-results.md`: actual product/test revision (and dirty diff if relevant), toolchain, commands, each T1–T5 outcome, all failures/retries, evidence paths, remaining limitations. Raw temporary configs/logs stay under /tmp; only necessary sanitized outcome text is committed. Author records observed outcomes, reviewer gives Gate 3; neither self-awarded approval nor unexecuted PASS. No all-platform or arbitrary-backend safety claim from Linux fixture results.
