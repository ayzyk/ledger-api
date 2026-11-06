// Package ledger keeps account balances using double-entry bookkeeping.
// Amounts are always integers in minor units (kopecks, cents).
package ledger

import (
	"errors"
	"time"
)

var (
	ErrNotFound            = errors.New("account not found")
	ErrInsufficientFunds   = errors.New("insufficient funds")
	ErrCurrencyMismatch    = errors.New("accounts have different currencies")
	ErrSameAccount         = errors.New("cannot transfer to the same account")
	ErrInvalidAmount       = errors.New("amount must be positive")
	ErrInvalidCurrency     = errors.New("unsupported currency")
	ErrInvalidOwner        = errors.New("owner is required")
	ErrSystemAccount       = errors.New("operation not allowed on system account")
	ErrIdempotencyMismatch = errors.New("idempotency key was already used with other parameters")
)

type Account struct {
	ID        int64     `json:"id"`
	Owner     string    `json:"owner"`
	Currency  string    `json:"currency"`
	Balance   int64     `json:"balance"`
	CreatedAt time.Time `json:"created_at"`
}

type Transfer struct {
	ID            int64     `json:"id"`
	FromAccountID int64     `json:"from_account_id"`
	ToAccountID   int64     `json:"to_account_id"`
	Amount        int64     `json:"amount"`
	CreatedAt     time.Time `json:"created_at"`
	// Replayed is true when the key was seen before and nothing was moved this time.
	Replayed bool `json:"replayed"`
}

type Entry struct {
	ID         int64     `json:"id"`
	TransferID int64     `json:"transfer_id"`
	Amount     int64     `json:"amount"`
	CreatedAt  time.Time `json:"created_at"`
}

type Mismatch struct {
	AccountID  int64 `json:"account_id"`
	Balance    int64 `json:"balance"`
	EntriesSum int64 `json:"entries_sum"`
}

// Report is the result of a reconciliation run.
// Totals are per currency and have to be zero because money only moves between accounts.
type Report struct {
	Mismatches []Mismatch       `json:"mismatches"`
	Totals     map[string]int64 `json:"totals"`
}

func (r Report) OK() bool {
	if len(r.Mismatches) > 0 {
		return false
	}
	for _, total := range r.Totals {
		if total != 0 {
			return false
		}
	}
	return true
}
