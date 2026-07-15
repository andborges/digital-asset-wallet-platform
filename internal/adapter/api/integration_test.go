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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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
	balanceRepo := postgres.NewBalanceRepository(pool)
	getBalances := core.NewGetCustomerBalances(balanceRepo)
	transferRepo := postgres.NewTransferRepository()
	createTransfer := core.NewCreateTransfer(transferRepo)

	serverImpl := adapterapi.NewServerInterface(createCustomer, getBalances, createTransfer)
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

// creditAccount inserts a fixture journal entry + posting directly via test SQL,
// crediting customerID's (chain, asset) account by amount — the same technique Story
// 1.2 used to prove its derivation query was real, reused here to give a transfer test
// a source balance to draw down without depending on any other write path.
func creditAccount(t *testing.T, env testEnv, customerID, chain, asset, amount string) {
	t.Helper()
	ctx := context.Background()

	var accountID string
	if err := env.pool.QueryRow(ctx,
		`SELECT id FROM accounts WHERE customer_id = $1 AND chain = $2 AND asset = $3`,
		customerID, chain, asset,
	).Scan(&accountID); err != nil {
		t.Fatalf("look up account (%s, %s) for credit fixture: %v", chain, asset, err)
	}

	journalEntryID := uuid.New().String()
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO journal_entries (id, cause_type, cause_id) VALUES ($1, 'test_fixture', $2)`,
		journalEntryID, journalEntryID,
	); err != nil {
		t.Fatalf("insert journal_entries fixture row: %v", err)
	}
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO postings (id, journal_entry_id, account_id, amount) VALUES ($1, $2, $3, $4)`,
		uuid.New().String(), journalEntryID, accountID, amount,
	); err != nil {
		t.Fatalf("insert postings fixture row: %v", err)
	}
}

// postTransfer issues a POST /v1/transfers request with the given fields and returns
// the recorded response. An empty idempotencyKey omits the header entirely (AC5).
func postTransfer(t *testing.T, env testEnv, idempotencyKey, sourceID, destID, chain, asset, amount string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(
		`{"sourceCustomerId":%q,"destinationCustomerId":%q,"chain":%q,"asset":%q,"amount":%q}`,
		sourceID, destID, chain, asset, amount,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/transfers", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

// createTestCustomer creates a customer through the real HTTP stack (reusing the
// already-tested creation path) and returns its id.
func createTestCustomer(t *testing.T, env testEnv, idempotencyKey string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create customer status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode create-customer response: %v", err)
	}
	return body.ID
}

func TestGetCustomerBalances_EndToEnd(t *testing.T) {
	env := newTestHandler(t)

	t.Run("AC1 & AC3: zero balances for every (chain, asset) pair, returned quickly", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "balances-e2e-key-1")

		start := time.Now()
		req := httptest.NewRequest(http.MethodGet, "/v1/customers/"+customerID+"/balances", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)
		elapsed := time.Since(start)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		// AC3 sanity check: a single call against a locally-run testcontainer should be
		// far under the 500ms p95 target. We record the timing but deliberately do NOT
		// fail on it — a cold-start container under CI load can exceed 500ms without any
		// real regression, and a wall-clock assertion in a functional test is flaky.
		// Real load-based p95 measurement is Story 6.4's job, not this test's.
		t.Logf("AC3 sanity: balances call took %s (target: well under 500ms; real p95 is Story 6.4)", elapsed)

		var body struct {
			Balances []struct {
				Chain   string `json:"chain"`
				Asset   string `json:"asset"`
				Balance string `json:"balance"`
			} `json:"balances"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(body.Balances) != 4 {
			t.Fatalf("balances = %+v, want exactly 4 entries", body.Balances)
		}
		for _, b := range body.Balances {
			if b.Balance != "0" {
				t.Errorf("balance for %s/%s = %q, want \"0\"", b.Chain, b.Asset, b.Balance)
			}
		}
	})

	t.Run("AC2: unknown customer id returns 404", func(t *testing.T) {
		unknownID := uuid.New().String()
		req := httptest.NewRequest(http.MethodGet, "/v1/customers/"+unknownID+"/balances", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("Content-Type = %q, want application/problem+json", ct)
		}
	})

	t.Run("AC4: balance reflects postings written directly to the ledger", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "balances-e2e-key-2")
		ctx := context.Background()

		var accountID string
		if err := env.pool.QueryRow(ctx,
			`SELECT id FROM accounts WHERE customer_id = $1 AND chain = 'base' AND asset = 'eth'`,
			customerID,
		).Scan(&accountID); err != nil {
			t.Fatalf("look up test account: %v", err)
		}

		journalEntryID := uuid.New().String()
		if _, err := env.pool.Exec(ctx,
			`INSERT INTO journal_entries (id, cause_type, cause_id) VALUES ($1, 'test_fixture', $2)`,
			journalEntryID, journalEntryID,
		); err != nil {
			t.Fatalf("insert journal_entries fixture row: %v", err)
		}
		if _, err := env.pool.Exec(ctx,
			`INSERT INTO postings (id, journal_entry_id, account_id, amount) VALUES ($1, $2, $3, $4)`,
			uuid.New().String(), journalEntryID, accountID, "1000000000000000000",
		); err != nil {
			t.Fatalf("insert postings fixture row: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/v1/customers/"+customerID+"/balances", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var body struct {
			Balances []struct {
				Chain   string `json:"chain"`
				Asset   string `json:"asset"`
				Balance string `json:"balance"`
			} `json:"balances"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		var found bool
		for _, b := range body.Balances {
			if b.Chain == "base" && b.Asset == "eth" {
				found = true
				if b.Balance != "1000000000000000000" {
					t.Fatalf("base/eth balance = %q, want %q (derived from the fixture posting)", b.Balance, "1000000000000000000")
				}
			} else if b.Balance != "0" {
				t.Errorf("balance for %s/%s = %q, want \"0\" (untouched account)", b.Chain, b.Asset, b.Balance)
			}
		}
		if !found {
			t.Fatal("expected a base/eth balance entry")
		}
	})

	t.Run("AC5: missing bearer token is rejected", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "balances-e2e-key-3")
		req := httptest.NewRequest(http.MethodGet, "/v1/customers/"+customerID+"/balances", nil)
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})
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

