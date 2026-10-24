package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

const testIdleTimeout = 500 * time.Millisecond

func idleStdioConfig(t *testing.T, serverType string, reconnect bool) (string, string, string, string) {
	t.Helper()
	path, addr, starts := lazyStdioConfig(t, reconnect)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	config["mcpProxy"].(map[string]any)["type"] = serverType
	fixture := config["mcpServers"].(map[string]any)["fixture"].(map[string]any)
	fixture["idleTimeout"] = testIdleTimeout.String()
	entered := filepath.Join(t.TempDir(), "echo-entered")
	env := fixture["env"].(map[string]any)
	env["MCP_PROXY_TEST_ECHO_DELAY"] = (4 * testIdleTimeout).String()
	env["MCP_PROXY_TEST_ECHO_ENTER_FILE"] = entered
	body, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return writeConfig(t, string(body)), addr, starts, entered
}

// Keep one initialized frontend client, including its open SSE event stream,
// across several real subprocess lifetimes. Neither that stream nor readiness
// polling/automatic recovery may keep an idle backend alive or resurrect it.
func TestLazyStdioIdle(t *testing.T) {
	skipShort(t)
	for _, serverType := range []string{"sse", "streamable-http"} {
		t.Run(serverType, func(t *testing.T) {
			for _, reconnect := range []bool{true, false} {
				t.Run(fmt.Sprintf("autoReconnect=%t", reconnect), func(t *testing.T) {
					config, addr, marker, entered := idleStdioConfig(t, serverType, reconnect)
					p := startProxy(t, config, addr)
					defer p.stop(t)
					checkHealthEndpoints(t, p)
					if starts := lazyStarts(t, marker); len(starts) != 0 {
						t.Fatalf("health checks started dormant fixture: %v", starts)
					}

					c := p.connect(t, serverType, "fixture")
					first := idleFixturePID(t, c)
					assertIdleStarts(t, marker, []int{first})
					assertLazyProcessGone(t, first)
					// This spans multiple configured 200ms probe and 100ms retry
					// intervals without sending any MCP activity.
					observeIdleState(t, p, marker, []int{first}, 0, 2*testIdleTimeout)

					// All requests use the original client/session, not fresh
					// initializes that could hide a broken frontend connection.
					second := concurrentIdleWake(t, c)
					if second == first {
						t.Fatalf("idle wake reused exited fixture PID %d", first)
					}
					assertIdleStarts(t, marker, []int{first, second})
					checkIdleToolCatalog(t, c)
					checkPrompts(t, c)
					checkResources(t, c)

					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					finished := make(chan idleCallResult, 1)
					go func() {
						text, err := idleCallText(ctx, c, "echo", map[string]any{"message": "long idle-safe echo"})
						finished <- idleCallResult{text, err}
					}()
					waitForIdleEchoEntry(t, p, entered, second, finished)

					// The fixture's entry marker proves actual forwarding began.
					// SSE's POST already returned 202 by this point, so a lease
					// covering only HTTP handler lifetime cannot pass this check.
					observeIdleState(t, p, marker, []int{first, second}, second, 2*testIdleTimeout)
					select {
					case result := <-finished:
						if result.err != nil || result.text != "long idle-safe echo" {
							t.Fatalf("long echo = %q, %v\n%s", result.text, result.err, p.stderr.String())
						}
					case <-ctx.Done():
						t.Fatalf("long echo did not finish: %v\n%s", ctx.Err(), p.stderr.String())
					}
					// Completion, not call entry, must start the new idle clock.
					observeIdleState(t, p, marker, []int{first, second}, second, testIdleTimeout/2)
					assertLazyProcessGone(t, second)
					observeIdleState(t, p, marker, []int{first, second}, 0, testIdleTimeout)

					third := idleFixturePID(t, c)
					if third == first || third == second {
						t.Fatalf("final wake reused exited fixture PID %d", third)
					}
					assertIdleStarts(t, marker, []int{first, second, third})
					// Stop while the last generation is live, rather than merely
					// shutting down an already dormant route.
					assertIdleProcessAlive(t, third)
					p.stop(t)
					assertLazyProcessGone(t, third)
					assertIdleStarts(t, marker, []int{first, second, third})
				})
			}
		})
	}
}

