package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

func lazyStdioConfig(t *testing.T, reconnect bool) (string, string, string) {
	t.Helper()
	addr := freeAddr(t)
	marker := filepath.Join(t.TempDir(), "starts")
	var config map[string]any
	if err := json.Unmarshal([]byte(fmt.Sprintf(configTemplate, addr, "streamable-http", filepath.ToSlash(buildFixture(t)), testEnvValue)), &config); err != nil {
		t.Fatal(err)
	}
	servers := config["mcpServers"].(map[string]any)
	fixture := servers["fixture"].(map[string]any)
	fixture["lazyStart"] = true
	fixture["env"].(map[string]any)["MCP_PROXY_TEST_START_FILE"] = marker
	options := fixture["options"].(map[string]any)
	options["autoReconnect"] = reconnect
	options["reconnectInterval"] = "100ms"
	options["pingFailureThreshold"] = 1
	// The disabled entry shares the marker so an accidental launch is visible.
	servers["off"].(map[string]any)["env"] = map[string]string{"MCP_PROXY_TEST_START_FILE": marker}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return writeConfig(t, string(body)), addr, marker
}

func lazyStarts(t *testing.T, path string) []int {
	t.Helper()
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read startup marker: %v", err)
	}
	var pids []int
	for _, line := range strings.Fields(string(body)) {
		pid, err := strconv.Atoi(line)
		if err != nil || pid <= 0 {
			t.Fatalf("invalid startup PID %q", line)
		}
		pids = append(pids, pid)
	}
	return pids
}

func assertLazyProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatalf("check owned PID %d: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("owned fixture PID %d remains after cleanup", pid)
}

func TestLazyStdioSharedStartup(t *testing.T) {
	skipShort(t)
	config, addr, marker := lazyStdioConfig(t, false)
	p := startProxy(t, config, addr)
	defer p.stop(t)
	checkHealthEndpoints(t, p)
	if got := p.get(t, "/off/mcp"); got != http.StatusNotFound {
		t.Fatalf("disabled route = %d, want 404", got)
	}
	req, err := http.NewRequestWithContext(testContext(t), http.MethodPost, p.baseURL+"/fixture/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"unauthorized","version":"1"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized initialize = %d, want 401", resp.StatusCode)
	}
	if starts := lazyStarts(t, marker); len(starts) != 0 {
		t.Fatalf("health/unauthorized request started fixture: %v", starts)
	}

	// Both independent HTTP clients leave the barrier together; only the shared
	// backend should start. Return errors rather than Fatal from worker goroutines.
	ctx := testContext(t)
	barrier := make(chan struct{})
	type outcome struct {
		pid int
		err error
	}
	results := make(chan outcome, 2)
	for _, marker := range []string{"caller-A", "caller-B"} {
		go func(value string) {
			<-barrier
			pid, err := lazyCaller(ctx, p.baseURL+"/fixture/mcp", value)
			results <- outcome{pid, err}
		}(marker)
	}
	close(barrier)
	var pids []int
	for range 2 {
		select {
		case r := <-results:
			if r.err != nil {
				t.Errorf("concurrent caller: %v", r.err)
			} else {
				pids = append(pids, r.pid)
			}
		case <-ctx.Done():
			t.Fatal("concurrent callers did not finish")
		}
	}
	if t.Failed() {
		return
	}
	starts := lazyStarts(t, marker)
	if len(starts) != 1 || pids[0] != pids[1] || starts[0] != pids[0] {
		t.Fatalf("startup PIDs %v, caller PIDs %v; want one shared process", starts, pids)
	}
	p.stop(t)
	assertLazyProcessGone(t, pids[0])
}

func lazyCaller(ctx context.Context, endpoint, value string) (int, error) {
	c, err := client.NewStreamableHttpClient(endpoint, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + testAuthToken}))
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close() }()
	if err := c.Start(ctx); err != nil {
		return 0, err
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: value, Version: "1"}
	if _, err := c.Initialize(ctx, init); err != nil {
		return 0, fmt.Errorf("initialize: %w", err)
	}
	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return 0, err
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{"add", "echo", "fail", "getenv", "hang", "noisy_stderr", "pid"}; !slices.Equal(names, want) {
		return 0, fmt.Errorf("catalog = %v, want %v", names, want)
	}
	call := func(name string, args map[string]any) (string, error) {
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
	echo, err := call("echo", map[string]any{"message": value})
	if err != nil {
		return 0, err
	}
	if echo != value {
		return 0, fmt.Errorf("echo = %q, want %q", echo, value)
	}
	pid, err := call("pid", nil)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(pid)
}

func TestLazyStdioReconnect(t *testing.T) {
	skipShort(t)
	for _, reconnect := range []bool{true, false} {
		t.Run(fmt.Sprintf("autoReconnect=%t", reconnect), func(t *testing.T) {
			config, addr, marker := lazyStdioConfig(t, reconnect)
			p := startProxy(t, config, addr)
			defer p.stop(t)
			c := p.connect(t, "streamable-http", "fixture")
			old, err := strconv.Atoi(callToolText(t, c, "pid", nil))
			if err != nil {
				t.Fatal(err)
			}
			if starts := lazyStarts(t, marker); !slices.Equal(starts, []int{old}) {
				t.Fatalf("initial startups = %v, want [%d]", starts, old)
			}
			if err := syscall.Kill(old, syscall.SIGKILL); err != nil {
				t.Fatalf("kill owned fixture: %v", err)
			}
			if !reconnect {
				p.waitForReadyBody(t, http.StatusServiceUnavailable, `"status":"degraded"`)
				result, err := callTool(t, c, "echo", map[string]any{"message": "must-not-restart"})
				if err == nil && !result.IsError {
					t.Fatal("dead backend unexpectedly served echo")
				}
				// Observe multiple configured probe/retry intervals after the later request.
				deadline := time.Now().Add(time.Second)
				for time.Now().Before(deadline) {
					if starts := lazyStarts(t, marker); !slices.Equal(starts, []int{old}) {
						t.Fatalf("autoReconnect=false restarted fixture: %v", starts)
					}
					time.Sleep(20 * time.Millisecond)
				}
				p.stop(t)
				assertLazyProcessGone(t, old)
				return
			}
			var replacement int
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				starts := lazyStarts(t, marker)
				if len(starts) > 2 {
					t.Fatalf("duplicate replacements: %v", starts)
				}
				if len(starts) == 2 {
					if err := syscall.Kill(old, 0); !errors.Is(err, syscall.ESRCH) {
						t.Fatalf("old PID %d still present when replacement recorded: %v", old, err)
					}
					replacement = starts[1]
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if replacement == 0 || replacement == old {
				t.Fatalf("no distinct replacement; startups = %v\n%s", lazyStarts(t, marker), p.stderr.String())
			}
			p.waitForReady(t)
			if got := callToolText(t, c, "pid", nil); got != strconv.Itoa(replacement) {
				t.Fatalf("active PID = %s, want %d", got, replacement)
			}
			if got := callToolText(t, c, "echo", map[string]any{"message": "after-reconnect"}); got != "after-reconnect" {
				t.Fatalf("recovered echo = %q", got)
			}
			p.stop(t)
			assertLazyProcessGone(t, replacement)
			if starts := lazyStarts(t, marker); !slices.Equal(starts, []int{old, replacement}) {
				t.Fatalf("startup sequence = %v", starts)
			}
		})
	}
}
