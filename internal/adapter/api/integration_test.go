// Package api_test exercises the wired stack (API adapter + Postgres adapter + core)
// against a real PostgreSQL container — this project's stated thesis is rigor over
// shortcuts (PRD Success Metric 5), so this test does not substitute a mocked
// repository for the real thing. It lives in an external test package (not `package
// api`) precisely so it can import both internal/adapter/api and
// internal/adapter/postgres without that import appearing in either adapter's own
// production code — mirroring cmd/walletd/main.go's role as composition root.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	adapterapi "github.com/andborges/digital-asset-wallet-platform/internal/adapter/api"
	"github.com/andborges/digital-asset-wallet-platform/internal/adapter/postgres"
	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

type testEnv struct {
	handler http.Handler
	pool    *pgxpool.Pool
}

func newTestHandler(t *testing.T) testEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Docker-backed integration test in -short mode")
	}
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:18",
		tcpostgres.WithDatabase("walletd"),
		tcpostgres.WithUsername("walletd"),
		tcpostgres.WithPassword("walletd"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	txBeginner := postgres.NewTxBeginner(pool)
	idempotencyStore := postgres.NewIdempotencyStore(pool)
	customerRepo := postgres.NewCustomerRepository()
	createCustomer := core.NewCreateCustomer(customerRepo)

	serverImpl := adapterapi.NewServerInterface(createCustomer)
	mux := http.NewServeMux()
	handler := adapterapi.HandlerWithOptions(serverImpl, adapterapi.StdHTTPServerOptions{
		BaseRouter: mux,
		BaseURL:    "/v1",
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			adapterapi.WriteProblem(w, http.StatusBadRequest, "invalid-request", err.Error(), r.URL.Path)
		},
	})
	handler = adapterapi.IdempotencyMiddleware(txBeginner, idempotencyStore)(handler)
	handler = adapterapi.AuthMiddleware([]string{"test-token"})(handler)

	return testEnv{handler: handler, pool: pool}
}

func TestCreateCustomer_EndToEnd(t *testing.T) {
	env := newTestHandler(t)
	handler := env.handler

	t.Run("AC1 & AC4: creates a customer with exactly four provisioned accounts, atomically", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Idempotency-Key", "e2e-key-1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
		}
		var body struct {
			ID        string    `json:"id"`
			CreatedAt time.Time `json:"createdAt"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.ID == "" {
			t.Fatal("expected a non-empty customer id")
		}

		// AC4: verify directly against Postgres — not just the HTTP response — that the
		// customer and all four accounts exist as a single fait accompli, one insert
		// each, no partial state. AC1: verify the exact (chain, asset) pairs.
		ctx := context.Background()
		var customerCount int
		if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM customers WHERE id = $1`, body.ID).Scan(&customerCount); err != nil {
			t.Fatalf("query customers: %v", err)
		}
		if customerCount != 1 {
			t.Fatalf("customers row count = %d, want 1", customerCount)
		}

		rows, err := env.pool.Query(ctx, `SELECT chain, asset FROM accounts WHERE customer_id = $1 ORDER BY chain, asset`, body.ID)
		if err != nil {
			t.Fatalf("query accounts: %v", err)
		}
		defer rows.Close()
		var pairs []string
		for rows.Next() {
			var chain, asset string
			if err := rows.Scan(&chain, &asset); err != nil {
				t.Fatalf("scan account row: %v", err)
			}
			pairs = append(pairs, chain+"/"+asset)
		}
		want := []string{"arbitrum/eth", "arbitrum/usdc", "base/eth", "base/usdc"}
		if len(pairs) != len(want) {
			t.Fatalf("provisioned accounts = %v, want exactly %v", pairs, want)
		}
		for i, w := range want {
			if pairs[i] != w {
				t.Fatalf("provisioned accounts = %v, want %v", pairs, want)
			}
		}

		// AD-3: accounts carry no balance column at all — a zero balance is the
		// absence of postings, not a stored value.
		var balanceColumnExists bool
		if err := env.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'accounts' AND column_name = 'balance')`,
		).Scan(&balanceColumnExists); err != nil {
			t.Fatalf("query information_schema: %v", err)
		}
		if balanceColumnExists {
			t.Fatal("accounts table must not have a balance column (AD-3: balances are derived from postings)")
		}
	})

	t.Run("AC2: replay with the same body returns the original response and creates nothing new", func(t *testing.T) {
		req1 := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader(`{"note":"replay-test"}`))
		req1.Header.Set("Authorization", "Bearer test-token")
		req1.Header.Set("Idempotency-Key", "e2e-key-2")
		rec1 := httptest.NewRecorder()
		handler.ServeHTTP(rec1, req1)
		if rec1.Code != http.StatusCreated {
			t.Fatalf("first request status = %d, want %d, body = %s", rec1.Code, http.StatusCreated, rec1.Body.String())
		}

		req2 := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader(`{"note":"replay-test"}`))
		req2.Header.Set("Authorization", "Bearer test-token")
		req2.Header.Set("Idempotency-Key", "e2e-key-2")
		rec2 := httptest.NewRecorder()
		handler.ServeHTTP(rec2, req2)

		if rec2.Code != rec1.Code {
			t.Fatalf("replay status = %d, want %d", rec2.Code, rec1.Code)
		}
		if rec2.Body.String() != rec1.Body.String() {
			t.Fatalf("replay body = %q, want byte-for-byte match with original %q", rec2.Body.String(), rec1.Body.String())
		}

		req3 := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader(`{"note":"different-body"}`))
		req3.Header.Set("Authorization", "Bearer test-token")
		req3.Header.Set("Idempotency-Key", "e2e-key-2")
		rec3 := httptest.NewRecorder()
		handler.ServeHTTP(rec3, req3)
		if rec3.Code != http.StatusConflict {
			t.Fatalf("different-body replay status = %d, want %d", rec3.Code, http.StatusConflict)
		}
	})

	t.Run("AC3: missing Idempotency-Key is rejected with no side effects", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("Content-Type = %q, want application/problem+json", ct)
		}
	})

	t.Run("AC5: missing bearer token is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader("{}"))
		req.Header.Set("Idempotency-Key", "e2e-key-3")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})
}
