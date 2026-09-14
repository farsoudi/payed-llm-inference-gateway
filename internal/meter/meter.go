package meter

import (
	"context"
	"sync"
	"time"

	"github.com/farsoudi/payed-llm-inference/internal/config"
	"github.com/farsoudi/payed-llm-inference/internal/domain"
	"github.com/farsoudi/payed-llm-inference/internal/ledger"
	"github.com/farsoudi/payed-llm-inference/internal/money"
	"github.com/farsoudi/payed-llm-inference/internal/ollama"
)

type balanceValue struct{ actual, reserved int64 }

// BalanceCache tracks the durable balance and local estimated spend reserved
// by active streams. This prevents concurrent streams from all spending the
// same visible balance while keeping Postgres authoritative for final billing.
type BalanceCache struct {
	mu     sync.Mutex
	values map[string]balanceValue
}

func NewBalanceCache() *BalanceCache { return &BalanceCache{values: make(map[string]balanceValue)} }

func (c *BalanceCache) Get(key string, fallback int64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.values[key]
	if !ok {
		entry.actual = fallback
		c.values[key] = entry
	}
	return entry.actual - entry.reserved
}

func (c *BalanceCache) Set(key string, value int64) {
	c.mu.Lock()
	entry := c.values[key]
	entry.actual = value
	c.values[key] = entry
	c.mu.Unlock()
}

func (c *BalanceCache) Reserve(key string, fallback, amount int64) bool {
	if amount <= 0 {
		return true
	}
	c.mu.Lock()
	entry, ok := c.values[key]
	if !ok {
		entry.actual = fallback
	}
	if entry.actual-entry.reserved < amount {
		c.mu.Unlock()
		return false
	}
	entry.reserved += amount
	c.values[key] = entry
	c.mu.Unlock()
	return true
}

func (c *BalanceCache) AfterDebit(key string, balance, reservation int64) {
	c.mu.Lock()
	entry := c.values[key]
	entry.actual = balance
	entry.reserved -= reservation
	if entry.reserved < 0 {
		entry.reserved = 0
	}
	c.values[key] = entry
	c.mu.Unlock()
}

func (c *BalanceCache) Release(key string, reservation int64) {
	c.mu.Lock()
	entry := c.values[key]
	entry.reserved -= reservation
	if entry.reserved < 0 {
		entry.reserved = 0
	}
	c.values[key] = entry
	c.mu.Unlock()
}

type Meter struct {
	Store  ledger.Store
	Cache  *BalanceCache
	Config config.Config
}

func (m *Meter) Preflight(ctx context.Context, keyHash string) (domain.User, int64, error) {
	u, err := m.Store.GetUser(ctx, keyHash)
	if err != nil {
		return domain.User{}, 0, err
	}
	balance := m.Cache.Get(keyHash, u.BalanceMicroUSDC)
	if balance <= 0 {
		return domain.User{}, 0, ledger.ErrInsufficientFunds
	}
	return u, balance, nil
}

type Tracker struct {
	meter     *Meter
	keyHash   string
	balance   int64
	started   time.Time
	lastCheck int
	estimated int
	reserved  int64
	stopped   bool
}

func (m *Meter) Tracker(keyHash string, initialBalance int64, started time.Time) *Tracker {
	return &Tracker{meter: m, keyHash: keyHash, balance: initialBalance, started: started}
}

func (t *Tracker) Observe(event ollama.Event) error {
	text := event.Response + event.Message.Content
	if text != "" {
		t.estimated += estimateTokens(text)
	}
	if !event.Done && t.estimated-t.lastCheck >= t.meter.Config.CheckpointTokens {
		t.lastCheck = t.estimated
		balance := t.meter.Cache.Get(t.keyHash, t.balance)
		cost, _ := money.MultiplyTokens(t.estimated, t.meter.Config.PricePerTokenMicro)
		delta := cost - t.reserved
		if delta > 0 && !t.meter.Cache.Reserve(t.keyHash, balance, delta) {
			t.stopped = true
			return ollama.ErrStopped
		}
		if delta > 0 {
			t.reserved = cost
		}
		remaining := t.meter.Cache.Get(t.keyHash, balance)
		rate := float64(t.estimated) / time.Since(t.started).Seconds()
		if rate <= 0 {
			rate = 1
		}
		leadCost, _ := money.MultiplyTokens(int(rate*t.meter.Config.ReloadLead.Seconds()), t.meter.Config.PricePerTokenMicro)
		if remaining <= leadCost {
			t.stopped = true
			return ollama.ErrStopped
		}
	}
	return nil
}

func (t *Tracker) Partial() bool { return t.stopped }

func (t *Tracker) Finalize(ctx context.Context, usage ollama.Event, partial bool) (int64, int64, error) {
	completion := usage.EvalCount
	prompt := usage.PromptEvalCount
	if completion <= 0 {
		completion = t.estimated
	}
	if prompt < 0 {
		prompt = 0
	}
	// The configured price is for generated output. Prompt tokens remain in the
	// ledger for auditability but do not consume the prepaid balance.
	cost, err := money.MultiplyTokens(completion, t.meter.Config.PricePerTokenMicro)
	if err != nil {
		t.meter.Cache.Release(t.keyHash, t.reserved)
		return 0, 0, err
	}
	balance, err := t.meter.Store.Debit(ctx, domain.Debit{KeyHash: t.keyHash, PromptTokens: prompt, CompletionTokens: completion, CostMicroUSDC: cost, Latency: time.Since(t.started), Partial: partial})
	if err != nil {
		t.meter.Cache.Release(t.keyHash, t.reserved)
		return 0, cost, err
	}
	t.meter.Cache.AfterDebit(t.keyHash, balance, t.reserved)
	return balance, cost, nil
}

func (m *Meter) Credit(keyHash string, balance int64) { m.Cache.Set(keyHash, balance) }

func estimateTokens(text string) int {
	// This is only a checkpoint estimate. Ollama's final eval_count is used for billing.
	runes := len([]rune(text))
	if runes < 1 {
		return 0
	}
	n := (runes + 3) / 4
	if n < 1 {
		return 1
	}
	return n
}
