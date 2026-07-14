package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

type fakeTx struct {
	committed  bool
	rolledBack bool
	commitErr  error
}

func (t *fakeTx) Commit(ctx context.Context) error {
	t.committed = true
	return t.commitErr
}

func (t *fakeTx) Rollback(ctx context.Context) error {
	t.rolledBack = true
	return nil
}

type txCtxKey struct{}

type fakeTxBeginner struct {
	beginCount int
	lastTx     *fakeTx
	beginErr   error
}

func (b *fakeTxBeginner) Begin(ctx context.Context) (context.Context, core.Tx, error) {
	b.beginCount++
	if b.beginErr != nil {
		return ctx, nil, b.beginErr
	}
	tx := &fakeTx{}
	b.lastTx = tx
	return context.WithValue(ctx, txCtxKey{}, "tx-open"), tx, nil
}

type fakeIdempotencyStore struct {
	entries      map[string]core.StoredEntry
	insertCalls  int
	insertErr    error
	conflictOnce bool // if true, the first Insert call returns ErrKeyConflict, then a stored entry appears
}

func (s *fakeIdempotencyStore) Lookup(ctx context.Context, key string) (core.StoredEntry, bool, error) {
	e, ok := s.entries[key]
	return e, ok, nil
}

func (s *fakeIdempotencyStore) Insert(ctx context.Context, key string, requestHash []byte, resp core.StoredResponse) error {
	s.insertCalls++
	if s.conflictOnce && s.insertCalls == 1 {
		// Simulate a concurrent winner having committed first.
		if s.entries == nil {
			s.entries = map[string]core.StoredEntry{}
		}
		s.entries[key] = core.StoredEntry{RequestHash: []byte("winner-hash"), Response: core.StoredResponse{Status: http.StatusCreated, Body: []byte(`{"id":"winner"}`)}}
		return core.ErrKeyConflict
	}
	if s.insertErr != nil {
		return s.insertErr
	}
	if s.entries == nil {
		s.entries = map[string]core.StoredEntry{}
	}
	s.entries[key] = core.StoredEntry{RequestHash: requestHash, Response: resp}
	return nil
}

