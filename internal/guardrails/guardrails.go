package guardrails

import (
	"context"
	"sync"
	"time"
)

type Limits struct{ RatePerMinute, Concurrency int }

type Controller struct {
	mu     sync.Mutex
	states map[string]*state
}

type state struct {
	limits      Limits
	windowStart time.Time
	windowCount int
	active      int
}

func New() *Controller { return &Controller{states: make(map[string]*state)} }

func (c *Controller) Acquire(ctx context.Context, key string, limits Limits) (func(), bool) {
	for {
		c.mu.Lock()
		s := c.states[key]
		if s == nil {
			s = &state{limits: limits, windowStart: time.Now()}
			c.states[key] = s
		}
		if s.limits != limits {
			s.limits = limits
		}
		now := time.Now()
		if now.Sub(s.windowStart) >= time.Minute {
			s.windowStart, s.windowCount = now, 0
		}
		if s.active < limits.Concurrency && s.windowCount < limits.RatePerMinute {
			s.active++
			s.windowCount++
			c.mu.Unlock()
			return func() { c.release(key) }, true
		}
		wait := time.Until(s.windowStart.Add(time.Minute))
		if s.active >= limits.Concurrency {
			wait = 50 * time.Millisecond
		}
		if wait < 10*time.Millisecond {
			wait = 10 * time.Millisecond
		}
		c.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, false
		case <-timer.C:
		}
	}
}

func (c *Controller) release(key string) {
	c.mu.Lock()
	if s := c.states[key]; s != nil && s.active > 0 {
		s.active--
	}
	c.mu.Unlock()
}
