package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/tbxark/optional-go"
)

func lazyTestClient(t *testing.T, d *rawDownstream, auto bool) *Client {
	t.Helper()
	opts := &OptionsV2{ReconnectInterval: Duration(40 * time.Millisecond), PingInterval: Duration(time.Hour)}
	opts.AutoReconnect = optional.NewField(auto)
	c, err := newMCPClient("lazy", &MCPClientConfigV2{LazyStart: true, TransportType: MCPClientTypeStreamable, URL: d.url, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	if c.getClient() != nil {
		t.Fatal("lazy constructor created transport")
	}
	c.bindLazy(t.Context(), mcp.Implementation{Name: "test"}, newProxyServerForTest(t))
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return c
}

// Gate one existing fixture endpoint, without replacing its MCP behavior.
func gateLazyTools(t *testing.T, d *rawDownstream) (<-chan struct{}, chan struct{}) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	original := d.server.Config.Handler
	d.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Method == "tools/list" {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		original.ServeHTTP(w, r)
	})
	return entered, release
}

func awaitLazy(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("activation did not finish")
		return nil
	}
}

func TestLazyActivationLifecycle(t *testing.T) {
	// No parallel subtests: the required-catalog timeout is shared by existing tests.
	old := catalogTimeout
	catalogTimeout = 200 * time.Millisecond
	defer func() { catalogTimeout = old }()
	t.Run("wait for catalog and cancel only one caller", func(t *testing.T) {
		d := newRawDownstream(t)
		d.setTools("alpha")
		entered, release := gateLazyTools(t, d)
		c := lazyTestClient(t, d, false)
		ctx, cancel := context.WithCancel(t.Context())
		first := make(chan error, 1)
		go func() { first <- c.ensureStarted(ctx) }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("catalog not entered")
		}
		var forwarded atomic.Bool
		second := make(chan error, 1)
		handler := c.lazyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Store(true) }))
		go func() {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/lazy/mcp", nil))
			second <- nil
		}()
		cancel()
		if err := awaitLazy(t, first); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel = %v", err)
		}
		if forwarded.Load() {
			t.Fatal("forwarded before required catalog")
		}
		close(release)
		if err := awaitLazy(t, second); err != nil {
			t.Fatal(err)
		}
		if !forwarded.Load() {
			t.Fatal("remaining caller not forwarded")
		}
		if c.Health() != healthOK {
			t.Fatal("shared startup canceled")
		}
	})
	t.Run("failed catalog closes transport then next request retries", func(t *testing.T) {
		d := newRawDownstream(t)
		d.setTools("alpha")
		d.hangToolsListCalls(true)
		c := lazyTestClient(t, d, false)
		c.options.ReconnectInterval = Duration(300 * time.Millisecond)
		h := c.lazyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/lazy/mcp", nil))
		if rec.Code != 503 || c.getClient() != nil {
			t.Fatalf("failure=%d raw=%v", rec.Code, c.getClient())
		}
		d.hangToolsListCalls(false)
		d.mu.Lock()
		callsBefore := d.toolsListCalls
		d.mu.Unlock()
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/lazy/mcp", nil))
		d.mu.Lock()
		callsAfter := d.toolsListCalls
		d.mu.Unlock()
		if rec.Code != 503 || callsAfter != callsBefore {
			t.Fatalf("cooldown status=%d catalog calls=%d -> %d", rec.Code, callsBefore, callsAfter)
		}
		c.lazy.mu.Lock()
		retryAt := c.lazy.retryAt
		c.lazy.mu.Unlock()
		time.Sleep(time.Until(retryAt))
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/lazy/mcp", nil))
		if rec.Code != 204 {
			t.Fatalf("same route retry status=%d", rec.Code)
		}
	})
	t.Run("activation enables background initial retry", func(t *testing.T) {
		d := newRawDownstream(t)
		d.hangToolsListCalls(true)
		c := lazyTestClient(t, d, true)
		if err := c.ensureStarted(t.Context()); err == nil {
			t.Fatal("expected catalog timeout")
		}
		d.hangToolsListCalls(false)
		deadline := time.After(2 * time.Second)
		for c.Health() != healthOK {
			select {
			case <-deadline:
				t.Fatal("background retry did not recover")
			case <-time.After(10 * time.Millisecond):
			}
		}
	})
	t.Run("shutdown during startup", func(t *testing.T) {
		d := newRawDownstream(t)
		entered, _ := gateLazyTools(t, d)
		c := lazyTestClient(t, d, false)
		waiting := make(chan error, 1)
		go func() { waiting <- c.ensureStarted(t.Context()) }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("catalog not entered")
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if err := awaitLazy(t, waiting); err == nil {
			t.Fatal("shutdown allowed successful waiter")
		}
		if err := c.ensureStarted(t.Context()); err == nil {
			t.Fatal("closed client restarted")
		}
		if c.getClient() != nil {
			t.Fatal("shutdown left raw published")
		}
	})
}
