package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestIdleTimeoutConfig(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         time.Duration
		invalid      bool
	}{
		{"omitted", ``, 0, false}, {"zero", `,"idleTimeout":"0s"`, 0, false},
		{"enabled", `,"lazyStart":true,"idleTimeout":"2m"`, 2 * time.Minute, false},
		{"requires lazy", `,"idleTimeout":"1s"`, 0, true},
		{"negative", `,"lazyStart":true,"idleTimeout":"-1s"`, 0, true},
		{"bare seconds", `,"lazyStart":true,"idleTimeout":30`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.json")
			body := `{"mcpProxy":{"baseURL":"http://localhost:9090","addr":":9090","name":"test","version":"1"},"mcpServers":{"fixture":{"command":"not-run"` + tc.fields + `}}}`
			if err := os.WriteFile(p, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			conf, err := load(p, false, false, "", 10)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid idle config accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := time.Duration(conf.McpServers["fixture"].IdleTimeout); got != tc.want {
				t.Fatalf("idle timeout=%v want %v", got, tc.want)
			}
		})
	}
}

func idleTestClient(t *testing.T, d *rawDownstream, auto bool) *Client {
	c := lazyTestClient(t, d, auto)
	c.lazy.idleTimeout = 50 * time.Millisecond
	// lazyTestClient has not started its ping loop yet.
	c.options.PingInterval = Duration(10 * time.Millisecond)
	return c
}
func awaitIdleCondition(t *testing.T, name string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out: " + name)
}
func idleDormant(c *Client) bool {
	c.lazy.mu.Lock()
	defer c.lazy.mu.Unlock()
	return !c.lazy.ready && !c.lazy.everReady && c.lazy.closing == nil && c.getClient() == nil
}
func idleUse(t *testing.T, c *Client) func() {
	t.Helper()
	release, err := c.acquireIdle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return release
}

func TestLazyIdleLifecycle(t *testing.T) {
	t.Run("shared wake and probes do not extend deadline", func(t *testing.T) {
		d := newRawDownstream(t)
		d.setTools("alpha")
		c := idleTestClient(t, d, true)
		release := idleUse(t, c)
		old := c.getClient()
		release()
		awaitIdleCondition(t, "first idle", func() bool { return idleDormant(c) })
		time.Sleep(4 * c.options.pingInterval())
		if !idleDormant(c) {
			t.Fatal("probe woke dormant backend")
		}
		d.setTools("beta")
		d.mu.Lock()
		beforeWake := d.toolsListCalls
		d.mu.Unlock()
		const n = 8
		var wg sync.WaitGroup
		errs := make(chan error, n)
		gate := make(chan struct{})
		hold := make(chan struct{})
		entered := make(chan struct{}, n)
		for range n {
			wg.Go(func() {
				<-gate
				done, err := c.acquireIdle(t.Context())
				errs <- err
				if err == nil {
					entered <- struct{}{}
					<-hold
					done()
				}
			})
		}
		close(gate)
		for range n {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		for range n {
			<-entered
		}
		if raw := c.getClient(); raw == nil || raw == old {
			t.Fatal("did not construct fresh shared client")
		}
		d.mu.Lock()
		calls := d.toolsListCalls
		d.mu.Unlock()
		if calls != beforeWake+1 {
			t.Fatalf("wake catalog calls = %d, want %d", calls, beforeWake+1)
		}
		if c.mcpServer.GetTool("beta") == nil || c.mcpServer.GetTool("alpha") != nil {
			t.Fatal("wake did not refresh catalog")
		}
		// Initial catalog plus health probes make the absolute count variable;
		// while all leases are held, no further health probe may run.
		time.Sleep(3 * c.lazy.idleTimeout)
		d.mu.Lock()
		after := d.toolsListCalls
		d.mu.Unlock()
		if after != calls {
			t.Fatalf("probe ran during work: %d -> %d", calls, after)
		}
		if c.getClient() == nil {
			t.Fatal("closed active backend")
		}
		close(hold)
		wg.Wait()
		awaitIdleCondition(t, "idle after all callers", func() bool { return idleDormant(c) })
	})
	t.Run("long forwarded call cancellation releases", func(t *testing.T) {
		d := newRawDownstream(t)
		d.hangMethodsNamed("tools/call")
		c := idleTestClient(t, d, true)
		release := idleUse(t, c)
		release()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := c.callTool(ctx, mcp.CallToolRequest{}); result <- err }()
		awaitIdleCondition(t, "forwarded lease", func() bool { c.lazy.mu.Lock(); defer c.lazy.mu.Unlock(); return c.lazy.active > 0 })
		old := c.getClient()
		time.Sleep(3 * c.lazy.idleTimeout)
		if c.getClient() != old || old == nil {
			t.Fatal("long request lost transport")
		}
		cancel()
		if err := awaitLazy(t, result); !errors.Is(err, context.Canceled) {
			t.Fatalf("call error=%v", err)
		}
		awaitIdleCondition(t, "idle after canceled request", func() bool { return idleDormant(c) })
	})
	t.Run("failed call releases", func(t *testing.T) {
		d := newRawDownstream(t)
		c := idleTestClient(t, d, false)
		release := idleUse(t, c)
		// A canceled context fails the actual forwarding path without retaining its lease.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := c.readResource(ctx, mcp.ReadResourceRequest{})
		if err == nil {
			t.Fatal("canceled resource unexpectedly succeeded")
		}
		release()
		awaitIdleCondition(t, "idle after failed resource", func() bool { return idleDormant(c) })
	})
	t.Run("disabled remains resident", func(t *testing.T) {
		d := newRawDownstream(t)
		c := lazyTestClient(t, d, false)
		if err := c.ensureStarted(t.Context()); err != nil {
			t.Fatal(err)
		}
		old := c.getClient()
		time.Sleep(150 * time.Millisecond)
		if c.getClient() != old || old == nil {
			t.Fatal("default idle disabled did not preserve transport")
		}
	})
	t.Run("confirmed failure is not intentional idle", func(t *testing.T) {
		d := newRawDownstream(t)
		c := idleTestClient(t, d, false)
		c.needPing = false
		release := idleUse(t, c)
		old := c.getClient()
		c.health.Store(int32(healthFailed))
		release()
		time.Sleep(3 * c.lazy.idleTimeout)
		c.lazy.mu.Lock()
		ever := c.lazy.everReady
		c.lazy.mu.Unlock()
		if c.getClient() != old || !ever || c.Health() != healthFailed {
			t.Fatal("failure was relabeled dormant")
		}
	})
}

