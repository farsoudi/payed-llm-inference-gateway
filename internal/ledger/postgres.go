package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/farsoudi/payed-llm-inference/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Postgres struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) CreateUser(ctx context.Context, keyHash, label string, rate, concurrency int) (domain.User, error) {
	var u domain.User
	err := p.pool.QueryRow(ctx, `
		INSERT INTO users (key_hash, label, rate_limit_per_minute, concurrency_limit)
		VALUES ($1, $2, $3, $4)
		RETURNING id, label, balance_micro_usdc, rate_limit_per_minute, concurrency_limit, revoked, created_at`,
		keyHash, label, rate, concurrency).Scan(&u.ID, &u.Label, &u.BalanceMicroUSDC, &u.RateLimitPerMin, &u.ConcurrencyLimit, &u.Revoked, &u.CreatedAt)
	if err != nil {
		return domain.User{}, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

func (p *Postgres) GetUser(ctx context.Context, keyHash string) (domain.User, error) {
	var u domain.User
	err := p.pool.QueryRow(ctx, `SELECT id, label, balance_micro_usdc, rate_limit_per_minute, concurrency_limit, revoked, created_at FROM users WHERE key_hash = $1`, keyHash).
		Scan(&u.ID, &u.Label, &u.BalanceMicroUSDC, &u.RateLimitPerMin, &u.ConcurrencyLimit, &u.Revoked, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, ErrNotFound
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("get user: %w", err)
	}
	if u.Revoked {
		return u, ErrRevoked
	}
	return u, nil
}

func (p *Postgres) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, label, balance_micro_usdc, rate_limit_per_minute, concurrency_limit, revoked, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	users := make([]domain.User, 0)
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(&u.ID, &u.Label, &u.BalanceMicroUSDC, &u.RateLimitPerMin, &u.ConcurrencyLimit, &u.Revoked, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("decode users: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return users, nil
}

func (p *Postgres) DeleteUser(ctx context.Context, keyHash string) error {
	result, err := p.pool.Exec(ctx, `UPDATE users SET revoked = true, revoked_at = now() WHERE key_hash = $1 AND revoked = false`, keyHash)
	if err != nil {
		return fmt.Errorf("revoke user: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) SetLimits(ctx context.Context, keyHash string, rate, concurrency int) error {
	result, err := p.pool.Exec(ctx, `UPDATE users SET rate_limit_per_minute = $2, concurrency_limit = $3 WHERE key_hash = $1 AND revoked = false`, keyHash, rate, concurrency)
	if err != nil {
		return fmt.Errorf("set limits: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) CreditTopUp(ctx context.Context, topup domain.TopUp) (balance int64, inserted bool, err error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, false, fmt.Errorf("begin top-up: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		} else {
			err = tx.Commit(ctx)
		}
	}()
	var topupID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO topups (key_hash, amount_micro_usdc, transaction_hash, payer, network)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (transaction_hash) DO NOTHING
		RETURNING id`, topup.KeyHash, topup.Amount, topup.Transaction, topup.Payer, topup.Network).Scan(&topupID)
	if errors.Is(err, pgx.ErrNoRows) {
		var existingKey, existingPayer, existingNetwork string
		var existingAmount int64
		err = tx.QueryRow(ctx, `SELECT key_hash, amount_micro_usdc, payer, network FROM topups WHERE transaction_hash = $1`, topup.Transaction).
			Scan(&existingKey, &existingAmount, &existingPayer, &existingNetwork)
		if err != nil {
			return 0, false, fmt.Errorf("inspect duplicate top-up: %w", err)
		}
		if existingKey != topup.KeyHash || existingAmount != topup.Amount || existingPayer != topup.Payer || existingNetwork != topup.Network {
			return 0, false, ErrConflict
		}
		err = tx.QueryRow(ctx, `SELECT balance_micro_usdc FROM users WHERE key_hash = $1`, topup.KeyHash).Scan(&balance)
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return balance, false, err
	}
	if err != nil {
		return 0, false, fmt.Errorf("record top-up: %w", err)
	}
	err = tx.QueryRow(ctx, `UPDATE users SET balance_micro_usdc = balance_micro_usdc + $2 WHERE key_hash = $1 AND revoked = false RETURNING balance_micro_usdc`, topup.KeyHash, topup.Amount).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, ErrNotFound
	}
	if err != nil {
		return 0, false, fmt.Errorf("credit balance: %w", err)
	}
	return balance, true, nil
}

func (p *Postgres) Debit(ctx context.Context, debit domain.Debit) (balance int64, err error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin debit: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		} else {
			err = tx.Commit(ctx)
		}
	}()
	err = tx.QueryRow(ctx, `
		UPDATE users SET balance_micro_usdc = balance_micro_usdc - $2
		WHERE key_hash = $1 AND revoked = false AND balance_micro_usdc >= $2
		RETURNING balance_micro_usdc`, debit.KeyHash, debit.CostMicroUSDC).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		var revoked bool
		checkErr := tx.QueryRow(ctx, `SELECT revoked FROM users WHERE key_hash = $1`, debit.KeyHash).Scan(&revoked)
		if errors.Is(checkErr, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		if checkErr != nil {
			return 0, checkErr
		}
		if revoked {
			return 0, ErrRevoked
		}
		return 0, ErrInsufficientFunds
	}
	if err != nil {
		return 0, fmt.Errorf("debit balance: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO request_ledger (key_hash, prompt_tokens, completion_tokens, cost_micro_usdc, latency_ms, partial, balance_after_micro_usdc) VALUES ($1, $2, $3, $4, $5, $6, $7)`, debit.KeyHash, debit.PromptTokens, debit.CompletionTokens, debit.CostMicroUSDC, debit.Latency.Milliseconds(), debit.Partial, balance)
	if err != nil {
		return 0, fmt.Errorf("record debit: %w", err)
	}
	return balance, nil
}

// Compile time enforcement that Postgres implements Store interface
var _ Store = (*Postgres)(nil)
