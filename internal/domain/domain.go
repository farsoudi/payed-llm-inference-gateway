package domain

import "time"

const MicroUSDCPerUSDC int64 = 1_000_000

type User struct {
	ID               int64     `json:"id"`
	Label            string    `json:"label"`
	BalanceMicroUSDC int64     `json:"balance_micro_usdc"`
	RateLimitPerMin  int       `json:"rate_limit_per_minute"`
	ConcurrencyLimit int       `json:"concurrency_limit"`
	Revoked          bool      `json:"revoked"`
	CreatedAt        time.Time `json:"created_at"`
}

type TopUp struct {
	KeyHash     string
	Amount      int64
	Transaction string
	Payer       string
	Network     string
}

type Debit struct {
	KeyHash          string
	PromptTokens     int
	CompletionTokens int
	CostMicroUSDC    int64
	Latency          time.Duration
	Partial          bool
}