func TestLazyIdleOrdering(t *testing.T) {
	t.Run("request waits for close then wakes once", func(t *testing.T) {
		d := newRawDownstream(t)
		c := idleTestClient(t, d, false)
		c.needPing = false
		release := idleUse(t, c)
		old := c.getClient()
		c.connectMu.Lock()
		release()
		awaitIdleCondition(t, "closing published", func() bool { c.lazy.mu.Lock(); defer c.lazy.mu.Unlock(); return c.lazy.closing != nil })
		done := make(chan error, 1)
		go func() {
			release, err := c.acquireIdle(t.Context())
			if err == nil {
				release()
			}
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("request crossed close: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		c.connectMu.Unlock()
		if err := awaitLazy(t, done); err != nil {
			t.Fatal(err)
		}
		if c.getClient() == old {
			t.Fatal("old transport reused")
		}
	})
	t.Run("shutdown wins queued close and request", func(t *testing.T) {
		d := newRawDownstream(t)
		c := idleTestClient(t, d, false)
		c.needPing = false
		release := idleUse(t, c)
		c.connectMu.Lock()
		release()
		awaitIdleCondition(t, "closing", func() bool { c.lazy.mu.Lock(); defer c.lazy.mu.Unlock(); return c.lazy.closing != nil })
		waiting := make(chan error, 1)
		go func() {
			release, err := c.acquireIdle(t.Context())
			if err == nil {
				release()
			}
			waiting <- err
		}()
		stopped := make(chan error, 1)
		go func() { stopped <- c.Close() }()
		awaitIdleCondition(t, "closed", func() bool { return c.closed.Load() })
		c.connectMu.Unlock()
		if err := awaitLazy(t, waiting); err == nil {
			t.Fatal("request succeeded after shutdown")
		}
		if err := awaitLazy(t, stopped); err != nil {
			t.Fatal(err)
		}
		awaitIdleCondition(t, "close completed", func() bool { c.lazy.mu.Lock(); defer c.lazy.mu.Unlock(); return c.lazy.closing == nil })
		if c.getClient() != nil {
			t.Fatal("shutdown left transport")
		}
	})
	t.Run("canceled startup waiters still idle", func(t *testing.T) {
		d := newRawDownstream(t)
		entered, unblock := gateLazyTools(t, d)
		c := idleTestClient(t, d, false)
		c.needPing = false
		ctx, cancel := context.WithCancel(t.Context())
		waiting := make(chan error, 1)
		go func() {
			release, err := c.acquireIdle(ctx)
			if err == nil {
				release()
			}
			waiting <- err
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("startup not entered")
		}
		cancel()
		if err := awaitLazy(t, waiting); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		time.Sleep(2 * c.lazy.idleTimeout)
		close(unblock)
		awaitIdleCondition(t, "completed startup eventually idles", func() bool { return idleDormant(c) })
	})
	t.Run("probe completion rearms original deadline and defers recovery for new business", func(t *testing.T) {
		d := newRawDownstream(t)
		c := idleTestClient(t, d, true)
		c.needPing = false
		release := idleUse(t, c)
		release()
		probe, ok := c.acquireIdleProbe()
		if !ok {
			t.Fatal("cannot acquire probe")
		}
		c.lazy.mu.Lock()
		deadline := c.lazy.idleDeadline
		c.lazy.mu.Unlock()
		time.Sleep(2 * c.lazy.idleTimeout)
		if c.getClient() == nil {
			t.Fatal("idle closed probing transport")
		}
		business := idleUse(t, c)
		old := c.getClient()
		if err := c.reconnect(t.Context()); err == nil {
			t.Fatal("reconnect did not defer new business")
		}
		if c.getClient() != old {
			t.Fatal("reconnect changed live request transport")
		}
		business()
		c.lazy.mu.Lock()
		c.lazy.idleDeadline = deadline
		c.lazy.mu.Unlock()
		probe()
		awaitIdleCondition(t, "probe release rearmed elapsed deadline", func() bool { return idleDormant(c) })
	})
	t.Run("GET stream does not pin backend", func(t *testing.T) {
		d := newRawDownstream(t)
		c := idleTestClient(t, d, false)
		entered, leave := make(chan struct{}), make(chan struct{})
		h := c.lazyHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-leave }))
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/sse", nil))
		}()
		<-entered
		awaitIdleCondition(t, "idle with GET open", func() bool { return idleDormant(c) })
		close(leave)
		<-finished
	})
}
