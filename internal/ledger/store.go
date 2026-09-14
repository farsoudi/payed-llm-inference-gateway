package ledger

import (
	"context"
	"errors"

	"github.com/farsoudi/payed-llm-inference/internal/domain"
)

var (
	ErrNotFound          = errors.New("user not found")
	ErrInsufficientFunds = errors.New("insufficient balance")
	ErrRevoked           = errors.New("user revoked")
	ErrConflict          = errors.New("top-up transaction conflicts with an existing record")
)

type Store interface {
	CreateUser(context.Context, string, string, int, int) (domain.User, error)
	GetUser(context.Context, string) (domain.User, error)
	ListUsers(context.Context) ([]domain.User, error)
	DeleteUser(context.Context, string) error
	SetLimits(context.Context, string, int, int) error
	CreditTopUp(context.Context, domain.TopUp) (int64, bool, error)
	Debit(context.Context, domain.Debit) (int64, error)
	Close()
}
