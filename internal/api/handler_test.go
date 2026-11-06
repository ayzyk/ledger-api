package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ayzyk/ledger-api/internal/ledger"
)

// fakeLedger returns whatever error the test sets
type fakeLedger struct {
	err       error
	replayed  bool
	gotKey    string
	gotLimit  int
	gotBefore int64
}

func (f *fakeLedger) CreateAccount(ctx context.Context, owner, currency string) (ledger.Account, error) {
	return ledger.Account{ID: 1, Owner: owner, Currency: currency}, f.err
}

func (f *fakeLedger) GetAccount(ctx context.Context, id int64) (ledger.Account, error) {
	return ledger.Account{ID: id}, f.err
}

func (f *fakeLedger) Transfer(ctx context.Context, key string, from, to, amount int64) (ledger.Transfer, error) {
	f.gotKey = key
	return ledger.Transfer{ID: 1, Replayed: f.replayed}, f.err
}

func (f *fakeLedger) Deposit(ctx context.Context, key string, accountID, amount int64) (ledger.Transfer, error) {
	f.gotKey = key
	return ledger.Transfer{ID: 1, Replayed: f.replayed}, f.err
}

func (f *fakeLedger) Entries(ctx context.Context, accountID, beforeID int64, limit int) ([]ledger.Entry, error) {
	f.gotBefore, f.gotLimit = beforeID, limit
	return nil, f.err
}

func (f *fakeLedger) Reconcile(ctx context.Context) (ledger.Report, error) {
	return ledger.Report{}, f.err
}

func do(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newTestHandler(f *fakeLedger) http.Handler {
	return NewHandler(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestTransferRequiresIdempotencyKey(t *testing.T) {
	rec := do(newTestHandler(&fakeLedger{}), "POST", "/transfers", `{"from_account_id":1,"to_account_id":2,"amount":1}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestTransferStatusCodes(t *testing.T) {
	body := `{"from_account_id":1,"to_account_id":2,"amount":100}`
	key := map[string]string{"Idempotency-Key": "abc"}

	tests := []struct {
		name     string
		err      error
		replayed bool
		want     int
	}{
		{"created", nil, false, http.StatusCreated},
		{"replay", nil, true, http.StatusOK},
		{"not found", ledger.ErrNotFound, false, http.StatusNotFound},
		{"bad amount", ledger.ErrInvalidAmount, false, http.StatusBadRequest},
		{"no money", ledger.ErrInsufficientFunds, false, http.StatusUnprocessableEntity},
		{"key reused", ledger.ErrIdempotencyMismatch, false, http.StatusConflict},
		{"unknown error", errors.New("boom"), false, http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLedger{err: tt.err, replayed: tt.replayed}
			rec := do(newTestHandler(f), "POST", "/transfers", body, key)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
			if f.gotKey != "abc" {
				t.Errorf("key = %q", f.gotKey)
			}
		})
	}
}

func TestInternalErrorIsNotLeaked(t *testing.T) {
	f := &fakeLedger{err: errors.New("pq: secret table is on fire")}
	rec := do(newTestHandler(f), "GET", "/accounts/1", "", nil)
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("body leaks the error: %s", rec.Body.String())
	}
}

func TestBadRequests(t *testing.T) {
	key := map[string]string{"Idempotency-Key": "abc"}
	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"broken json", "POST", "/accounts", `{`},
		{"unknown field", "POST", "/accounts", `{"owner":"a","is_admin":true}`},
		{"amount as string", "POST", "/transfers", `{"amount":"100"}`},
		{"bad id", "GET", "/accounts/abc", ""},
		{"negative id", "GET", "/accounts/-1", ""},
		{"limit too big", "GET", "/accounts/1/entries?limit=1000", ""},
		{"bad before_id", "GET", "/accounts/1/entries?before_id=x", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(newTestHandler(&fakeLedger{}), tt.method, tt.path, tt.body, key)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestEntriesDefaults(t *testing.T) {
	f := &fakeLedger{}
	rec := do(newTestHandler(f), "GET", "/accounts/1/entries", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if f.gotLimit != defaultLimit || f.gotBefore != 0 {
		t.Errorf("limit = %d, before = %d", f.gotLimit, f.gotBefore)
	}
}