func countingHandler(calls *int, status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func TestIdempotencyMiddleware_MissingHeaderRejectsWithoutCallingHandler(t *testing.T) {
	var calls int
	txb := &fakeTxBeginner{}
	store := &fakeIdempotencyStore{}
	mw := IdempotencyMiddleware(txb, store)
	handler := mw(countingHandler(&calls, http.StatusCreated, `{"id":"1"}`))

	req := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if calls != 0 {
		t.Fatalf("handler was called %d times, want 0", calls)
	}
	if txb.beginCount != 0 {
		t.Fatalf("transaction was begun %d times, want 0", txb.beginCount)
	}
}

func TestIdempotencyMiddleware_NewKeyCallsHandlerOnceAndCommits(t *testing.T) {
	var calls int
	txb := &fakeTxBeginner{}
	store := &fakeIdempotencyStore{}
	mw := IdempotencyMiddleware(txb, store)
	handler := mw(countingHandler(&calls, http.StatusCreated, `{"id":"1"}`))

	req := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader(`{"foo":"bar"}`))
	req.Header.Set("Idempotency-Key", "key-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if calls != 1 {
		t.Fatalf("handler was called %d times, want 1", calls)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if rec.Body.String() != `{"id":"1"}` {
		t.Fatalf("body = %q, want %q", rec.Body.String(), `{"id":"1"}`)
	}
	if txb.beginCount != 1 {
		t.Fatalf("transaction was begun %d times, want 1", txb.beginCount)
	}
	if !txb.lastTx.committed {
		t.Fatal("expected the transaction to be committed")
	}
	if store.insertCalls != 1 {
		t.Fatalf("Insert was called %d times, want 1", store.insertCalls)
	}
	if got := store.entries["key-1"].Response.Status; got != http.StatusCreated {
		t.Fatalf("stored status = %d, want %d", got, http.StatusCreated)
	}
	if got := string(store.entries["key-1"].Response.Body); got != `{"id":"1"}` {
		t.Fatalf("stored body = %q, want %q", got, `{"id":"1"}`)
	}
}

func TestIdempotencyMiddleware_ReplaySameBodyReturnsStoredResponseWithoutCallingHandler(t *testing.T) {
	var calls int
	txb := &fakeTxBeginner{}
	body := `{"foo":"bar"}`
	hash := requestHash(http.MethodPost, "/v1/customers", []byte(body))
	store := &fakeIdempotencyStore{entries: map[string]core.StoredEntry{
		"key-1": {RequestHash: hash, Response: core.StoredResponse{Status: http.StatusCreated, Body: []byte(`{"id":"1"}`)}},
	}}
	mw := IdempotencyMiddleware(txb, store)
	handler := mw(countingHandler(&calls, http.StatusCreated, `{"id":"SHOULD_NOT_BE_CALLED"}`))

	req := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "key-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if calls != 0 {
		t.Fatalf("handler was called %d times, want 0 (replay must not re-invoke the handler)", calls)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if rec.Body.String() != `{"id":"1"}` {
		t.Fatalf("body = %q, want the originally stored body %q", rec.Body.String(), `{"id":"1"}`)
	}
	if txb.beginCount != 0 {
		t.Fatalf("transaction was begun %d times, want 0 on a pure replay", txb.beginCount)
	}
}

func TestIdempotencyMiddleware_ReplayDifferentBodyReturns409WithoutCallingHandler(t *testing.T) {
	var calls int
	txb := &fakeTxBeginner{}
	originalHash := requestHash(http.MethodPost, "/v1/customers", []byte(`{"foo":"bar"}`))
	store := &fakeIdempotencyStore{entries: map[string]core.StoredEntry{
		"key-1": {RequestHash: originalHash, Response: core.StoredResponse{Status: http.StatusCreated, Body: []byte(`{"id":"1"}`)}},
	}}
	mw := IdempotencyMiddleware(txb, store)
	handler := mw(countingHandler(&calls, http.StatusCreated, `{"id":"SHOULD_NOT_BE_CALLED"}`))

	req := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader(`{"foo":"DIFFERENT"}`))
	req.Header.Set("Idempotency-Key", "key-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if calls != 0 {
		t.Fatalf("handler was called %d times, want 0", calls)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestIdempotencyMiddleware_ConcurrentDuplicateRollsBackAndReturnsWinnerResponse(t *testing.T) {
	var calls int
	txb := &fakeTxBeginner{}
	store := &fakeIdempotencyStore{conflictOnce: true}
	mw := IdempotencyMiddleware(txb, store)
	handler := mw(countingHandler(&calls, http.StatusCreated, `{"id":"loser"}`))

	req := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader(`{"foo":"bar"}`))
	req.Header.Set("Idempotency-Key", "key-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if calls != 1 {
		t.Fatalf("handler was called %d times, want 1 (it runs before the conflict is discovered)", calls)
	}
	if !txb.lastTx.rolledBack {
		t.Fatal("expected the losing transaction to be rolled back")
	}
	if txb.lastTx.committed {
		t.Fatal("the losing transaction must not be committed")
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (the winner's status)", rec.Code, http.StatusCreated)
	}
	if rec.Body.String() != `{"id":"winner"}` {
		t.Fatalf("body = %q, want the winner's stored body %q", rec.Body.String(), `{"id":"winner"}`)
	}
}

func TestIdempotencyMiddleware_BeginFailureReturns500WithoutCallingHandler(t *testing.T) {
	var calls int
	txb := &fakeTxBeginner{beginErr: errors.New("db unavailable")}
	store := &fakeIdempotencyStore{}
	mw := IdempotencyMiddleware(txb, store)
	handler := mw(countingHandler(&calls, http.StatusCreated, `{"id":"1"}`))

	req := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader(`{"foo":"bar"}`))
	req.Header.Set("Idempotency-Key", "key-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if calls != 0 {
		t.Fatalf("handler was called %d times, want 0", calls)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestIdempotencyMiddleware_HandlerReceivesContextWithOpenTransaction(t *testing.T) {
	var sawTx bool
	txb := &fakeTxBeginner{}
	store := &fakeIdempotencyStore{}
	mw := IdempotencyMiddleware(txb, store)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v, _ := r.Context().Value(txCtxKey{}).(string); v == "tx-open" {
			sawTx = true
		}
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", "key-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !sawTx {
		t.Fatal("expected the handler to observe the open transaction via request context")
	}
}
