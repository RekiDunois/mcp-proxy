package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// lazyBackend owns only activation, not MCP forwarding or application state.
// Each attempt has its own immutable outcome after done closes, so a waiter
// cannot accidentally consume a later retry's result.
var errCleanupUncertain = errors.New("backend cleanup incomplete; restart gateway after resolving the old process")

type lazyAttempt struct {
	done chan struct{}
	err  error
}

type lazyBackend struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	attempt   *lazyAttempt
	latest    *lazyAttempt // invalidates an older failure timer when a new attempt starts
	ready     bool
	everReady bool
	lastErr   error
	retryAt   time.Time
}

// bindLazy is called once before publishing the HTTP route.
func (c *Client) bindLazy(ctx context.Context, info mcp.Implementation, s *server.MCPServer) {
	c.lazy.ctx, c.lazy.cancel = context.WithCancel(ctx)
	c.clientInfo, c.mcpServer = info, s
}

func (l *lazyBackend) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancel != nil {
		l.cancel()
	}
}

func (l *lazyBackend) startLocked(c *Client) *lazyAttempt {
	a := &lazyAttempt{done: make(chan struct{})}
	l.attempt = a
	l.latest = a
	l.ready = false
	go func() {
		err := c.connect(l.ctx, c.clientInfo, c.mcpServer)
		l.mu.Lock()
		if c.closed.Load() || l.ctx.Err() != nil {
			err = errors.Join(err, errors.New("client is closed"))
		}
		a.err = err
		l.lastErr = err
		l.attempt = nil
		if err == nil {
			l.ready, l.everReady = true, true
			if c.needPing {
				c.pingOnce.Do(func() { go c.startPingTask(l.ctx) })
			}
		} else {
			c.health.Store(int32(healthFailed))
			l.retryAt = time.Now().Add(c.options.reconnectInterval())
			slog.Warn("Lazy backend activation failed", "client", c.name, "err", redactURLCredentials(err))
		}
		retry := err != nil && !l.everReady && c.options.autoReconnect() && !permanentStartupError(err) && !errors.Is(err, errCleanupUncertain) && !c.closed.Load() && l.ctx.Err() == nil
		close(a.done)
		l.mu.Unlock()
		if retry && retryWait(l.ctx, c.options.reconnectInterval()) {
			l.mu.Lock()
			if l.attempt == nil && l.latest == a && !time.Now().Before(l.retryAt) && !l.everReady && !c.closed.Load() && l.ctx.Err() == nil {
				l.startLocked(c)
			}
			l.mu.Unlock()
		}
	}()
	return a
}

func waitLazy(ctx, lifetime context.Context, a *lazyAttempt) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-lifetime.Done():
		return lifetime.Err()
	case <-a.done:
		return a.err
	}
}

func (c *Client) ensureStarted(ctx context.Context) error {
	l := c.lazy
	l.mu.Lock()
	if c.closed.Load() || l.ctx.Err() != nil {
		l.mu.Unlock()
		return errors.New("client is closed")
	}
	if l.ready {
		l.mu.Unlock()
		return nil
	}
	a := l.attempt
	if a == nil {
		if l.everReady || time.Now().Before(l.retryAt) {
			err := l.lastErr
			l.mu.Unlock()
			return err
		}
		a = l.startLocked(c)
	}
	l.mu.Unlock()
	return waitLazy(ctx, l.ctx, a)
}

func (l *lazyBackend) reconnect(c *Client, ctx context.Context) error {
	l.mu.Lock()
	if c.closed.Load() || l.ctx.Err() != nil {
		l.mu.Unlock()
		return errors.New("client is closed")
	}
	a := l.attempt
	if a == nil {
		a = l.startLocked(c)
	}
	l.mu.Unlock()
	return waitLazy(ctx, l.ctx, a)
}

func (c *Client) lazyHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := c.ensureStarted(r.Context()); err != nil {
			if r.Context().Err() == nil {
				http.Error(w, "Backend unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}