func TestCreateTransfer_EndToEnd(t *testing.T) {
	env := newTestHandler(t)

	t.Run("AC1: successful transfer moves balance atomically", func(t *testing.T) {
		source := createTestCustomer(t, env, "transfer-e2e-source-1")
		dest := createTestCustomer(t, env, "transfer-e2e-dest-1")
		creditAccount(t, env, source, "base", "eth", "1000")

		rec := postTransfer(t, env, "transfer-e2e-key-1", source, dest, "base", "eth", "400")
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
		}

		var body struct {
			ID                    string `json:"id"`
			SourceCustomerID      string `json:"sourceCustomerId"`
			DestinationCustomerID string `json:"destinationCustomerId"`
			Chain                 string `json:"chain"`
			Asset                 string `json:"asset"`
			Amount                string `json:"amount"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.ID == "" {
			t.Fatal("expected a non-empty transfer id")
		}
		if body.SourceCustomerID != source || body.DestinationCustomerID != dest {
			t.Fatalf("body = %+v, want source=%s dest=%s", body, source, dest)
		}
		if body.Amount != "400" {
			t.Fatalf("amount = %q, want %q", body.Amount, "400")
		}

		assertBalance(t, env, source, "base", "eth", "600")
		assertBalance(t, env, dest, "base", "eth", "400")
	})

	t.Run("AC2: insufficient balance is rejected and writes nothing", func(t *testing.T) {
		source := createTestCustomer(t, env, "transfer-e2e-source-2")
		dest := createTestCustomer(t, env, "transfer-e2e-dest-2")
		creditAccount(t, env, source, "base", "eth", "100")

		beforeCount := postingsCount(t, env)

		rec := postTransfer(t, env, "transfer-e2e-key-2", source, dest, "base", "eth", "101")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
		}

		if got := postingsCount(t, env); got != beforeCount {
			t.Fatalf("postings count = %d, want unchanged %d", got, beforeCount)
		}
		assertBalance(t, env, source, "base", "eth", "100")
	})

	t.Run("AC3 & AC4: replaying the same Idempotency-Key moves balance only once, cause is recorded exactly once", func(t *testing.T) {
		source := createTestCustomer(t, env, "transfer-e2e-source-3")
		dest := createTestCustomer(t, env, "transfer-e2e-dest-3")
		creditAccount(t, env, source, "base", "eth", "1000")

		key := "transfer-e2e-key-3"
		rec1 := postTransfer(t, env, key, source, dest, "base", "eth", "250")
		if rec1.Code != http.StatusCreated {
			t.Fatalf("first request status = %d, want %d, body = %s", rec1.Code, http.StatusCreated, rec1.Body.String())
		}

		rec2 := postTransfer(t, env, key, source, dest, "base", "eth", "250")
		if rec2.Code != rec1.Code || rec2.Body.String() != rec1.Body.String() {
			t.Fatalf("replay status/body = %d/%q, want byte-for-byte match with original %d/%q",
				rec2.Code, rec2.Body.String(), rec1.Code, rec1.Body.String())
		}

		assertBalance(t, env, source, "base", "eth", "750")
		assertBalance(t, env, dest, "base", "eth", "250")

		var journalCount int
		if err := env.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM journal_entries WHERE cause_type = 'internal_transfer' AND cause_id = $1`,
			key,
		).Scan(&journalCount); err != nil {
			t.Fatalf("query journal_entries: %v", err)
		}
		if journalCount != 1 {
			t.Fatalf("journal_entries rows for cause_id %q = %d, want exactly 1", key, journalCount)
		}
	})

	t.Run("AC5: missing Idempotency-Key is rejected", func(t *testing.T) {
		source := createTestCustomer(t, env, "transfer-e2e-source-4")
		dest := createTestCustomer(t, env, "transfer-e2e-dest-4")

		rec := postTransfer(t, env, "", source, dest, "base", "eth", "1")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("AC6: missing bearer token is rejected", func(t *testing.T) {
		source := createTestCustomer(t, env, "transfer-e2e-source-5")
		dest := createTestCustomer(t, env, "transfer-e2e-dest-5")

		body := fmt.Sprintf(`{"sourceCustomerId":%q,"destinationCustomerId":%q,"chain":"base","asset":"eth","amount":"1"}`, source, dest)
		req := httptest.NewRequest(http.MethodPost, "/v1/transfers", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "transfer-e2e-key-6")
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("AC7: unknown source or destination customer returns 404", func(t *testing.T) {
		known := createTestCustomer(t, env, "transfer-e2e-source-7")
		unknown := uuid.New().String()

		beforeCount := postingsCount(t, env)

		rec := postTransfer(t, env, "transfer-e2e-key-7a", unknown, known, "base", "eth", "1")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unknown source: status = %d, want %d, body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
		}

		rec2 := postTransfer(t, env, "transfer-e2e-key-7b", known, unknown, "base", "eth", "1")
		if rec2.Code != http.StatusNotFound {
			t.Fatalf("unknown destination: status = %d, want %d, body = %s", rec2.Code, http.StatusNotFound, rec2.Body.String())
		}

		// AC7's second clause: an unknown customer writes no postings.
		if got := postingsCount(t, env); got != beforeCount {
			t.Fatalf("postings count = %d, want unchanged %d after 404s", got, beforeCount)
		}
	})

	t.Run("AC8: self-transfer and non-positive amounts are rejected", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "transfer-e2e-source-8")

		rec := postTransfer(t, env, "transfer-e2e-key-8a", customerID, customerID, "base", "eth", "1")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("self-transfer: status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}

		other := createTestCustomer(t, env, "transfer-e2e-dest-8")
		rec2 := postTransfer(t, env, "transfer-e2e-key-8b", customerID, other, "base", "eth", "0")
		if rec2.Code != http.StatusBadRequest {
			t.Fatalf("zero amount: status = %d, want %d, body = %s", rec2.Code, http.StatusBadRequest, rec2.Body.String())
		}

		rec3 := postTransfer(t, env, "transfer-e2e-key-8c", customerID, other, "base", "eth", "-1")
		if rec3.Code != http.StatusBadRequest {
			t.Fatalf("negative amount: status = %d, want %d, body = %s", rec3.Code, http.StatusBadRequest, rec3.Body.String())
		}
	})

	t.Run("AC9: unsupported chain or asset is rejected with 400, not 404", func(t *testing.T) {
		source := createTestCustomer(t, env, "transfer-e2e-source-9")
		dest := createTestCustomer(t, env, "transfer-e2e-dest-9")

		rec := postTransfer(t, env, "transfer-e2e-key-9a", source, dest, "polygon", "eth", "1")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid chain: status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}

		rec2 := postTransfer(t, env, "transfer-e2e-key-9b", source, dest, "base", "btc", "1")
		if rec2.Code != http.StatusBadRequest {
			t.Fatalf("invalid asset: status = %d, want %d, body = %s", rec2.Code, http.StatusBadRequest, rec2.Body.String())
		}
	})
}

// assertBalance queries the balances endpoint and asserts the (chain, asset) balance
// for customerID equals want.
func assertBalance(t *testing.T, env testEnv, customerID, chain, asset, want string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/customers/"+customerID+"/balances", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get balances status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body struct {
		Balances []struct {
			Chain   string `json:"chain"`
			Asset   string `json:"asset"`
			Balance string `json:"balance"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode balances response: %v", err)
	}
	for _, b := range body.Balances {
		if b.Chain == chain && b.Asset == asset {
			if b.Balance != want {
				t.Fatalf("balance for %s/%s = %q, want %q", chain, asset, b.Balance, want)
			}
			return
		}
	}
	t.Fatalf("no balance entry found for %s/%s", chain, asset)
}

// postingsCount returns the total number of rows in the postings table, used to assert
// a rejected transfer wrote nothing.
func postingsCount(t *testing.T, env testEnv) int {
	t.Helper()
	var count int
	if err := env.pool.QueryRow(context.Background(), `SELECT count(*) FROM postings`).Scan(&count); err != nil {
		t.Fatalf("count postings: %v", err)
	}
	return count
}
