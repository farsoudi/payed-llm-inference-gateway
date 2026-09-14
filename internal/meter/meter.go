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

// BalanceCache tracks the durable balance and maximum spend reserved locally
// by active requests. This prevents concurrent requests from all spending the
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

func (c *BalanceCache) Set(key string, balance int64) {
	c.mu.Lock()
	entry := c.values[key]
	entry.actual = balance
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
	// The durable result is authoritative; callers serialize it per account.
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
	locks  sync.Map
}

func (m *Meter) accountLock(keyHash string) *sync.Mutex {
	lock, _ := m.locks.LoadOrStore(keyHash, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (m *Meter) CreditTopUp(ctx context.Context, topup domain.TopUp) (int64, bool, error) {
	lock := m.accountLock(topup.KeyHash)
	lock.Lock()
	defer lock.Unlock()
	balance, inserted, err := m.Store.CreditTopUp(ctx, topup)
	if err == nil {
		m.Cache.Set(topup.KeyHash, balance)
	}
	return balance, inserted, err
}

func (m *Meter) PreflightUser(keyHash string, u domain.User) (int64, error) {
	balance := m.Cache.Get(keyHash, u.BalanceMicroUSDC)
	if balance <= 0 {
		return 0, ledger.ErrInsufficientFunds
	}
	return balance, nil
}

type Tracker struct {
	meter     *Meter
	keyHash   string
	started   time.Time
	estimated int
	reserved  int64
	finished  bool
}

func (m *Meter) Tracker(keyHash string, initialBalance int64, started time.Time, maxTokens int) (*Tracker, error) {
	reservation, err := money.MultiplyTokens(maxTokens, m.Config.PricePerTokenMicro)
	if err != nil || !m.Cache.Reserve(keyHash, initialBalance, reservation) {
		return nil, ledger.ErrInsufficientFunds
	}
	return &Tracker{meter: m, keyHash: keyHash, started: started, reserved: reservation}, nil
}

func (t *Tracker) Observe(event ollama.Event) {
	text := event.Text
	if text != "" {
		t.estimated += estimateTokens(text)
	}
}

func (t *Tracker) Cancel() {
	if !t.finished {
		t.finished = true
		t.meter.Cache.Release(t.keyHash, t.reserved)
	}
}

func (t *Tracker) Finalize(ctx context.Context, usage ollama.Event, partial bool) (int64, int64, error) {
	return t.finalize(ctx, usage, partial, false)
}

func (t *Tracker) FinalizeInput(ctx context.Context, usage ollama.Event, partial bool) (int64, int64, error) {
	return t.finalize(ctx, usage, partial, true)
}

func (t *Tracker) finalize(ctx context.Context, usage ollama.Event, partial, billInput bool) (int64, int64, error) {
	if t.finished {
		return 0, 0, nil
	}
	t.finished = true
	completion := usage.EvalCount
	prompt := usage.PromptEvalCount
	if completion <= 0 {
		completion = t.estimated
	}
	if prompt < 0 {
		prompt = 0
	}
	billedTokens := completion
	if billInput {
		billedTokens = prompt
	}
	// Generation is priced by output. Embedding requests opt into input pricing
	// because they produce vectors rather than generated text.
	cost, err := money.MultiplyTokens(billedTokens, t.meter.Config.PricePerTokenMicro)
	if err != nil {
		t.meter.Cache.Release(t.keyHash, t.reserved)
		return 0, 0, err
	}
	lock := t.meter.accountLock(t.keyHash)
	lock.Lock()
	balance, err := t.meter.Store.Debit(ctx, domain.Debit{KeyHash: t.keyHash, PromptTokens: prompt, CompletionTokens: completion, CostMicroUSDC: cost, Latency: time.Since(t.started), Partial: partial})
	if err != nil {
		lock.Unlock()
		t.meter.Cache.Release(t.keyHash, t.reserved)
		return 0, cost, err
	}
	t.meter.Cache.AfterDebit(t.keyHash, balance, t.reserved)
	lock.Unlock()
	return balance, cost, nil
}

func estimateTokens(text string) int {
	// This is only a fallback for streams that end without Ollama's final usage.
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
