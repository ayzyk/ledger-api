// Package api is the HTTP layer of the service.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/ayzyk/ledger-api/internal/ledger"
)

const (
	maxBodyBytes = 1 << 16
	defaultLimit = 50
	maxLimit     = 200
)

type Ledger interface {
	CreateAccount(ctx context.Context, owner, currency string) (ledger.Account, error)
	GetAccount(ctx context.Context, id int64) (ledger.Account, error)
	Transfer(ctx context.Context, key string, from, to, amount int64) (ledger.Transfer, error)
	Deposit(ctx context.Context, key string, accountID, amount int64) (ledger.Transfer, error)
	Entries(ctx context.Context, accountID, beforeID int64, limit int) ([]ledger.Entry, error)
	Reconcile(ctx context.Context) (ledger.Report, error)
}

type Handler struct {
	ledger Ledger
	log    *slog.Logger
}

func NewHandler(l Ledger, log *slog.Logger) http.Handler {
	h := &Handler{ledger: l, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("POST /accounts", h.createAccount)
	mux.HandleFunc("GET /accounts/{id}", h.getAccount)
	mux.HandleFunc("GET /accounts/{id}/entries", h.listEntries)
	mux.HandleFunc("POST /deposits", h.deposit)
	mux.HandleFunc("POST /transfers", h.transfer)
	mux.HandleFunc("GET /reconciliation", h.reconcile)
	return mux
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) createAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Owner    string `json:"owner"`
		Currency string `json:"currency"`
	}
	if !decode(w, r, &req) {
		return
	}
	account, err := h.ledger.CreateAccount(r.Context(), req.Owner, req.Currency)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, account)
}

func (h *Handler) getAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	account, err := h.ledger.GetAccount(r.Context(), id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, account)
}

func (h *Handler) listEntries(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}

	limit, beforeID := defaultLimit, int64(0)
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			writeMessage(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	if v := r.URL.Query().Get("before_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeMessage(w, http.StatusBadRequest, "before_id must be a positive number")
			return
		}
		beforeID = n
	}

	entries, err := h.ledger.Entries(r.Context(), id, beforeID, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (h *Handler) deposit(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var req struct {
		AccountID int64 `json:"account_id"`
		Amount    int64 `json:"amount"`
	}
	if !decode(w, r, &req) {
		return
	}
	t, err := h.ledger.Deposit(r.Context(), key, req.AccountID, req.Amount)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeTransfer(w, t)
}

func (h *Handler) transfer(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var req struct {
		FromAccountID int64 `json:"from_account_id"`
		ToAccountID   int64 `json:"to_account_id"`
		Amount        int64 `json:"amount"`
	}
	if !decode(w, r, &req) {
		return
	}
	t, err := h.ledger.Transfer(r.Context(), key, req.FromAccountID, req.ToAccountID, req.Amount)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeTransfer(w, t)
}

func (h *Handler) reconcile(w http.ResponseWriter, r *http.Request) {
	report, err := h.ledger.Reconcile(r.Context())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": report.OK(), "report": report})
}

func writeTransfer(w http.ResponseWriter, t ledger.Transfer) {
	status := http.StatusCreated
	if t.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, t)
}

func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 255 {
		writeMessage(w, http.StatusBadRequest, "Idempotency-Key header is required (max 255 characters)")
		return "", false
	}
	return key, true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeMessage(w, http.StatusBadRequest, "invalid account id")
		return 0, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeMessage(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ledger.ErrNotFound):
		writeMessage(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ledger.ErrInvalidAmount),
		errors.Is(err, ledger.ErrInvalidCurrency),
		errors.Is(err, ledger.ErrInvalidOwner),
		errors.Is(err, ledger.ErrSameAccount):
		writeMessage(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ledger.ErrInsufficientFunds),
		errors.Is(err, ledger.ErrCurrencyMismatch),
		errors.Is(err, ledger.ErrSystemAccount):
		writeMessage(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, ledger.ErrIdempotencyMismatch):
		writeMessage(w, http.StatusConflict, err.Error())
	default:
		// details stay in the log, the client should not see SQL errors
		h.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeMessage(w, http.StatusInternalServerError, "internal error")
	}
}

func writeMessage(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
