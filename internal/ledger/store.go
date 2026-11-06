package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) CreateAccount(ctx context.Context, owner, currency string) (Account, error) {
	if owner == "" {
		return Account{}, ErrInvalidOwner
	}
	if currency != "RUB" && currency != "USD" && currency != "EUR" {
		return Account{}, ErrInvalidCurrency
	}

	var a Account
	err := s.pool.QueryRow(ctx, `
		INSERT INTO accounts (owner, currency) VALUES ($1, $2)
		RETURNING id, owner, currency, balance, created_at`, owner, currency,
	).Scan(&a.ID, &a.Owner, &a.Currency, &a.Balance, &a.CreatedAt)
	if err != nil {
		return Account{}, fmt.Errorf("insert account: %w", err)
	}
	return a, nil
}

// GetAccount returns only user accounts, system accounts are internal.
func (s *Store) GetAccount(ctx context.Context, id int64) (Account, error) {
	var a Account
	err := s.pool.QueryRow(ctx, `
		SELECT id, owner, currency, balance, created_at
		FROM accounts WHERE id = $1 AND kind = 'user'`, id,
	).Scan(&a.ID, &a.Owner, &a.Currency, &a.Balance, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("select account: %w", err)
	}
	return a, nil
}

// Transfer moves money between two user accounts.
func (s *Store) Transfer(ctx context.Context, key string, from, to, amount int64) (Transfer, error) {
	if from == to {
		return Transfer{}, ErrSameAccount
	}
	return s.move(ctx, key, from, to, amount, func(fromAcc, toAcc lockedAccount) error {
		if fromAcc.kind == "system" || toAcc.kind == "system" {
			return ErrSystemAccount
		}
		return nil
	})
}

// Deposit adds money to a user account from the system account of the same currency.
func (s *Store) Deposit(ctx context.Context, key string, accountID, amount int64) (Transfer, error) {
	var systemID int64
	err := s.pool.QueryRow(ctx, `
		SELECT sys.id
		FROM accounts acc
		JOIN accounts sys ON sys.currency = acc.currency AND sys.kind = 'system'
		WHERE acc.id = $1 AND acc.kind = 'user'`, accountID,
	).Scan(&systemID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, ErrNotFound
	}
	if err != nil {
		return Transfer{}, fmt.Errorf("find system account: %w", err)
	}
	return s.move(ctx, key, systemID, accountID, amount, nil)
}

type lockedAccount struct {
	id       int64
	kind     string
	currency string
	balance  int64
}

