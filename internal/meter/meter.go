package meter

import (
	"context"
	"sync"
	"time"

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
	store ledger.Store
	cache *BalanceCache
	price int64
	locks sync.Map
}

func New(store ledger.Store, pricePerTokenMicro int64) *Meter {
	return &Meter{store: store, cache: NewBalanceCache(), price: pricePerTokenMicro}
}

func (m *Meter) accountLock(keyHash string) *sync.Mutex {
	lock, _ := m.locks.LoadOrStore(keyHash, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (m *Meter) CreditTopUp(ctx context.Context, topup domain.TopUp) (int64, bool, error) {
	lock := m.accountLock(topup.KeyHash)
	lock.Lock()
	defer lock.Unlock()
	balance, inserted, err := m.store.CreditTopUp(ctx, topup)
	if err == nil {
		m.cache.Set(topup.KeyHash, balance)
	}
	return balance, inserted, err
}

func (m *Meter) PreflightUser(keyHash string, u domain.User) error {
	if m.cache.Get(keyHash, u.BalanceMicroUSDC) <= 0 {
		return ledger.ErrInsufficientFunds
	}
	return nil
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
	reservation, err := money.MultiplyTokens(maxTokens, m.price)
	if err != nil || !m.cache.Reserve(keyHash, initialBalance, reservation) {
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
		t.meter.cache.Release(t.keyHash, t.reserved)
	}
}

func (t *Tracker) Finalize(ctx context.Context, usage ollama.Event, partial, billInput bool) (int64, int64, error) {
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
	cost, err := money.MultiplyTokens(billedTokens, t.meter.price)
	if err != nil {
		t.meter.cache.Release(t.keyHash, t.reserved)
		return 0, 0, err
	}
	lock := t.meter.accountLock(t.keyHash)
	lock.Lock()
	balance, err := t.meter.store.Debit(ctx, domain.Debit{KeyHash: t.keyHash, PromptTokens: prompt, CompletionTokens: completion, CostMicroUSDC: cost, Latency: time.Since(t.started), Partial: partial})
	if err != nil {
		lock.Unlock()
		t.meter.cache.Release(t.keyHash, t.reserved)
		return 0, cost, err
	}
	t.meter.cache.AfterDebit(t.keyHash, balance, t.reserved)
	lock.Unlock()
	return balance, cost, nil
}

func estimateTokens(text string) int {
	// Fallback for streams that end without Ollama's final usage: ~4 chars/token.
	return (len([]rune(text)) + 3) / 4
}
