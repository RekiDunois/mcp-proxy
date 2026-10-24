package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// acquireIdle pins the lazy transport before resolving it. HTTP SSE dispatch
// can outlive its POST, so forwarded calls take their own lease as well.
func (c *Client) acquireIdle(ctx context.Context) (func(), error) {
	l := c.lazy
	if l == nil || l.idleTimeout == 0 {
		return func() {}, nil
	}
	l.mu.Lock()
	l.active++
	if l.idleTimer != nil {
		l.idleTimer.Stop()
	}
	l.mu.Unlock()
	release := func() {
		l.mu.Lock()
		l.active--
		l.idleDeadline = time.Now().Add(l.idleTimeout)
		l.scheduleIdleLocked(c)
		l.mu.Unlock()
	}
	if err := c.ensureStarted(ctx); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// scheduleIdleLocked is also called after activation/probing. A timer which
// fired during either operation must not leave the backend resident forever.
func (l *lazyBackend) scheduleIdleLocked(c *Client) {
	if l.idleTimer != nil {
		l.idleTimer.Stop()
	}
	if l.idleTimeout == 0 || !l.ready || l.active != 0 || l.probing || l.closing != nil || c.closed.Load() || l.ctx.Err() != nil || c.Health() != healthOK {
		return
	}
	deadline := l.idleDeadline
	l.idleTimer = time.AfterFunc(time.Until(deadline), func() { c.expireIdle(deadline) })
}

func (c *Client) expireIdle(deadline time.Time) {
	l := c.lazy
	l.mu.Lock()
	if !l.idleDeadline.Equal(deadline) || time.Now().Before(deadline) || !l.ready || l.active != 0 || l.probing || l.closing != nil || c.closed.Load() || l.ctx.Err() != nil || c.Health() != healthOK {
		l.mu.Unlock()
		return
	}
	done := make(chan struct{})
	l.closing, l.ready = done, false
	l.mu.Unlock()

	// Never hold lazy.mu while waiting for connectMu: activation completes in
	// the opposite order. Closing is published first so all new callers wait.
	c.connectMu.Lock()
	var err error
	if raw := c.getClient(); raw != nil {
		err = closeTransport(raw)
		if err == nil {
			c.forget(raw)
		} else {
			c.cleanupErr = fmt.Errorf("%w: %v", errCleanupUncertain, err)
			err = c.cleanupErr
		}
	}
	c.connectMu.Unlock()

	l.mu.Lock()
	if err == nil {
		l.everReady, l.lastErr, l.latest, l.retryAt = false, nil, nil, time.Time{}
		c.health.Store(int32(healthUnknown))
	} else {
		// Retain ownership and the previous epoch: no request may spawn a
		// replacement after teardown could not confirm that the old one stopped.
		l.lastErr = err
		c.health.Store(int32(healthFailed))
		slog.Warn("Idle backend cleanup failed", "client", c.name, "err", redactURLCredentials(err))
	}
	l.closing = nil
	close(done)
	l.mu.Unlock()
}

// A probe pins its transport through its health/reconnect decision but does
// not count as user activity or extend the user's idle deadline.
func (c *Client) acquireIdleProbe() (func(), bool) {
	l := c.lazy
	if l == nil || l.idleTimeout == 0 {
		return func() {}, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.everReady || l.attempt != nil || l.closing != nil || l.active > 0 || c.closed.Load() || l.ctx.Err() != nil {
		return nil, false
	}
	l.probing = true
	return func() {
		l.mu.Lock()
		l.probing = false
		l.scheduleIdleLocked(c)
		l.mu.Unlock()
	}, true
}

func (c *Client) idleBusinessActive() bool {
	l := c.lazy
	if l == nil || l.idleTimeout == 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active > 0
}