func (s *Store) move(
	ctx context.Context,
	key string,
	from, to, amount int64,
	check func(fromAcc, toAcc lockedAccount) error,
) (Transfer, error) {
	if amount <= 0 {
		return Transfer{}, ErrInvalidAmount
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Transfer{}, err
	}
	defer tx.Rollback(ctx)

	fromAcc, toAcc, err := lockAccounts(ctx, tx, from, to)
	if err != nil {
		return Transfer{}, err
	}

	// The key is checked before the balance. A replay of a successful transfer
	// has to return the old result even if the account is empty by now.
	t := Transfer{FromAccountID: from, ToAccountID: to, Amount: amount}
	err = tx.QueryRow(ctx, `
		INSERT INTO transfers (idempotency_key, from_account_id, to_account_id, amount)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id, created_at`, key, from, to, amount,
	).Scan(&t.ID, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return existingTransfer(ctx, tx, key, from, to, amount)
	}
	if err != nil {
		return Transfer{}, fmt.Errorf("insert transfer: %w", err)
	}

	if check != nil {
		if err := check(fromAcc, toAcc); err != nil {
			return Transfer{}, err
		}
	}
	if fromAcc.currency != toAcc.currency {
		return Transfer{}, ErrCurrencyMismatch
	}
	if fromAcc.kind == "user" && fromAcc.balance < amount {
		return Transfer{}, ErrInsufficientFunds
	}

	if err := apply(ctx, tx, t.ID, from, -amount); err != nil {
		return Transfer{}, err
	}
	if err := apply(ctx, tx, t.ID, to, amount); err != nil {
		return Transfer{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Transfer{}, fmt.Errorf("commit: %w", err)
	}
	return t, nil
}

// lockAccounts locks both rows in id order, so A->B and B->A can not deadlock.
// FOR NO KEY UPDATE is enough here and does not conflict with the foreign key
// checks from inserts into transfers and entries.
func lockAccounts(ctx context.Context, tx pgx.Tx, from, to int64) (lockedAccount, lockedAccount, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, kind, currency, balance
		FROM accounts
		WHERE id = ANY($1)
		ORDER BY id
		FOR NO KEY UPDATE`, []int64{from, to})
	if err != nil {
		return lockedAccount{}, lockedAccount{}, fmt.Errorf("lock accounts: %w", err)
	}

	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (lockedAccount, error) {
		var a lockedAccount
		err := row.Scan(&a.id, &a.kind, &a.currency, &a.balance)
		return a, err
	})
	if err != nil {
		return lockedAccount{}, lockedAccount{}, fmt.Errorf("lock accounts: %w", err)
	}
	if len(found) != 2 {
		return lockedAccount{}, lockedAccount{}, ErrNotFound
	}
	if found[0].id == from {
		return found[0], found[1], nil
	}
	return found[1], found[0], nil
}

func existingTransfer(ctx context.Context, tx pgx.Tx, key string, from, to, amount int64) (Transfer, error) {
	var t Transfer
	err := tx.QueryRow(ctx, `
		SELECT id, from_account_id, to_account_id, amount, created_at
		FROM transfers WHERE idempotency_key = $1`, key,
	).Scan(&t.ID, &t.FromAccountID, &t.ToAccountID, &t.Amount, &t.CreatedAt)
	if err != nil {
		return Transfer{}, fmt.Errorf("select transfer: %w", err)
	}
	if t.FromAccountID != from || t.ToAccountID != to || t.Amount != amount {
		return Transfer{}, ErrIdempotencyMismatch
	}
	t.Replayed = true
	return t, nil
}

func apply(ctx context.Context, tx pgx.Tx, transferID, accountID, amount int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO entries (transfer_id, account_id, amount) VALUES ($1, $2, $3)`,
		transferID, accountID, amount)
	if err != nil {
		return fmt.Errorf("insert entry: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE accounts SET balance = balance + $1 WHERE id = $2`, amount, accountID)
	if err != nil {
		return fmt.Errorf("update balance: %w", err)
	}
	return nil
}

// Entries returns the account statement, newest first.
// beforeID is the id of the last entry of the previous page, 0 means first page.
func (s *Store) Entries(ctx context.Context, accountID, beforeID int64, limit int) ([]Entry, error) {
	if _, err := s.GetAccount(ctx, accountID); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, transfer_id, amount, created_at
		FROM entries
		WHERE account_id = $1 AND ($2 = 0 OR id < $2)
		ORDER BY id DESC
		LIMIT $3`, accountID, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("select entries: %w", err)
	}
	entries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Entry])
	if err != nil {
		return nil, fmt.Errorf("select entries: %w", err)
	}
	return entries, nil
}

// Reconcile checks that balances match the entries and every currency sums to zero.
// REPEATABLE READ gives both queries the same snapshot, otherwise a transfer
// committed in between could look like an error.
func (s *Store) Reconcile(ctx context.Context) (Report, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `SELECT account_id, balance, entries_sum FROM balance_mismatches`)
	if err != nil {
		return Report{}, fmt.Errorf("select mismatches: %w", err)
	}
	mismatches, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Mismatch])
	if err != nil {
		return Report{}, fmt.Errorf("select mismatches: %w", err)
	}

	report := Report{Mismatches: mismatches, Totals: map[string]int64{}}
	rows, err = tx.Query(ctx, `SELECT currency, SUM(balance)::BIGINT FROM accounts GROUP BY currency`)
	if err != nil {
		return Report{}, fmt.Errorf("select totals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var currency string
		var total int64
		if err := rows.Scan(&currency, &total); err != nil {
			return Report{}, err
		}
		report.Totals[currency] = total
	}
	return report, rows.Err()
}