type idleCallResult struct {
	text string
	err  error
}

// Return errors to the caller so concurrent workers never call Fatal/FailNow.
func idleCallText(ctx context.Context, c *client.Client, name string, args map[string]any) (string, error) {
	request := mcp.CallToolRequest{}
	request.Params.Name, request.Params.Arguments = name, args
	result, err := c.CallTool(ctx, request)
	if err != nil {
		return "", err
	}
	if result.IsError || len(result.Content) != 1 {
		return "", fmt.Errorf("%s result = %+v", name, result)
	}
	text, ok := mcp.AsTextContent(result.Content[0])
	if !ok {
		return "", fmt.Errorf("%s result is not text", name)
	}
	return text.Text, nil
}

func idleFixturePID(t *testing.T, c *client.Client) int {
	t.Helper()
	pid, err := strconv.Atoi(callToolText(t, c, "pid", nil))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid fixture PID %d: %v", pid, err)
	}
	return pid
}

func concurrentIdleWake(t *testing.T, c *client.Client) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const callers = 6
	barrier := make(chan struct{})
	results := make(chan idleCallResult, callers)
	for range callers {
		go func() {
			<-barrier
			text, err := idleCallText(ctx, c, "pid", nil)
			results <- idleCallResult{text, err}
		}()
	}
	close(barrier)
	var shared int
	for range callers {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("concurrent idle wake: %v", result.err)
			}
			pid, err := strconv.Atoi(result.text)
			if err != nil || pid <= 0 {
				t.Fatalf("concurrent PID = %q: %v", result.text, err)
			}
			if shared != 0 && shared != pid {
				t.Fatalf("concurrent idle wake returned different PIDs %d and %d", shared, pid)
			}
			shared = pid
		case <-ctx.Done():
			t.Fatalf("concurrent idle wake did not finish: %v", ctx.Err())
		}
	}
	return shared
}

func checkIdleToolCatalog(t *testing.T, c *client.Client) {
	t.Helper()
	tools, err := c.ListTools(testContext(t), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools after idle wake: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{"add", "echo", "fail", "getenv", "hang", "noisy_stderr", "pid"}; !slices.Equal(names, want) {
		t.Fatalf("catalog after idle wake = %v, want %v", names, want)
	}
}

func assertIdleStarts(t *testing.T, marker string, want []int) {
	t.Helper()
	if starts := lazyStarts(t, marker); !slices.Equal(starts, want) {
		t.Fatalf("startup PIDs = %v, want %v", starts, want)
	}
}

func assertIdleProcessAlive(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("active fixture PID %d exited prematurely: %v", pid, err)
	}
}

// Health is deliberately the only HTTP traffic during this observation. A
// nonzero livePID additionally asserts an in-flight or newly completed request
// has kept that exact backend alive, not silently replaced it.
func observeIdleState(t *testing.T, p *proxy, marker string, starts []int, livePID int, duration time.Duration) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for {
		code, body := p.ready(t)
		var readiness struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(body), &readiness); err != nil {
			t.Fatalf("decode readiness: %v: %s", err, body)
		}
		if code != http.StatusOK || readiness.Status != "ok" {
			t.Fatalf("readiness during idle lifecycle = %d %s\n%s", code, body, p.stderr.String())
		}
		assertIdleStarts(t, marker, starts)
		if livePID != 0 {
			assertIdleProcessAlive(t, livePID)
		}
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForIdleEchoEntry(t *testing.T, p *proxy, path string, pid int, finished <-chan idleCallResult) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case result := <-finished:
			t.Fatalf("long echo finished before entry was observed: %q, %v\n%s", result.text, result.err, p.stderr.String())
		default:
		}
		if entered := lazyStarts(t, path); len(entered) != 0 {
			if !slices.Equal(entered, []int{pid}) {
				t.Fatalf("echo entered PIDs %v, want [%d]", entered, pid)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("long echo never entered fixture PID %d\n%s", pid, p.stderr.String())
}
