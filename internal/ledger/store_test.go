package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ayzyk/ledger-api/internal/ledger"
	"github.com/ayzyk/ledger-api/migrations"
)

var keyCounter atomic.Int64

// keys have to be unique between test runs, the database is not cleaned
func newKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d-%d", t.Name(), os.Getpid(), keyCounter.Add(1))
}

func newStore(t *testing.T) *ledger.Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrations.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return ledger.NewStore(pool)
}

func newAccount(t *testing.T, s *ledger.Store, balance int64) ledger.Account {
	t.Helper()
	ctx := context.Background()
	a, err := s.CreateAccount(ctx, "test", "RUB")
	if err != nil {
		t.Fatal(err)
	}
	if balance > 0 {
		if _, err := s.Deposit(ctx, newKey(t), a.ID, balance); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func assertBalance(t *testing.T, s *ledger.Store, id, want int64) {
	t.Helper()
	a, err := s.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Balance != want {
		t.Errorf("account %d: balance = %d, want %d", id, a.Balance, want)
	}
}

func assertReconciled(t *testing.T, s *ledger.Store) {
	t.Helper()
	report, err := s.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Errorf("reconciliation failed: %+v", report)
	}
}

func TestTransfer(t *testing.T) {
	s := newStore(t)
	from, to := newAccount(t, s, 1000), newAccount(t, s, 0)

	got, err := s.Transfer(context.Background(), newKey(t), from.ID, to.ID, 300)
	if err != nil {
		t.Fatal(err)
	}
	if got.Replayed {
		t.Error("first transfer must not be a replay")
	}
	assertBalance(t, s, from.ID, 700)
	assertBalance(t, s, to.ID, 300)
	assertReconciled(t, s)
}

func TestTransferErrors(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	rub := newAccount(t, s, 100)
	other := newAccount(t, s, 0)
	usd, err := s.CreateAccount(ctx, "test", "USD")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		from   int64
		to     int64
		amount int64
		want   error
	}{
		{"not enough money", rub.ID, other.ID, 101, ledger.ErrInsufficientFunds},
		{"zero amount", rub.ID, other.ID, 0, ledger.ErrInvalidAmount},
		{"negative amount", rub.ID, other.ID, -5, ledger.ErrInvalidAmount},
		{"same account", rub.ID, rub.ID, 1, ledger.ErrSameAccount},
		{"other currency", rub.ID, usd.ID, 1, ledger.ErrCurrencyMismatch},
		{"unknown account", rub.ID, 999999999, 1, ledger.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.Transfer(ctx, newKey(t), tt.from, tt.to, tt.amount)
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
	assertBalance(t, s, rub.ID, 100)
	assertBalance(t, s, other.ID, 0)
}

func TestFailedTransferDoesNotBurnKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	from, to := newAccount(t, s, 50), newAccount(t, s, 0)
	key := newKey(t)

	if _, err := s.Transfer(ctx, key, from.ID, to.ID, 100); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Deposit(ctx, newKey(t), from.ID, 50); err != nil {
		t.Fatal(err)
	}
	got, err := s.Transfer(ctx, key, from.ID, to.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got.Replayed {
		t.Error("retry after a failure should do the transfer")
	}
	assertBalance(t, s, to.ID, 100)
}

func TestIdempotentReplay(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	from, to := newAccount(t, s, 100), newAccount(t, s, 0)
	key := newKey(t)

	first, err := s.Transfer(ctx, key, from.ID, to.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	// the account is empty now, but a replay must still succeed
	second, err := s.Transfer(ctx, key, from.ID, to.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.ID != first.ID {
		t.Errorf("second = %+v, want replay of transfer %d", second, first.ID)
	}
	assertBalance(t, s, to.ID, 100)
}

func TestIdempotencyKeyWithOtherParams(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	from, to := newAccount(t, s, 100), newAccount(t, s, 0)
	key := newKey(t)

	if _, err := s.Transfer(ctx, key, from.ID, to.ID, 10); err != nil {
		t.Fatal(err)
	}
	_, err := s.Transfer(ctx, key, from.ID, to.ID, 20)
	if !errors.Is(err, ledger.ErrIdempotencyMismatch) {
		t.Errorf("err = %v, want %v", err, ledger.ErrIdempotencyMismatch)
	}
	assertBalance(t, s, to.ID, 10)
}

func TestSystemAccountIsNotAvailable(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	user := newAccount(t, s, 10)

	// system accounts are created first by the migration
	for id := int64(1); id <= 3; id++ {
		_, err := s.Transfer(ctx, newKey(t), id, user.ID, 1)
		if !errors.Is(err, ledger.ErrSystemAccount) && !errors.Is(err, ledger.ErrCurrencyMismatch) {
			t.Errorf("from system %d: err = %v", id, err)
		}
	}
	assertBalance(t, s, user.ID, 10)
}

// Exactly ten withdrawals of 100 fit in a balance of 1000.
func TestConcurrentWithdrawalsDoNotOverdraw(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	from, to := newAccount(t, s, 1000), newAccount(t, s, 0)

	var ok, rejected atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		key := newKey(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Transfer(ctx, key, from.ID, to.ID, 100)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ledger.ErrInsufficientFunds):
				rejected.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if ok.Load() != 10 || rejected.Load() != 20 {
		t.Errorf("ok = %d, rejected = %d, want 10 and 20", ok.Load(), rejected.Load())
	}
	assertBalance(t, s, from.ID, 0)
	assertBalance(t, s, to.ID, 1000)
	assertReconciled(t, s)
}

// Transfers in opposite directions would deadlock without ordered locking.
func TestConcurrentOppositeTransfers(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a, b := newAccount(t, s, 10000), newAccount(t, s, 10000)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		from, to := a.ID, b.ID
		if i%2 == 1 {
			from, to = b.ID, a.ID
		}
		key := newKey(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Transfer(ctx, key, from, to, 10); err != nil {
				t.Errorf("transfer: %v", err)
			}
		}()
	}
	wg.Wait()

	assertBalance(t, s, a.ID, 10000)
	assertBalance(t, s, b.ID, 10000)
	assertReconciled(t, s)
}

func TestConcurrentRequestsWithSameKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	from, to := newAccount(t, s, 1000), newAccount(t, s, 0)
	key := newKey(t)

	var replays atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.Transfer(ctx, key, from.ID, to.ID, 100)
			if err != nil {
				t.Errorf("transfer: %v", err)
				return
			}
			if got.Replayed {
				replays.Add(1)
			}
		}()
	}
	wg.Wait()

	if replays.Load() != 19 {
		t.Errorf("replays = %d, want 19", replays.Load())
	}
	assertBalance(t, s, to.ID, 100)
}

func TestEntriesPagination(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	from, to := newAccount(t, s, 100), newAccount(t, s, 0)
	for i := 0; i < 5; i++ {
		if _, err := s.Transfer(ctx, newKey(t), from.ID, to.ID, int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}

	page1, err := s.Entries(ctx, to.ID, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 3 || page1[0].Amount != 5 || page1[2].Amount != 3 {
		t.Fatalf("page1 = %+v", page1)
	}

	page2, err := s.Entries(ctx, to.ID, page1[2].ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].Amount != 2 || page2[1].Amount != 1 {
		t.Fatalf("page2 = %+v", page2)
	}
}
