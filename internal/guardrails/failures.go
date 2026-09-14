package guardrails

import (
	"sync"
	"time"
)

type failureWindow struct {
	started time.Time
	count   int
}

// FailureMonitor provides a lightweight signal for repeated malformed or
// unauthenticated requests. It is intentionally process-local like the other
// guardrails; operators can ship structured logs to their alerting system.
type FailureMonitor struct {
	mu      sync.Mutex
	windows map[string]failureWindow
}

func NewFailureMonitor() *FailureMonitor {
	return &FailureMonitor{windows: make(map[string]failureWindow)}
}

func (m *FailureMonitor) Record(identity string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	window := m.windows[identity]
	if window.started.IsZero() || now.Sub(window.started) >= time.Minute {
		window = failureWindow{started: now}
	}
	window.count++
	m.windows[identity] = window
	return window.count
}
