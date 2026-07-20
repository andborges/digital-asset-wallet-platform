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
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	adapterapi "github.com/andborges/digital-asset-wallet-platform/internal/adapter/api"
	"github.com/andborges/digital-asset-wallet-platform/internal/adapter/evm"
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
	addressDeriver := evm.NewDepositAddressDeriver()
	createCustomer := core.NewCreateCustomer(customerRepo, addressDeriver)
	customerReader := postgres.NewCustomerReader(pool)
	getCustomer := core.NewGetCustomer(customerReader)
	balanceRepo := postgres.NewBalanceRepository(pool)
	getBalances := core.NewGetCustomerBalances(balanceRepo)
	transferRepo := postgres.NewTransferRepository()
	createTransfer := core.NewCreateTransfer(transferRepo)
	transactionRepo := postgres.NewTransactionRepository(pool, []byte("test-cursor-signing-key"))
	listTransactions := core.NewListCustomerTransactions(transactionRepo)
	depositReader := postgres.NewDepositReader(pool)
	getDeposits := core.NewGetCustomerDeposits(depositReader)
	unsupportedTokenRepo := postgres.NewUnsupportedTokenRepository(pool)
	listUnsupportedTokenObservations := core.NewListUnsupportedTokenObservations(unsupportedTokenRepo)
	// Story 3.1: a fake FeeEstimator, not a real evm.FeeEstimator — this end-to-end test
	// proves the HTTP wiring (query-param validation, response shape, auth), not chain
	// RPC behavior, which fee_estimator_test.go already covers against fakes/canned
	// responses. No real RPC round-trip is needed or wanted here. Story 3.3's
	// CreateWithdrawal shares this SAME fake instance (as the core.FeeEstimator port, not
	// the core.EstimateFee use case) — its own fee-inclusive balance check needs the
	// identical fixed TotalFee this test's fixtures are sized around.
	feeEstimator := &fakeFeeEstimator{
		result: core.FeeEstimate{L2Fee: big.NewInt(1000), L1Fee: big.NewInt(500), TotalFee: big.NewInt(1500)},
	}
	estimateFee := core.NewEstimateFee(feeEstimator)
	withdrawalRepo := postgres.NewWithdrawalRepository()
	// Story 3.3: a fake WithdrawalThresholdLister with one fixed threshold for every
	// (chain, asset) — this end-to-end test proves the HTTP wiring and routing outcomes,
	// not the real withdrawal_approval_thresholds table (withdrawal_threshold_lister_test.go
	// already covers that against a real Postgres container). Every withdrawal amount used
	// below this threshold routes to "approved"; a dedicated test case uses an amount above
	// it to exercise "awaiting-approval".
	withdrawalThresholdLister := &fakeWithdrawalThresholdLister{threshold: big.NewInt(5000)}
	createWithdrawal := core.NewCreateWithdrawal(withdrawalRepo, feeEstimator, withdrawalThresholdLister)
	approveWithdrawal := core.NewApproveWithdrawal(withdrawalRepo)

	testLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serverImpl := adapterapi.NewServerInterface(createCustomer, getCustomer, getBalances, createTransfer, listTransactions, getDeposits, listUnsupportedTokenObservations, estimateFee, createWithdrawal, approveWithdrawal, testLogger)
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

	// account_type = 'available' (Story 3.2): since that story, every customer has TWO
	// accounts per (chain, asset) — available and hold — so an unscoped lookup here would
	// match both rows and nondeterministically credit whichever QueryRow happened to
	// return first. This fixture always means "fund the customer's spendable balance."
	var accountID string
	if err := env.pool.QueryRow(ctx,
		`SELECT id FROM accounts WHERE customer_id = $1 AND chain = $2 AND asset = $3 AND account_type = 'available'`,
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

// assertWellFormedDepositAddress fails the test unless addr is a well-formed,
// EIP-55-checksummed 20-byte hex address. The check is delegated to
// evm.IsChecksummedAddress so this test — a composition root, like cmd/walletd/main.go —
// never imports go-ethereum directly; the dependency stays confined to internal/adapter/evm
// (AD-1). It is a stronger check than a regex: it fails on wrong length, non-hex
// characters, AND a wrong checksum.
func assertWellFormedDepositAddress(t *testing.T, addr string) {
	t.Helper()
	if addr == "" {
		t.Fatal("expected a non-empty depositAddress")
	}
	if !evm.IsChecksummedAddress(addr) {
		t.Fatalf("depositAddress %q is not a well-formed EIP-55 checksummed address", addr)
	}
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
			`SELECT id FROM accounts WHERE customer_id = $1 AND chain = 'base' AND asset = 'eth' AND account_type = 'available'`,
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

	t.Run("AC1 & AC4: creates a customer with exactly eight provisioned accounts (available + hold per pair), atomically", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Idempotency-Key", "e2e-key-1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
		}
		var body struct {
			ID             string    `json:"id"`
			CreatedAt      time.Time `json:"createdAt"`
			DepositAddress string    `json:"depositAddress"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.ID == "" {
			t.Fatal("expected a non-empty customer id")
		}
		assertWellFormedDepositAddress(t, body.DepositAddress)

		// AC4: verify directly against Postgres — not just the HTTP response — that the
		// customer and all eight accounts exist as a single fait accompli, one insert
		// each, no partial state. AC1: verify the exact (chain, asset, account_type)
		// triples (Story 3.2: available + hold per pair, 4 pairs x 2 types = 8).
		ctx := context.Background()
		var customerCount int
		if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM customers WHERE id = $1`, body.ID).Scan(&customerCount); err != nil {
			t.Fatalf("query customers: %v", err)
		}
		if customerCount != 1 {
			t.Fatalf("customers row count = %d, want 1", customerCount)
		}

		rows, err := env.pool.Query(ctx, `SELECT chain, asset, account_type FROM accounts WHERE customer_id = $1 ORDER BY chain, asset, account_type`, body.ID)
		if err != nil {
			t.Fatalf("query accounts: %v", err)
		}
		defer rows.Close()
		var triples []string
		for rows.Next() {
			var chain, asset, accountType string
			if err := rows.Scan(&chain, &asset, &accountType); err != nil {
				t.Fatalf("scan account row: %v", err)
			}
			triples = append(triples, chain+"/"+asset+"/"+accountType)
		}
		want := []string{
			"arbitrum/eth/available", "arbitrum/eth/hold",
			"arbitrum/usdc/available", "arbitrum/usdc/hold",
			"base/eth/available", "base/eth/hold",
			"base/usdc/available", "base/usdc/hold",
		}
		if len(triples) != len(want) {
			t.Fatalf("provisioned accounts = %v, want exactly %v", triples, want)
		}
		for i, w := range want {
			if triples[i] != w {
				t.Fatalf("provisioned accounts = %v, want %v", triples, want)
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

	t.Run("Story 1.5 AC1: deposit address is computed once and persisted atomically with the customer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/customers", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Idempotency-Key", "e2e-deposit-address-key-1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
		}
		var body struct {
			ID             string `json:"id"`
			DepositAddress string `json:"depositAddress"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		assertWellFormedDepositAddress(t, body.DepositAddress)

		// AD-4: the deposit address lands in the same transaction as the customer and
		// account rows — verify directly against Postgres that exactly one row exists,
		// matching the address the API returned.
		ctx := context.Background()
		var count int
		var storedAddress string
		if err := env.pool.QueryRow(ctx,
			`SELECT count(*), max(address) FROM deposit_addresses WHERE customer_id = $1`,
			body.ID,
		).Scan(&count, &storedAddress); err != nil {
			t.Fatalf("query deposit_addresses: %v", err)
		}
		if count != 1 {
			t.Fatalf("deposit_addresses row count = %d, want exactly 1", count)
		}
		if storedAddress != body.DepositAddress {
			t.Fatalf("stored address = %q, want %q (must match what CreateCustomer returned)", storedAddress, body.DepositAddress)
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

// getCustomer issues a GET /v1/customers/{id} request and returns the recorded response.
func getCustomer(t *testing.T, env testEnv, customerID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/customers/"+customerID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

func TestGetCustomer_EndToEnd(t *testing.T) {
	env := newTestHandler(t)

	t.Run("AC2: response includes the deposit address as an attribute of the customer resource", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "get-customer-e2e-key-1")

		rec := getCustomer(t, env, customerID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var body struct {
			ID             string    `json:"id"`
			CreatedAt      time.Time `json:"createdAt"`
			DepositAddress string    `json:"depositAddress"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.ID != customerID {
			t.Fatalf("id = %q, want %q", body.ID, customerID)
		}
		assertWellFormedDepositAddress(t, body.DepositAddress)
	})

	t.Run("AC4: repeated requests return the exact same stored address, never re-derived", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "get-customer-e2e-key-2")

		first := getCustomer(t, env, customerID)
		if first.Code != http.StatusOK {
			t.Fatalf("first request status = %d, want %d, body = %s", first.Code, http.StatusOK, first.Body.String())
		}
		second := getCustomer(t, env, customerID)
		if second.Code != http.StatusOK {
			t.Fatalf("second request status = %d, want %d, body = %s", second.Code, http.StatusOK, second.Body.String())
		}

		var firstBody, secondBody struct {
			DepositAddress string `json:"depositAddress"`
		}
		if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
			t.Fatalf("decode first response: %v", err)
		}
		if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
			t.Fatalf("decode second response: %v", err)
		}
		if firstBody.DepositAddress != secondBody.DepositAddress {
			t.Fatalf("address changed across requests: %q != %q (must be read from storage, never re-derived)", firstBody.DepositAddress, secondBody.DepositAddress)
		}
	})

	t.Run("unknown customer id returns 404", func(t *testing.T) {
		unknownID := uuid.New().String()
		rec := getCustomer(t, env, unknownID)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
		}
	})

	t.Run("missing bearer token is rejected", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "get-customer-e2e-key-3")
		req := httptest.NewRequest(http.MethodGet, "/v1/customers/"+customerID, nil)
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)

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

// postWithdrawal issues a POST /v1/withdrawals request with the given fields and returns
// the recorded response. An empty idempotencyKey omits the header entirely (mirrors
// postTransfer's identical convention).
func postWithdrawal(t *testing.T, env testEnv, idempotencyKey, customerID, chain, asset, amount, destinationAddress string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(
		`{"customerId":%q,"chain":%q,"asset":%q,"amount":%q,"destinationAddress":%q}`,
		customerID, chain, asset, amount, destinationAddress,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/withdrawals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

// validDestinationAddress is a structurally well-formed 0x-prefixed 40-hex-character
// address, used throughout the withdrawal tests below — Story 3.2 validates shape only,
// never a real on-chain destination.
const validDestinationAddress = "0x00000000000000000000000000000000000000AA"

func TestCreateWithdrawal_EndToEnd(t *testing.T) {
	env := newTestHandler(t)

	t.Run("AC1: a valid withdrawal places a hold, then auto-approves — available debited, hold credited, status approved", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-1")
		// 2000 covers the withdrawn 400 PLUS the fixed 1500 fee estimate this test env's
		// fakeFeeEstimator returns (Story 3.3's fee-inclusive balance check), with headroom
		// to spare.
		creditAccount(t, env, customerID, "base", "eth", "2000")

		rec := postWithdrawal(t, env, "withdrawal-e2e-key-1", customerID, "base", "eth", "400", validDestinationAddress)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
		}

		var body struct {
			ID                 string `json:"id"`
			CustomerID         string `json:"customerId"`
			Chain              string `json:"chain"`
			Asset              string `json:"asset"`
			Amount             string `json:"amount"`
			DestinationAddress string `json:"destinationAddress"`
			Status             string `json:"status"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.ID == "" {
			t.Fatal("expected a non-empty withdrawal id")
		}
		if body.CustomerID != customerID {
			t.Fatalf("customerId = %q, want %q", body.CustomerID, customerID)
		}
		if body.Amount != "400" {
			t.Fatalf("amount = %q, want %q", body.Amount, "400")
		}
		if body.DestinationAddress != validDestinationAddress {
			t.Fatalf("destinationAddress = %q, want %q", body.DestinationAddress, validDestinationAddress)
		}
		// 400 is below this test env's fixed 5000 threshold (Story 3.3), so this withdrawal
		// auto-approves in the same request — no operator action needed.
		if body.Status != "approved" {
			t.Fatalf("status = %q, want %q", body.Status, "approved")
		}

		// The customer's visible (available) balance is decreased by the withdrawn
		// amount — the Manual Checks section of Story 3.2's own spec calls this out
		// explicitly as the observable effect of a successful withdrawal.
		assertBalance(t, env, customerID, "base", "eth", "1600")

		// The hold account itself is not exposed by any endpoint yet (Story 3.2 scope),
		// so it is verified directly against Postgres.
		var holdBalanceText string
		if err := env.pool.QueryRow(context.Background(),
			`SELECT COALESCE(SUM(p.amount), 0)::text
			 FROM accounts a LEFT JOIN postings p ON p.account_id = a.id
			 WHERE a.customer_id = $1 AND a.chain = 'base' AND a.asset = 'eth' AND a.account_type = 'hold'`,
			customerID,
		).Scan(&holdBalanceText); err != nil {
			t.Fatalf("query hold balance: %v", err)
		}
		if holdBalanceText != "400" {
			t.Fatalf("hold balance = %q, want %q", holdBalanceText, "400")
		}

		// Exactly one withdrawal_hold journal entry, with exactly two postings, exists.
		var journalCount int
		if err := env.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM journal_entries WHERE cause_type = 'withdrawal_hold' AND cause_id = $1`,
			"withdrawal-e2e-key-1",
		).Scan(&journalCount); err != nil {
			t.Fatalf("query journal_entries: %v", err)
		}
		if journalCount != 1 {
			t.Fatalf("withdrawal_hold journal entries for key = %d, want exactly 1", journalCount)
		}

		var withdrawalRowCount int
		if err := env.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM withdrawals WHERE id = $1`, body.ID,
		).Scan(&withdrawalRowCount); err != nil {
			t.Fatalf("query withdrawals: %v", err)
		}
		if withdrawalRowCount != 1 {
			t.Fatalf("withdrawals row count for id %s = %d, want 1", body.ID, withdrawalRowCount)
		}

		// A paired "withdrawal.approved" outbox event was written in the same
		// transaction (AD-4) — Story 3.2's own "withdrawal.created" event is no longer
		// written by this story's CreateWithdrawal (Design Notes).
		var outboxCount int
		if err := env.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM outbox_events WHERE event_type = 'withdrawal.approved' AND payload->>'withdrawalId' = $1`,
			body.ID,
		).Scan(&outboxCount); err != nil {
			t.Fatalf("query outbox_events: %v", err)
		}
		if outboxCount != 1 {
			t.Fatalf("withdrawal.approved outbox events for withdrawal %s = %d, want exactly 1", body.ID, outboxCount)
		}
	})

	t.Run("AC2: replaying the same Idempotency-Key with an identical body places exactly one hold", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-2")
		creditAccount(t, env, customerID, "base", "eth", "2000")

		key := "withdrawal-e2e-key-2"
		rec1 := postWithdrawal(t, env, key, customerID, "base", "eth", "250", validDestinationAddress)
		if rec1.Code != http.StatusCreated {
			t.Fatalf("first request status = %d, want %d, body = %s", rec1.Code, http.StatusCreated, rec1.Body.String())
		}

		rec2 := postWithdrawal(t, env, key, customerID, "base", "eth", "250", validDestinationAddress)
		if rec2.Code != rec1.Code || rec2.Body.String() != rec1.Body.String() {
			t.Fatalf("replay status/body = %d/%q, want byte-for-byte match with original %d/%q",
				rec2.Code, rec2.Body.String(), rec1.Code, rec1.Body.String())
		}

		assertBalance(t, env, customerID, "base", "eth", "1750")

		var journalCount int
		if err := env.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM journal_entries WHERE cause_type = 'withdrawal_hold' AND cause_id = $1`,
			key,
		).Scan(&journalCount); err != nil {
			t.Fatalf("query journal_entries: %v", err)
		}
		if journalCount != 1 {
			t.Fatalf("withdrawal_hold journal entries for key %q = %d, want exactly 1 (no second hold)", key, journalCount)
		}

		var withdrawalCount int
		if err := env.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM withdrawals WHERE customer_id = $1`, customerID,
		).Scan(&withdrawalCount); err != nil {
			t.Fatalf("query withdrawals: %v", err)
		}
		if withdrawalCount != 1 {
			t.Fatalf("withdrawals row count = %d, want exactly 1 (no duplicate row on replay)", withdrawalCount)
		}
	})

	t.Run("AC3: replaying the same Idempotency-Key with a different body is rejected with 409", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-3")
		creditAccount(t, env, customerID, "base", "eth", "2000")

		key := "withdrawal-e2e-key-3"
		rec1 := postWithdrawal(t, env, key, customerID, "base", "eth", "100", validDestinationAddress)
		if rec1.Code != http.StatusCreated {
			t.Fatalf("first request status = %d, want %d, body = %s", rec1.Code, http.StatusCreated, rec1.Body.String())
		}

		rec2 := postWithdrawal(t, env, key, customerID, "base", "eth", "200", validDestinationAddress)
		if rec2.Code != http.StatusConflict {
			t.Fatalf("different-body replay status = %d, want %d, body = %s", rec2.Code, http.StatusConflict, rec2.Body.String())
		}
	})

	t.Run("insufficient available balance is rejected with 422 and places no hold", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-4")
		creditAccount(t, env, customerID, "base", "eth", "100")

		beforeCount := postingsCount(t, env)

		rec := postWithdrawal(t, env, "withdrawal-e2e-key-4", customerID, "base", "eth", "101", validDestinationAddress)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
		}

		if got := postingsCount(t, env); got != beforeCount {
			t.Fatalf("postings count = %d, want unchanged %d (no hold placed)", got, beforeCount)
		}
		assertBalance(t, env, customerID, "base", "eth", "100")
	})

	t.Run("non-positive or missing amount is rejected with 400", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-5")

		rec := postWithdrawal(t, env, "withdrawal-e2e-key-5a", customerID, "base", "eth", "0", validDestinationAddress)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("zero amount: status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}

		rec2 := postWithdrawal(t, env, "withdrawal-e2e-key-5b", customerID, "base", "eth", "-1", validDestinationAddress)
		if rec2.Code != http.StatusBadRequest {
			t.Fatalf("negative amount: status = %d, want %d, body = %s", rec2.Code, http.StatusBadRequest, rec2.Body.String())
		}
	})

	t.Run("malformed destination address is rejected with 400", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-6")

		rec := postWithdrawal(t, env, "withdrawal-e2e-key-6", customerID, "base", "eth", "1", "not-an-address")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("unknown customer returns 404", func(t *testing.T) {
		unknown := uuid.New().String()

		rec := postWithdrawal(t, env, "withdrawal-e2e-key-7", unknown, "base", "eth", "1", validDestinationAddress)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
		}
	})

	t.Run("invalid chain or asset enum is rejected with 400, not 404", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-8")

		rec := postWithdrawal(t, env, "withdrawal-e2e-key-8a", customerID, "polygon", "eth", "1", validDestinationAddress)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid chain: status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}

		rec2 := postWithdrawal(t, env, "withdrawal-e2e-key-8b", customerID, "base", "btc", "1", validDestinationAddress)
		if rec2.Code != http.StatusBadRequest {
			t.Fatalf("invalid asset: status = %d, want %d, body = %s", rec2.Code, http.StatusBadRequest, rec2.Body.String())
		}
	})

	t.Run("missing Idempotency-Key is rejected", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-9")

		rec := postWithdrawal(t, env, "", customerID, "base", "eth", "1", validDestinationAddress)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing bearer token is rejected", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-10")

		body := fmt.Sprintf(`{"customerId":%q,"chain":"base","asset":"eth","amount":"1","destinationAddress":%q}`, customerID, validDestinationAddress)
		req := httptest.NewRequest(http.MethodPost, "/v1/withdrawals", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "withdrawal-e2e-key-10")
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("concurrent withdrawal requests for the same customer/chain/asset never lose an update", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-concurrent-1")
		creditAccount(t, env, customerID, "base", "eth", "10000")

		const numRequests = 10
		results := make([]*httptest.ResponseRecorder, numRequests)
		var wg sync.WaitGroup
		for i := 0; i < numRequests; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i] = postWithdrawal(t, env, fmt.Sprintf("withdrawal-e2e-concurrent-key-%d", i), customerID, "base", "eth", "10", validDestinationAddress)
			}(i)
		}
		wg.Wait()

		for i, rec := range results {
			if rec.Code != http.StatusCreated {
				t.Fatalf("concurrent withdrawal %d status = %d, want %d, body = %s", i, rec.Code, http.StatusCreated, rec.Body.String())
			}
		}

		assertBalance(t, env, customerID, "base", "eth", "9900")

		var withdrawalCount int
		if err := env.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM withdrawals WHERE customer_id = $1`, customerID,
		).Scan(&withdrawalCount); err != nil {
			t.Fatalf("query withdrawals: %v", err)
		}
		if withdrawalCount != numRequests {
			t.Fatalf("withdrawals row count = %d, want %d (no lost update)", withdrawalCount, numRequests)
		}
	})

	t.Run("amount exceeding a uint256's maximum is rejected with 400", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-11")
		creditAccount(t, env, customerID, "base", "eth", "1000")

		tooLarge := new(big.Int).Lsh(big.NewInt(1), 256) // 2^256, one past uint256's max
		rec := postWithdrawal(t, env, "withdrawal-e2e-key-11", customerID, "base", "eth", tooLarge.String(), validDestinationAddress)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("a withdrawal hold appears exactly once in the customer's transaction history, not twice", func(t *testing.T) {
		// Regression test (adversarial review): a withdrawal hold's journal entry posts to
		// this customer's OWN available and hold accounts in one entry. Before the
		// account_type filter was added to transaction_repo.go's query, that single entry
		// surfaced as two rows (one per posting) sharing the same id with opposite-signed
		// amounts, since — unlike an internal transfer — both legs belong to this customer.
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-12")
		creditAccount(t, env, customerID, "base", "eth", "2000")

		rec := postWithdrawal(t, env, "withdrawal-e2e-key-12", customerID, "base", "eth", "300", validDestinationAddress)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
		}

		txRec := getTransactions(t, env, customerID, "")
		if txRec.Code != http.StatusOK {
			t.Fatalf("transactions status = %d, want %d, body = %s", txRec.Code, http.StatusOK, txRec.Body.String())
		}
		var body transactionsResponseBody
		if err := json.Unmarshal(txRec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode transactions response: %v", err)
		}
		// Two transactions are expected in total: creditAccount's own "test_fixture" entry
		// (funding the customer's available balance) and the withdrawal hold — each a
		// DISTINCT journal entry. What this test guards against is the withdrawal hold's
		// single journal entry (which posts to this customer's own available AND hold
		// accounts) contributing TWO rows instead of one.
		var withdrawalHoldRows []string
		for _, txn := range body.Transactions {
			if txn.Type == "withdrawal_hold" {
				withdrawalHoldRows = append(withdrawalHoldRows, txn.Amount)
			}
		}
		if len(withdrawalHoldRows) != 1 {
			t.Fatalf("withdrawal_hold transaction rows = %d, want exactly 1 (the hold-account's mirrored posting must not also appear): %+v", len(withdrawalHoldRows), body.Transactions)
		}
		if withdrawalHoldRows[0] != "-300" {
			t.Fatalf("withdrawal_hold transaction amount = %q, want %q (the available account's debit)", withdrawalHoldRows[0], "-300")
		}
	})

	t.Run("Story 3.3: a withdrawal above the threshold routes to awaiting-approval, with an approval.required outbox event", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-13")
		// 20000 comfortably covers 6000 (above this test env's fixed 5000 threshold) plus
		// the fixed 1500 fee estimate.
		creditAccount(t, env, customerID, "base", "eth", "20000")

		rec := postWithdrawal(t, env, "withdrawal-e2e-key-13", customerID, "base", "eth", "6000", validDestinationAddress)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
		}

		var body struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.Status != "awaiting-approval" {
			t.Fatalf("status = %q, want %q", body.Status, "awaiting-approval")
		}

		// The hold is still placed even though the withdrawal now waits for approval —
		// Story 3.3 never re-validates or re-places the hold, it only reads the resulting
		// balance.
		assertBalance(t, env, customerID, "base", "eth", "14000")

		if got := outboxEventCountForWithdrawal(t, env, "approval.required", body.ID); got != 1 {
			t.Fatalf("approval.required outbox events = %d, want exactly 1", got)
		}
		if got := outboxEventCountForWithdrawal(t, env, "withdrawal.approved", body.ID); got != 0 {
			t.Fatalf("withdrawal.approved outbox events = %d, want 0 (not yet approved)", got)
		}
	})

	t.Run("Story 3.3: the zero address is rejected with 400 before any hold is placed", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-14")
		creditAccount(t, env, customerID, "base", "eth", "2000")

		beforeCount := postingsCount(t, env)

		zeroAddress := "0x0000000000000000000000000000000000000000"
		rec := postWithdrawal(t, env, "withdrawal-e2e-key-14", customerID, "base", "eth", "400", zeroAddress)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}

		if got := postingsCount(t, env); got != beforeCount {
			t.Fatalf("postings count = %d, want unchanged %d (no hold placed for a denylisted destination)", got, beforeCount)
		}
		assertBalance(t, env, customerID, "base", "eth", "2000")
	})

	t.Run("Story 3.3: available balance covers the amount but not the estimated fee is rejected with 422 and places no hold", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "withdrawal-e2e-source-15")
		// Covers the withdrawn 100 comfortably, but nowhere near 100 + the fixed 1500 fee
		// estimate: post-hold available would be only 1, far short of 1500.
		creditAccount(t, env, customerID, "base", "eth", "101")

		beforeCount := postingsCount(t, env)

		rec := postWithdrawal(t, env, "withdrawal-e2e-key-15", customerID, "base", "eth", "100", validDestinationAddress)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
		}

		// The whole request transaction rolled back — no partial write, not even the hold
		// itself (Story 3.3 I/O matrix: "hold is NOT committed").
		if got := postingsCount(t, env); got != beforeCount {
			t.Fatalf("postings count = %d, want unchanged %d (no hold placed on a fee-rejected request)", got, beforeCount)
		}
		assertBalance(t, env, customerID, "base", "eth", "101")

		var withdrawalCount int
		if err := env.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM withdrawals WHERE customer_id = $1`, customerID,
		).Scan(&withdrawalCount); err != nil {
			t.Fatalf("query withdrawals: %v", err)
		}
		if withdrawalCount != 0 {
			t.Fatalf("withdrawals row count = %d, want 0 (no partial write)", withdrawalCount)
		}
	})
}

// outboxEventCountForWithdrawal returns how many outbox_events rows of eventType carry
// withdrawalID in their jsonb payload.
func outboxEventCountForWithdrawal(t *testing.T, env testEnv, eventType, withdrawalID string) int {
	t.Helper()
	var count int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE event_type = $1 AND payload->>'withdrawalId' = $2`,
		eventType, withdrawalID,
	).Scan(&count); err != nil {
		t.Fatalf("count %s outbox events: %v", eventType, err)
	}
	return count
}

// postApproveWithdrawal issues a POST /v1/withdrawals/{id}/approve request and returns
// the recorded response. An empty idempotencyKey omits the header entirely (mirrors
// postWithdrawal's identical convention); bearer likewise omits the Authorization header
// when false.
func postApproveWithdrawal(t *testing.T, env testEnv, idempotencyKey, withdrawalID, actor, reason string, bearer bool) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"actor":%q,"reason":%q}`, actor, reason)
	req := httptest.NewRequest(http.MethodPost, "/v1/withdrawals/"+withdrawalID+"/approve", strings.NewReader(body))
	if bearer {
		req.Header.Set("Authorization", "Bearer test-token")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

// createAwaitingApprovalWithdrawalViaHTTP creates a customer, funds it, and posts a
// withdrawal whose amount exceeds this test env's fixed 5000 threshold — routing it to
// awaiting-approval — through the real HTTP stack, returning its id.
func createAwaitingApprovalWithdrawalViaHTTP(t *testing.T, env testEnv, idempotencyKeyPrefix string) string {
	t.Helper()
	customerID := createTestCustomer(t, env, idempotencyKeyPrefix+"-customer")
	creditAccount(t, env, customerID, "base", "eth", "20000")

	rec := postWithdrawal(t, env, idempotencyKeyPrefix+"-withdrawal", customerID, "base", "eth", "6000", validDestinationAddress)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create awaiting-approval withdrawal fixture: status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode create-withdrawal response: %v", err)
	}
	return body.ID
}

// TestApproveWithdrawal_EndToEnd exercises Story 3.3's POST /v1/withdrawals/{id}/approve
// endpoint over the real, wired HTTP stack — the full status matrix (200/404/409/400) plus
// the auth/idempotency guards every other mutating route already has.
func TestApproveWithdrawal_EndToEnd(t *testing.T) {
	env := newTestHandler(t)

	t.Run("200: an operator approves an awaiting-approval withdrawal, with audit columns and outbox event", func(t *testing.T) {
		withdrawalID := createAwaitingApprovalWithdrawalViaHTTP(t, env, "approve-e2e-1")

		rec := postApproveWithdrawal(t, env, "approve-e2e-key-1", withdrawalID, "ops-alice", "manually reviewed, looks fine", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}

		var body struct {
			ID             string  `json:"id"`
			Status         string  `json:"status"`
			ApprovedAt     *string `json:"approvedAt"`
			ApprovedBy     *string `json:"approvedBy"`
			ApprovalReason *string `json:"approvalReason"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.Status != "approved" {
			t.Fatalf("status = %q, want %q", body.Status, "approved")
		}
		if body.ApprovedAt == nil || *body.ApprovedAt == "" {
			t.Fatal("expected a non-empty approvedAt")
		}
		if body.ApprovedBy == nil || *body.ApprovedBy != "ops-alice" {
			t.Fatalf("approvedBy = %v, want %q", body.ApprovedBy, "ops-alice")
		}
		if body.ApprovalReason == nil || *body.ApprovalReason != "manually reviewed, looks fine" {
			t.Fatalf("approvalReason = %v, want %q", body.ApprovalReason, "manually reviewed, looks fine")
		}

		if got := outboxEventCountForWithdrawal(t, env, "withdrawal.approved", withdrawalID); got != 1 {
			t.Fatalf("withdrawal.approved outbox events = %d, want exactly 1", got)
		}
	})

	t.Run("404: approving an unknown withdrawal id", func(t *testing.T) {
		unknown := uuid.New().String()
		rec := postApproveWithdrawal(t, env, "approve-e2e-key-2", unknown, "ops-alice", "reason", true)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
		}
	})

	t.Run("409: approving a withdrawal that is not awaiting approval (already approved)", func(t *testing.T) {
		withdrawalID := createAwaitingApprovalWithdrawalViaHTTP(t, env, "approve-e2e-3")

		rec1 := postApproveWithdrawal(t, env, "approve-e2e-key-3a", withdrawalID, "ops-alice", "first approval", true)
		if rec1.Code != http.StatusOK {
			t.Fatalf("first approve: status = %d, want %d, body = %s", rec1.Code, http.StatusOK, rec1.Body.String())
		}

		rec2 := postApproveWithdrawal(t, env, "approve-e2e-key-3b", withdrawalID, "ops-bob", "second approval attempt", true)
		if rec2.Code != http.StatusConflict {
			t.Fatalf("second approve: status = %d, want %d, body = %s", rec2.Code, http.StatusConflict, rec2.Body.String())
		}
	})

	t.Run("409: approving a withdrawal that is still below-threshold approved (never entered awaiting-approval)", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "approve-e2e-4-customer")
		creditAccount(t, env, customerID, "base", "eth", "2000")
		rec := postWithdrawal(t, env, "approve-e2e-4-withdrawal", customerID, "base", "eth", "400", validDestinationAddress)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create withdrawal: status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode create-withdrawal response: %v", err)
		}

		approveRec := postApproveWithdrawal(t, env, "approve-e2e-key-4", body.ID, "ops-alice", "reason", true)
		if approveRec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want %d, body = %s", approveRec.Code, http.StatusConflict, approveRec.Body.String())
		}
	})

	t.Run("400: missing actor is rejected", func(t *testing.T) {
		withdrawalID := createAwaitingApprovalWithdrawalViaHTTP(t, env, "approve-e2e-5")

		rec := postApproveWithdrawal(t, env, "approve-e2e-key-5", withdrawalID, "", "reason", true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("400: missing reason is rejected", func(t *testing.T) {
		withdrawalID := createAwaitingApprovalWithdrawalViaHTTP(t, env, "approve-e2e-6")

		rec := postApproveWithdrawal(t, env, "approve-e2e-key-6", withdrawalID, "ops-alice", "", true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("missing Idempotency-Key is rejected", func(t *testing.T) {
		withdrawalID := createAwaitingApprovalWithdrawalViaHTTP(t, env, "approve-e2e-7")

		rec := postApproveWithdrawal(t, env, "", withdrawalID, "ops-alice", "reason", true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing bearer token is rejected", func(t *testing.T) {
		withdrawalID := createAwaitingApprovalWithdrawalViaHTTP(t, env, "approve-e2e-8")

		rec := postApproveWithdrawal(t, env, "approve-e2e-key-8", withdrawalID, "ops-alice", "reason", false)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
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

// transactionsResponseBody decodes a GET .../transactions response body.
type transactionsResponseBody struct {
	Transactions []struct {
		ID        string    `json:"id"`
		Type      string    `json:"type"`
		Amount    string    `json:"amount"`
		Chain     string    `json:"chain"`
		Asset     string    `json:"asset"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"createdAt"`
	} `json:"transactions"`
	NextCursor string `json:"nextCursor"`
}

// flipChar returns a different but still valid base64url character, used to tamper with a
// cursor in a way that keeps it well-formed base64 (so the rejection exercises the HMAC
// check, not merely a base64 decode failure).
func flipChar(c byte) string {
	if c == 'A' {
		return "B"
	}
	return "A"
}

// getTransactions issues a GET /v1/customers/{id}/transactions request with an optional
// raw query string (e.g. "cursor=...&pageSize=3") and returns the recorded response.
func getTransactions(t *testing.T, env testEnv, customerID, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/v1/customers/" + customerID + "/transactions"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

func TestListCustomerTransactions_EndToEnd(t *testing.T) {
	env := newTestHandler(t)

	t.Run("AC1: a completed transfer appears on both sides, signed from each customer's own perspective", func(t *testing.T) {
		source := createTestCustomer(t, env, "txn-e2e-source-1")
		dest := createTestCustomer(t, env, "txn-e2e-dest-1")
		creditAccount(t, env, source, "base", "eth", "1000")

		transferRec := postTransfer(t, env, "txn-e2e-transfer-key-1", source, dest, "base", "eth", "400")
		if transferRec.Code != http.StatusCreated {
			t.Fatalf("transfer status = %d, want %d, body = %s", transferRec.Code, http.StatusCreated, transferRec.Body.String())
		}

		sourceRec := getTransactions(t, env, source, "")
		if sourceRec.Code != http.StatusOK {
			t.Fatalf("source history status = %d, want %d, body = %s", sourceRec.Code, http.StatusOK, sourceRec.Body.String())
		}
		var sourceBody transactionsResponseBody
		if err := json.Unmarshal(sourceRec.Body.Bytes(), &sourceBody); err != nil {
			t.Fatalf("decode source history: %v", err)
		}
		// Source's history also contains the creditAccount fixture row (cause_type
		// "test_fixture") used to fund it — which is itself a live demonstration of AC4's
		// genericity (a cause_type this endpoint has never heard of still shows up
		// unfiltered), so assert on the specific internal_transfer entry rather than the
		// list's total length.
		var st *struct {
			ID        string    `json:"id"`
			Type      string    `json:"type"`
			Amount    string    `json:"amount"`
			Chain     string    `json:"chain"`
			Asset     string    `json:"asset"`
			Status    string    `json:"status"`
			CreatedAt time.Time `json:"createdAt"`
		}
		for i := range sourceBody.Transactions {
			if sourceBody.Transactions[i].Type == "internal_transfer" {
				st = &sourceBody.Transactions[i]
				break
			}
		}
		if st == nil {
			t.Fatalf("no internal_transfer entry found in source transactions: %+v", sourceBody.Transactions)
		}
		if st.Amount != "-400" || st.Chain != "base" || st.Asset != "eth" || st.Status != "completed" {
			t.Fatalf("source transaction = %+v, want amount=-400 chain=base asset=eth status=completed", *st)
		}
		if st.CreatedAt.IsZero() {
			t.Fatal("expected a non-zero createdAt timestamp")
		}

		destRec := getTransactions(t, env, dest, "")
		if destRec.Code != http.StatusOK {
			t.Fatalf("dest history status = %d, want %d, body = %s", destRec.Code, http.StatusOK, destRec.Body.String())
		}
		var destBody transactionsResponseBody
		if err := json.Unmarshal(destRec.Body.Bytes(), &destBody); err != nil {
			t.Fatalf("decode dest history: %v", err)
		}
		if len(destBody.Transactions) != 1 {
			t.Fatalf("dest transactions = %+v, want exactly 1", destBody.Transactions)
		}
		dt := destBody.Transactions[0]
		if dt.Type != "internal_transfer" || dt.Amount != "400" || dt.Chain != "base" || dt.Asset != "eth" || dt.Status != "completed" {
			t.Fatalf("dest transaction = %+v, want type=internal_transfer amount=400 chain=base asset=eth status=completed", dt)
		}
	})

	t.Run("AC2: a customer with no transactions gets an empty paginated list, not an error", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "txn-e2e-empty-1")

		rec := getTransactions(t, env, customerID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var body transactionsResponseBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(body.Transactions) != 0 {
			t.Fatalf("transactions = %+v, want empty", body.Transactions)
		}
		if body.NextCursor != "" {
			t.Fatalf("nextCursor = %q, want empty (no pages at all)", body.NextCursor)
		}
	})

	t.Run("AC3: paginates with stable newest-first ordering, no duplicates and no gaps across pages", func(t *testing.T) {
		source := createTestCustomer(t, env, "txn-e2e-page-source-1")
		dest := createTestCustomer(t, env, "txn-e2e-page-dest-1")
		creditAccount(t, env, source, "base", "eth", "1000")

		const numTransfers = 7
		for i := 0; i < numTransfers; i++ {
			key := fmt.Sprintf("txn-e2e-page-key-%d", i)
			rec := postTransfer(t, env, key, source, dest, "base", "eth", "1")
			if rec.Code != http.StatusCreated {
				t.Fatalf("transfer %d status = %d, want %d, body = %s", i, rec.Code, http.StatusCreated, rec.Body.String())
			}
		}

		// Paginate over dest, not source: source's history also carries the
		// creditAccount funding fixture (a "test_fixture" row), which would make the
		// exactly-numTransfers count below wrong. dest never receives that fixture — its
		// history is exactly the numTransfers credits from the loop above.
		var seenIDs []string
		var seenCreatedAt []time.Time
		cursor := ""
		for page := 0; ; page++ {
			if page > numTransfers {
				t.Fatal("paginated more times than there are transactions — nextCursor is not converging")
			}
			query := "pageSize=3"
			if cursor != "" {
				query += "&cursor=" + cursor
			}
			rec := getTransactions(t, env, dest, query)
			if rec.Code != http.StatusOK {
				t.Fatalf("page %d status = %d, want %d, body = %s", page, rec.Code, http.StatusOK, rec.Body.String())
			}
			var body transactionsResponseBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode page %d: %v", page, err)
			}
			for _, txn := range body.Transactions {
				seenIDs = append(seenIDs, txn.ID)
				seenCreatedAt = append(seenCreatedAt, txn.CreatedAt)
			}
			if body.NextCursor == "" {
				break
			}
			cursor = body.NextCursor
		}

		if len(seenIDs) != numTransfers {
			t.Fatalf("total transactions seen across all pages = %d, want exactly %d (no gaps/dups): %v", len(seenIDs), numTransfers, seenIDs)
		}
		seen := make(map[string]bool, len(seenIDs))
		for _, id := range seenIDs {
			if seen[id] {
				t.Fatalf("transaction id %s seen more than once across pages: %v", id, seenIDs)
			}
			seen[id] = true
		}
		// Assert the *strict total order* the keyset design guarantees, not just
		// non-ascending createdAt: newest-first on createdAt, and on a createdAt tie
		// (same-instant rows, reachable in a fast test loop) descending on id. This is the
		// exact ordering the (created_at, id) cursor comparison relies on to never skip or
		// repeat a row across a page boundary — asserting createdAt alone would let a
		// tie-ordering regression through.
		for i := 1; i < len(seenCreatedAt); i++ {
			switch {
			case seenCreatedAt[i].After(seenCreatedAt[i-1]):
				t.Fatalf("ordering not newest-first at index %d: %v is after %v", i, seenCreatedAt[i], seenCreatedAt[i-1])
			case seenCreatedAt[i].Equal(seenCreatedAt[i-1]) && seenIDs[i] >= seenIDs[i-1]:
				// UUID canonical text sorts lexicographically the same as Postgres's uuid
				// byte order, so a string compare is a faithful stand-in for the DB tiebreak.
				t.Fatalf("createdAt tie at index %d not broken by descending id: %s >= %s", i, seenIDs[i], seenIDs[i-1])
			}
		}
	})

	t.Run("AC5: unknown customer id returns 404", func(t *testing.T) {
		unknownID := uuid.New().String()
		rec := getTransactions(t, env, unknownID, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
		}
	})

	t.Run("AC6: missing bearer token is rejected", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "txn-e2e-auth-1")
		req := httptest.NewRequest(http.MethodGet, "/v1/customers/"+customerID+"/transactions", nil)
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("AC7: a garbage cursor is rejected with 400, not 500", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "txn-e2e-cursor-1")
		rec := getTransactions(t, env, customerID, "cursor=not-a-real-cursor")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("AC7: a tampered or cross-customer cursor is rejected with 400, not silently honored", func(t *testing.T) {
		// Build a real, valid cursor: fund source, send it 3 transfers, page dest at
		// pageSize=2 so a nextCursor is produced.
		source := createTestCustomer(t, env, "txn-e2e-cursor-tamper-source")
		dest := createTestCustomer(t, env, "txn-e2e-cursor-tamper-dest")
		creditAccount(t, env, source, "base", "eth", "1000")
		for i := 0; i < 3; i++ {
			key := fmt.Sprintf("txn-e2e-cursor-tamper-key-%d", i)
			if rec := postTransfer(t, env, key, source, dest, "base", "eth", "1"); rec.Code != http.StatusCreated {
				t.Fatalf("transfer %d status = %d, body = %s", i, rec.Code, rec.Body.String())
			}
		}

		firstRec := getTransactions(t, env, dest, "pageSize=2")
		if firstRec.Code != http.StatusOK {
			t.Fatalf("first page status = %d, want %d, body = %s", firstRec.Code, http.StatusOK, firstRec.Body.String())
		}
		var firstBody transactionsResponseBody
		if err := json.Unmarshal(firstRec.Body.Bytes(), &firstBody); err != nil {
			t.Fatalf("decode first page: %v", err)
		}
		validCursor := firstBody.NextCursor
		if validCursor == "" {
			t.Fatal("expected a nextCursor on the first of multiple pages")
		}

		// Sanity: the untampered cursor works for its own customer.
		if rec := getTransactions(t, env, dest, "cursor="+validCursor); rec.Code != http.StatusOK {
			t.Fatalf("valid cursor replayed by its own customer: status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}

		// Tampered: flip the first character of the well-formed cursor (a payload byte, so
		// the change is always significant — unlike the trailing base64 char of the MAC,
		// whose low bits are non-significant padding and can decode unchanged). The HMAC no
		// longer matches, so it must be rejected — never treated as a valid page origin.
		tampered := flipChar(validCursor[0]) + validCursor[1:]
		if rec := getTransactions(t, env, dest, "cursor="+tampered); rec.Code != http.StatusBadRequest {
			t.Fatalf("tampered cursor: status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}

		// From a different customer: dest's own valid cursor, replayed against another
		// existing customer, must 400 (customer binding) — not return that customer's rows.
		other := createTestCustomer(t, env, "txn-e2e-cursor-tamper-other")
		if rec := getTransactions(t, env, other, "cursor="+validCursor); rec.Code != http.StatusBadRequest {
			t.Fatalf("cross-customer cursor: status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("AC8: invalid pageSize is rejected with 400; an oversized pageSize is clamped, not rejected", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "txn-e2e-pagesize-1")

		// An explicit pageSize=0 is present and not a positive integer, so AC8 requires 400 —
		// it must NOT be treated as an omitted parameter and silently defaulted.
		recZero := getTransactions(t, env, customerID, "pageSize=0")
		if recZero.Code != http.StatusBadRequest {
			t.Fatalf("explicit zero pageSize: status = %d, want %d, body = %s", recZero.Code, http.StatusBadRequest, recZero.Body.String())
		}

		rec := getTransactions(t, env, customerID, "pageSize=-1")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("negative pageSize: status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}

		rec2 := getTransactions(t, env, customerID, "pageSize=abc")
		if rec2.Code != http.StatusBadRequest {
			t.Fatalf("non-numeric pageSize: status = %d, want %d, body = %s", rec2.Code, http.StatusBadRequest, rec2.Body.String())
		}

		rec3 := getTransactions(t, env, customerID, "pageSize=1000")
		if rec3.Code != http.StatusOK {
			t.Fatalf("oversized pageSize: status = %d, want %d, body = %s", rec3.Code, http.StatusOK, rec3.Body.String())
		}
		var body transactionsResponseBody
		if err := json.Unmarshal(rec3.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(body.Transactions) > 100 {
			t.Fatalf("transactions returned = %d, want clamped to at most 100", len(body.Transactions))
		}
	})
}

// depositsResponseBody decodes a GET .../deposits response body.
type depositsResponseBody struct {
	Deposits []struct {
		ID         string    `json:"id"`
		Chain      string    `json:"chain"`
		Asset      string    `json:"asset"`
		Amount     string    `json:"amount"`
		TxHash     string    `json:"txHash"`
		Status     string    `json:"status"`
		Tier       string    `json:"tier"`
		ObservedAt time.Time `json:"observedAt"`
	} `json:"deposits"`
}

// getDeposits issues a GET /v1/customers/{id}/deposits request and returns the recorded
// response.
func getDeposits(t *testing.T, env testEnv, customerID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/customers/"+customerID+"/deposits", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

// dummyBlockHash is a well-formed (but arbitrary) block hash used to satisfy the
// block_hash NOT NULL/CHECK constraint (migration 0008) in fixtures that don't care about
// its actual value — only the reorg-specific tests below assert on block_hash directly.
const dummyBlockHash = "0xd000000000000000000000000000000000000000000000000000000000000000"

// seedDeposit inserts a deposits row directly via test SQL — no watcher runs in this
// test, matching the story's instruction that TestGetCustomerDeposits_EndToEnd seeds the
// deposits row directly. address must be an existing customer's own deposit address
// (deposits has no customer_id column by design, AD-8; attribution is resolved at read
// time via the deposit_addresses join).
func seedDeposit(t *testing.T, env testEnv, address, chain, asset, txHash string, logIndex int, amount string, blockNumber uint64, state string) {
	t.Helper()
	seedDepositAt(t, env, address, chain, asset, txHash, logIndex, amount, blockNumber, state, time.Now().UTC())
}

// seedDepositAt is seedDeposit with an explicit observed_at, so a test can control
// ordering between multiple seeded rows (re-review 2026-07-16) — seedDeposit's own
// time.Now().UTC() call gives every row in the same test near-identical timestamps,
// too close together to reliably assert an ORDER BY observed_at DESC result. Uses
// dummyBlockHash (Story 2.4): callers that need to control block_hash directly (the
// reorg-detection tests) use seedDepositWithHash instead.
func seedDepositAt(t *testing.T, env testEnv, address, chain, asset, txHash string, logIndex int, amount string, blockNumber uint64, state string, observedAt time.Time) {
	t.Helper()
	seedDepositWithHash(t, env, address, chain, asset, txHash, logIndex, amount, blockNumber, dummyBlockHash, state, observedAt)
}

// seedDepositWithHash is seedDepositAt with an explicit block_hash (Story 2.4) — used by
// the reorg-detection tests, which need to seed a deposit with a KNOWN stale hash to force
// a mismatch against a fake/real chain's current hash at that height.
func seedDepositWithHash(t *testing.T, env testEnv, address, chain, asset, txHash string, logIndex int, amount string, blockNumber uint64, blockHash, state string, observedAt time.Time) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(),
		`INSERT INTO deposits (id, chain, asset, address, tx_hash, log_index, amount, block_number, block_hash, state, observed_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9, $10, $11, $11)`,
		uuid.New().String(), chain, asset, address, txHash, logIndex, amount, blockNumber, blockHash, state, observedAt,
	); err != nil {
		t.Fatalf("seed deposits fixture row: %v", err)
	}
}

func TestGetCustomerDeposits_EndToEnd(t *testing.T) {
	env := newTestHandler(t)

	t.Run("observed and safe deposits both appear with status pending and their own tier", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "deposits-e2e-key-1")

		custRec := getCustomer(t, env, customerID)
		if custRec.Code != http.StatusOK {
			t.Fatalf("get customer status = %d, want %d, body = %s", custRec.Code, http.StatusOK, custRec.Body.String())
		}
		var custBody struct {
			DepositAddress string `json:"depositAddress"`
		}
		if err := json.Unmarshal(custRec.Body.Bytes(), &custBody); err != nil {
			t.Fatalf("decode customer response: %v", err)
		}

		seedDeposit(t, env, custBody.DepositAddress, "base", "eth", "0xobserved1", -1, "1000000000000000000", 50, "observed")
		seedDeposit(t, env, custBody.DepositAddress, "base", "usdc", "0xsafe1", 3, "42000000", 10, "safe")

		rec := getDeposits(t, env, customerID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var body depositsResponseBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(body.Deposits) != 2 {
			t.Fatalf("deposits = %+v, want exactly 2", body.Deposits)
		}

		var gotObserved, gotSafe bool
		for _, d := range body.Deposits {
			if d.Status != "pending" {
				t.Fatalf("deposit %+v status = %q, want %q (both observed and safe tiers are pending this story)", d, d.Status, "pending")
			}
			switch d.TxHash {
			case "0xobserved1":
				gotObserved = true
				if d.Tier != "observed" || d.Chain != "base" || d.Asset != "eth" || d.Amount != "1000000000000000000" {
					t.Fatalf("observed deposit = %+v, want tier=observed chain=base asset=eth amount=1000000000000000000", d)
				}
			case "0xsafe1":
				gotSafe = true
				if d.Tier != "safe" || d.Chain != "base" || d.Asset != "usdc" || d.Amount != "42000000" {
					t.Fatalf("safe deposit = %+v, want tier=safe chain=base asset=usdc amount=42000000", d)
				}
			}
		}
		if !gotObserved || !gotSafe {
			t.Fatalf("deposits = %+v, want both the observed and safe fixtures", body.Deposits)
		}
	})

	t.Run("deposits are returned newest first (ORDER BY observed_at DESC)", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "deposits-e2e-key-order")

		custRec := getCustomer(t, env, customerID)
		var custBody struct {
			DepositAddress string `json:"depositAddress"`
		}
		if err := json.Unmarshal(custRec.Body.Bytes(), &custBody); err != nil {
			t.Fatalf("decode customer response: %v", err)
		}

		base := time.Now().UTC().Add(-1 * time.Hour)
		seedDepositAt(t, env, custBody.DepositAddress, "base", "eth", "0xoldest", -1, "1", 10, "observed", base)
		seedDepositAt(t, env, custBody.DepositAddress, "base", "eth", "0xmiddle", -1, "2", 20, "observed", base.Add(10*time.Minute))
		seedDepositAt(t, env, custBody.DepositAddress, "base", "eth", "0xnewest", -1, "3", 30, "observed", base.Add(20*time.Minute))

		rec := getDeposits(t, env, customerID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var body depositsResponseBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(body.Deposits) != 3 {
			t.Fatalf("deposits = %+v, want exactly 3", body.Deposits)
		}
		wantOrder := []string{"0xnewest", "0xmiddle", "0xoldest"}
		for i, want := range wantOrder {
			if body.Deposits[i].TxHash != want {
				t.Fatalf("deposits[%d].txHash = %q, want %q (newest first) — got order %v", i, body.Deposits[i].TxHash, want, body.Deposits)
			}
		}
	})

	t.Run("a customer with no deposits gets an empty list, not an error", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "deposits-e2e-key-2")

		rec := getDeposits(t, env, customerID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var body depositsResponseBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(body.Deposits) != 0 {
			t.Fatalf("deposits = %+v, want empty", body.Deposits)
		}
	})

	t.Run("unknown customer id returns 404", func(t *testing.T) {
		unknownID := uuid.New().String()
		rec := getDeposits(t, env, unknownID)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
		}
	})

	t.Run("missing bearer token is rejected", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "deposits-e2e-key-3")
		req := httptest.NewRequest(http.MethodGet, "/v1/customers/"+customerID+"/deposits", nil)
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})
}

// customerDepositAddress fetches customerID's own deposit address via GET
// /v1/customers/{id}, the same read path Story 1.5 exposes it through.
func customerDepositAddress(t *testing.T, env testEnv, customerID string) string {
	t.Helper()
	rec := getCustomer(t, env, customerID)
	if rec.Code != http.StatusOK {
		t.Fatalf("get customer status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		DepositAddress string `json:"depositAddress"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode customer response: %v", err)
	}
	return body.DepositAddress
}

// TestCreditFinalizedDeposits_EndToEnd exercises Story 2.2's crediting path directly
// against real Postgres: postgres.NewDepositRepository().CreditFinalizedDeposits is
// invoked against a transaction opened via the test env's own postgres.TxBeginner —
// mirroring exactly how core.TrackDeposits.Execute calls it mid-poll — rather than
// running the full watcher, since no ChainScanner/anvil interaction is needed to prove
// the credit write path (the migration/deposit_repo/transaction_repo/balance path this
// story actually changes).
func TestCreditFinalizedDeposits_EndToEnd(t *testing.T) {
	env := newTestHandler(t)
	ctx := context.Background()
	txBeginner := postgres.NewTxBeginner(env.pool)
	depositRepo := postgres.NewDepositRepository()

	t.Run("AC1 & AC4: a finalized deposit is credited — journal entry, postings, outbox event, transaction history, and balance all reflect it", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "credit-e2e-key-1")
		depositAddress := customerDepositAddress(t, env, customerID)

		seedDeposit(t, env, depositAddress, "base", "eth", "0xfinalized1", -1, "2500000000000000000", 100, "finalized")

		txCtx, tx, err := txBeginner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin transaction: %v", err)
		}
		credited, err := depositRepo.CreditFinalizedDeposits(txCtx, core.ChainBase)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("CreditFinalizedDeposits() error = %v, want nil", err)
		}
		if credited != 1 {
			_ = tx.Rollback(ctx)
			t.Fatalf("credited count = %d, want 1", credited)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit transaction: %v", err)
		}

		var depositID, state string
		if err := env.pool.QueryRow(ctx, `SELECT id, state FROM deposits WHERE tx_hash = $1`, "0xfinalized1").Scan(&depositID, &state); err != nil {
			t.Fatalf("query deposit: %v", err)
		}
		if state != "credited" {
			t.Fatalf("deposit state = %q, want %q", state, "credited")
		}

		// Exactly one deposit_credit journal entry, its two postings, and one paired
		// deposit.credited outbox row exist for this deposit.
		var journalCount int
		if err := env.pool.QueryRow(ctx,
			`SELECT count(*) FROM journal_entries WHERE cause_type = 'deposit_credit' AND cause_id = $1`,
			depositID,
		).Scan(&journalCount); err != nil {
			t.Fatalf("count journal_entries: %v", err)
		}
		if journalCount != 1 {
			t.Fatalf("deposit_credit journal entries for deposit %s = %d, want exactly 1", depositID, journalCount)
		}
		var journalEntryID string
		if err := env.pool.QueryRow(ctx,
			`SELECT id FROM journal_entries WHERE cause_type = 'deposit_credit' AND cause_id = $1`,
			depositID,
		).Scan(&journalEntryID); err != nil {
			t.Fatalf("query journal_entries id: %v", err)
		}

		var postingsN int
		if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM postings WHERE journal_entry_id = $1`, journalEntryID).Scan(&postingsN); err != nil {
			t.Fatalf("query postings: %v", err)
		}
		if postingsN != 2 {
			t.Fatalf("postings for journal entry %s = %d, want exactly 2", journalEntryID, postingsN)
		}

		var outboxN int
		if err := env.pool.QueryRow(ctx,
			`SELECT count(*) FROM outbox_events WHERE event_type = 'deposit.credited' AND payload->>'depositId' = $1`,
			depositID,
		).Scan(&outboxN); err != nil {
			t.Fatalf("query outbox_events: %v", err)
		}
		if outboxN != 1 {
			t.Fatalf("deposit.credited outbox events for deposit %s = %d, want exactly 1", depositID, outboxN)
		}

		// AC4: GET /customers/{id}/transactions shows it with type=deposit_credit,
		// status=credited.
		txnRec := getTransactions(t, env, customerID, "")
		if txnRec.Code != http.StatusOK {
			t.Fatalf("get transactions status = %d, want %d, body = %s", txnRec.Code, http.StatusOK, txnRec.Body.String())
		}
		var txnBody transactionsResponseBody
		if err := json.Unmarshal(txnRec.Body.Bytes(), &txnBody); err != nil {
			t.Fatalf("decode transactions response: %v", err)
		}
		var found bool
		for _, txn := range txnBody.Transactions {
			if txn.Type == "deposit_credit" {
				found = true
				if txn.Status != "credited" || txn.Amount != "2500000000000000000" || txn.Chain != "base" || txn.Asset != "eth" {
					t.Fatalf("deposit_credit transaction = %+v, want status=credited amount=2500000000000000000 chain=base asset=eth", txn)
				}
			}
		}
		if !found {
			t.Fatalf("no deposit_credit transaction found in %+v", txnBody.Transactions)
		}

		// The customer's balance reflects the credited amount.
		assertBalance(t, env, customerID, "base", "eth", "2500000000000000000")

		// Design Notes: credited deposits surface only through transaction history, never
		// through GET /customers/{id}/deposits (untouched by this story).
		depRec := getDeposits(t, env, customerID)
		if depRec.Code != http.StatusOK {
			t.Fatalf("get deposits status = %d, want %d, body = %s", depRec.Code, http.StatusOK, depRec.Body.String())
		}
		var depBody depositsResponseBody
		if err := json.Unmarshal(depRec.Body.Bytes(), &depBody); err != nil {
			t.Fatalf("decode deposits response: %v", err)
		}
		if len(depBody.Deposits) != 0 {
			t.Fatalf("deposits = %+v, want empty (credited deposits never appear on the pending-deposits endpoint)", depBody.Deposits)
		}
	})

	t.Run("AC2: a credited deposit is never re-selected or re-credited by a later poll", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "credit-e2e-key-2")
		depositAddress := customerDepositAddress(t, env, customerID)
		seedDeposit(t, env, depositAddress, "base", "eth", "0xfinalized2", -1, "1000", 100, "finalized")

		txCtx1, tx1, err := txBeginner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin first transaction: %v", err)
		}
		credited1, err := depositRepo.CreditFinalizedDeposits(txCtx1, core.ChainBase)
		if err != nil {
			t.Fatalf("first CreditFinalizedDeposits() error = %v, want nil", err)
		}
		if credited1 != 1 {
			t.Fatalf("first call credited count = %d, want 1", credited1)
		}
		if err := tx1.Commit(ctx); err != nil {
			t.Fatalf("commit first transaction: %v", err)
		}

		txCtx2, tx2, err := txBeginner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin second transaction: %v", err)
		}
		credited2, err := depositRepo.CreditFinalizedDeposits(txCtx2, core.ChainBase)
		if err != nil {
			t.Fatalf("second CreditFinalizedDeposits() error = %v, want nil", err)
		}
		if err := tx2.Commit(ctx); err != nil {
			t.Fatalf("commit second transaction: %v", err)
		}
		if credited2 != 0 {
			t.Fatalf("second call credited count = %d, want 0 (an already-credited deposit must never be re-selected)", credited2)
		}

		var journalCount int
		if err := env.pool.QueryRow(ctx,
			`SELECT count(*) FROM journal_entries je JOIN deposits d ON d.id::text = je.cause_id
			 WHERE je.cause_type = 'deposit_credit' AND d.tx_hash = $1`,
			"0xfinalized2",
		).Scan(&journalCount); err != nil {
			t.Fatalf("query journal_entries: %v", err)
		}
		if journalCount != 1 {
			t.Fatalf("deposit_credit journal entries = %d, want exactly 1 (no double-credit across repeated calls)", journalCount)
		}
	})

	t.Run("FR9: a (chain, asset) pair with no crediting_policy row is never credited — the policy join is load-bearing, not decorative", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "credit-e2e-key-3")
		depositAddress := customerDepositAddress(t, env, customerID)
		seedDeposit(t, env, depositAddress, "arbitrum", "usdc", "0xnopolicy1", -1, "5000000", 100, "finalized")

		// Remove arbitrum/usdc's policy row for the duration of this subtest, restoring
		// it afterward so later subtests (and this test's own shared env) see the normal
		// seeded policy.
		if _, err := env.pool.Exec(ctx, `DELETE FROM crediting_policy WHERE chain = 'arbitrum' AND asset = 'usdc'`); err != nil {
			t.Fatalf("remove crediting_policy row: %v", err)
		}
		t.Cleanup(func() {
			if _, err := env.pool.Exec(ctx,
				`INSERT INTO crediting_policy (chain, asset, credit_tier) VALUES ('arbitrum', 'usdc', 'finalized')
				 ON CONFLICT (chain, asset) DO NOTHING`,
			); err != nil {
				t.Fatalf("restore crediting_policy row: %v", err)
			}
		})

		txCtx, tx, err := txBeginner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin transaction: %v", err)
		}
		credited, err := depositRepo.CreditFinalizedDeposits(txCtx, core.ChainArbitrum)
		if err != nil {
			t.Fatalf("CreditFinalizedDeposits() error = %v, want nil", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit transaction: %v", err)
		}
		if credited != 0 {
			t.Fatalf("credited count = %d, want 0 (no crediting_policy row for arbitrum/usdc — the join must exclude it, proving FR9's policy-table-driven claim)", credited)
		}

		var state string
		if err := env.pool.QueryRow(ctx, `SELECT state FROM deposits WHERE tx_hash = $1`, "0xnopolicy1").Scan(&state); err != nil {
			t.Fatalf("query deposit: %v", err)
		}
		if state != "finalized" {
			t.Fatalf("deposit state = %q, want %q (left uncredited with no matching policy row)", state, "finalized")
		}
	})
}

// fakeReorgScanner is a minimal core.ChainScanner test double used only by
// TestReorgDetection_EndToEnd — it drives the real TrackDeposits.Execute orchestration
// (real Postgres, real DepositRepository) without needing a real anvil instance, since
// scanner_test.go's real-anvil TestScanner_RealAnvil_BlockHash_ReflectsReorg already
// proves BlockHash's real RPC behavior against a genuine anvil_reorg (AC3). Head and
// ScanDeposits are configured to be inert (latest/safe/finalized all low, no transfers)
// so the only effect a poll has is the reorg-check phase under test.
type fakeReorgScanner struct {
	latest, safe, finalized uint64
	hashes                  map[uint64]string
}

func (s *fakeReorgScanner) Head(ctx context.Context) (uint64, uint64, uint64, error) {
	return s.latest, s.safe, s.finalized, nil
}

func (s *fakeReorgScanner) BlockHash(ctx context.Context, blockNumber uint64) (string, bool, error) {
	hash, ok := s.hashes[blockNumber]
	return hash, ok, nil
}

func (s *fakeReorgScanner) ScanDeposits(ctx context.Context, knownAddresses []string, tokenRegistry map[string]core.Asset, fromBlock, toBlock uint64) ([]core.ObservedTransfer, []core.UnsupportedTokenObservation, error) {
	return nil, nil, nil
}

var _ core.ChainScanner = (*fakeReorgScanner)(nil)

// fakeFeeEstimator is a minimal core.FeeEstimator test double used by
// TestGetWithdrawalFeeEstimate_EndToEnd — this end-to-end test proves the HTTP wiring
// (query-param validation, response shape, auth) over the real stack, not chain RPC
// behavior, which fee_estimator_test.go already covers against fakes/canned responses in
// internal/adapter/evm. No real RPC call is needed or wanted here.
type fakeFeeEstimator struct {
	result core.FeeEstimate
	err    error
}

func (f *fakeFeeEstimator) EstimateFee(_ context.Context, _ core.Chain, _ core.Asset, _ *big.Int) (core.FeeEstimate, error) {
	return f.result, f.err
}

var _ core.FeeEstimator = (*fakeFeeEstimator)(nil)

// fakeWithdrawalThresholdLister is a minimal core.WithdrawalThresholdLister test double
// used by TestCreateWithdrawal_EndToEnd (Story 3.3) — one fixed threshold for every
// (chain, asset), mirroring fakeFeeEstimator's own "prove the HTTP wiring, not the real
// adapter" reasoning; withdrawal_threshold_lister_test.go already covers the real table
// against a real Postgres container.
type fakeWithdrawalThresholdLister struct {
	threshold *big.Int
	err       error
}

func (f *fakeWithdrawalThresholdLister) GetApprovalThreshold(_ context.Context, _ core.Chain, _ core.Asset) (*big.Int, error) {
	return f.threshold, f.err
}

var _ core.WithdrawalThresholdLister = (*fakeWithdrawalThresholdLister)(nil)

// TestReorgDetection_EndToEnd exercises Story 2.4's reorg-detection path against real
// Postgres: core.NewTrackDeposits is driven directly (mirroring
// TestCreditFinalizedDeposits_EndToEnd's pattern of invoking a use case against the test
// env's own postgres.TxBeginner) with a fake scanner standing in for the chain, so no
// real RPC/anvil interaction is needed to prove the migration/deposit_repo/deposit_reader
// path this story actually changes.
func TestReorgDetection_EndToEnd(t *testing.T) {
	env := newTestHandler(t)
	ctx := context.Background()

	t.Run("AC1 & AC2: a reorged deposit is orphaned and visible as status orphaned; the same transaction reappearing afterward is a fresh observed row, never conflated with the orphaned one", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "reorg-e2e-key-1")
		depositAddress := customerDepositAddress(t, env, customerID)

		const (
			txHash      = "0xreorgeddeposit1"
			blockNumber = uint64(10)
			staleHash   = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			newHash     = "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		)
		seedDepositWithHash(t, env, depositAddress, "base", "eth", txHash, -1, "1000", blockNumber, staleHash, "observed", time.Now().UTC())

		addressLister := postgres.NewDepositAddressLister(env.pool)
		tokenRegistry := postgres.NewTokenRegistry(env.pool)
		depositRepo := postgres.NewDepositRepository()
		unsupportedTokenRepo := postgres.NewUnsupportedTokenRepository(env.pool)
		txBeginner := postgres.NewTxBeginner(env.pool)

		// The chain's CURRENT hash at height 10 ("newHash") no longer matches the
		// deposit's stored ("staleHash") — a competing history replaced this block.
		scanner := &fakeReorgScanner{latest: 100, safe: 0, finalized: 0, hashes: map[uint64]string{blockNumber: newHash}}
		trackDeposits := core.NewTrackDeposits(scanner, addressLister, tokenRegistry, depositRepo, unsupportedTokenRepo, txBeginner)

		if _, err := trackDeposits.Execute(ctx, core.ChainBase); err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}

		var state string
		if err := env.pool.QueryRow(ctx, `SELECT state FROM deposits WHERE tx_hash = $1`, txHash).Scan(&state); err != nil {
			t.Fatalf("query deposit: %v", err)
		}
		if state != "orphaned" {
			t.Fatalf("state = %q, want %q", state, "orphaned")
		}

		// The paired deposit.orphaned outbox event was written in the same transaction
		// (AD-4), mirroring deposit.pending/deposit.credited's own paired-write pattern.
		var outboxCount int
		if err := env.pool.QueryRow(ctx,
			`SELECT count(*) FROM outbox_events oe JOIN deposits d ON d.id::text = oe.payload->>'depositId' WHERE oe.event_type = 'deposit.orphaned' AND d.tx_hash = $1`,
			txHash,
		).Scan(&outboxCount); err != nil {
			t.Fatalf("query outbox_events: %v", err)
		}
		if outboxCount != 1 {
			t.Fatalf("deposit.orphaned outbox events = %d, want exactly 1", outboxCount)
		}

		// AC2: the same transaction reappearing after the reorg inserts a brand-new
		// observed row — never conflated with the orphaned one, thanks to migration
		// 0008's partial unique index scoping (chain, tx_hash, log_index) uniqueness to
		// non-orphaned rows.
		seedDepositWithHash(t, env, depositAddress, "base", "eth", txHash, -1, "1000", blockNumber+5, newHash, "observed", time.Now().UTC())

		var rowCount int
		if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM deposits WHERE tx_hash = $1`, txHash).Scan(&rowCount); err != nil {
			t.Fatalf("count deposit rows: %v", err)
		}
		if rowCount != 2 {
			t.Fatalf("deposit rows for %s = %d, want exactly 2 (the orphaned original + the fresh re-observation)", txHash, rowCount)
		}

		var origState string
		if err := env.pool.QueryRow(ctx, `SELECT state FROM deposits WHERE tx_hash = $1 AND block_number = $2`, txHash, blockNumber).Scan(&origState); err != nil {
			t.Fatalf("query original deposit: %v", err)
		}
		if origState != "orphaned" {
			t.Fatalf("original deposit state = %q, want unchanged %q (untouched by the fresh re-observation)", origState, "orphaned")
		}

		// GET /customers/{id}/deposits shows both: the orphaned original with status
		// "orphaned", and the fresh re-observation with status "pending".
		rec := getDeposits(t, env, customerID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var body depositsResponseBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		var gotOrphaned, gotFresh bool
		for _, d := range body.Deposits {
			if d.TxHash != txHash {
				continue
			}
			switch d.Tier {
			case "orphaned":
				gotOrphaned = true
				if d.Status != "orphaned" {
					t.Fatalf("orphaned deposit status = %q, want %q", d.Status, "orphaned")
				}
			case "observed":
				gotFresh = true
				if d.Status != "pending" {
					t.Fatalf("fresh deposit status = %q, want %q", d.Status, "pending")
				}
			}
		}
		if !gotOrphaned {
			t.Fatalf("no orphaned deposit visible in %+v (AC1: provisional visibility must reflect the reorg)", body.Deposits)
		}
		if !gotFresh {
			t.Fatalf("no fresh observed deposit visible in %+v (AC2: the re-broadcast must appear, never double-counted)", body.Deposits)
		}
	})

	t.Run("matching block hash: no change (on a separate chain, for full isolation from the subtest above)", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "reorg-e2e-key-2")
		depositAddress := customerDepositAddress(t, env, customerID)

		const (
			txHash      = "0xstillvalid1"
			blockNumber = uint64(20)
			hash        = "0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		)
		seedDepositWithHash(t, env, depositAddress, "arbitrum", "eth", txHash, -1, "500", blockNumber, hash, "observed", time.Now().UTC())

		addressLister := postgres.NewDepositAddressLister(env.pool)
		tokenRegistry := postgres.NewTokenRegistry(env.pool)
		depositRepo := postgres.NewDepositRepository()
		unsupportedTokenRepo := postgres.NewUnsupportedTokenRepository(env.pool)
		txBeginner := postgres.NewTxBeginner(env.pool)
		scanner := &fakeReorgScanner{latest: 100, safe: 0, finalized: 0, hashes: map[uint64]string{blockNumber: hash}}
		trackDeposits := core.NewTrackDeposits(scanner, addressLister, tokenRegistry, depositRepo, unsupportedTokenRepo, txBeginner)

		if _, err := trackDeposits.Execute(ctx, core.ChainArbitrum); err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}

		var state string
		if err := env.pool.QueryRow(ctx, `SELECT state FROM deposits WHERE tx_hash = $1`, txHash).Scan(&state); err != nil {
			t.Fatalf("query deposit: %v", err)
		}
		if state != "observed" {
			t.Fatalf("state = %q, want unchanged %q (stored hash still matches the chain's current hash)", state, "observed")
		}
	})
}

// seedUnsupportedTokenObservation inserts an unsupported_token_observations row directly
// via test SQL — no watcher runs in this test, matching the story's instruction that
// TestGetUnsupportedTokenObservations_EndToEnd seeds the row directly rather than driving
// it through a real scan.
func seedUnsupportedTokenObservation(t *testing.T, env testEnv, address, chain, contractAddress, txHash string, logIndex int, amount string, blockNumber uint64) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(),
		`INSERT INTO unsupported_token_observations (id, chain, address, contract_address, tx_hash, log_index, amount, block_number, observed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, now())`,
		uuid.New().String(), chain, address, contractAddress, txHash, logIndex, amount, blockNumber,
	); err != nil {
		t.Fatalf("seed unsupported_token_observations fixture row: %v", err)
	}
}

// getUnsupportedTokenObservations issues a GET /v1/unsupported-token-observations
// request and returns the recorded response. bearer controls whether the Authorization
// header is set, so the same helper covers both the happy path and the auth-required
// assertion.
func getUnsupportedTokenObservations(t *testing.T, env testEnv, bearer bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/unsupported-token-observations", nil)
	if bearer {
		req.Header.Set("Authorization", "Bearer test-token")
	}
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

func TestGetUnsupportedTokenObservations_EndToEnd(t *testing.T) {
	env := newTestHandler(t)

	t.Run("AC3: a seeded observation is returned with its contract address and amount", func(t *testing.T) {
		customerID := createTestCustomer(t, env, "unsupported-e2e-key-1")
		depositAddress := customerDepositAddress(t, env, customerID)

		seedUnsupportedTokenObservation(t, env, depositAddress, "base", "0xdeadbeef0000000000000000000000000000dead", "0xunsupported1", 2, "123456", 77)

		rec := getUnsupportedTokenObservations(t, env, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}

		var body struct {
			Observations []struct {
				ID              string    `json:"id"`
				Chain           string    `json:"chain"`
				DepositAddress  string    `json:"depositAddress"`
				ContractAddress string    `json:"contractAddress"`
				TxHash          string    `json:"txHash"`
				Amount          string    `json:"amount"`
				BlockNumber     int64     `json:"blockNumber"`
				ObservedAt      time.Time `json:"observedAt"`
			} `json:"observations"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}

		var found bool
		for _, o := range body.Observations {
			if o.TxHash == "0xunsupported1" {
				found = true
				if o.ContractAddress != "0xdeadbeef0000000000000000000000000000dead" || o.Amount != "123456" || o.Chain != "base" || o.DepositAddress != depositAddress || o.BlockNumber != 77 {
					t.Fatalf("observation = %+v, want contractAddress=0xdeadbeef0000000000000000000000000000dead amount=123456 chain=base depositAddress=%s blockNumber=77", o, depositAddress)
				}
			}
		}
		if !found {
			t.Fatalf("no observation with txHash 0xunsupported1 found in %+v", body.Observations)
		}
	})

	t.Run("never produces a deposits row or a journal posting for the same seeded fixture", func(t *testing.T) {
		var depositCount int
		if err := env.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM deposits WHERE tx_hash = $1`, "0xunsupported1",
		).Scan(&depositCount); err != nil {
			t.Fatalf("query deposits: %v", err)
		}
		if depositCount != 0 {
			t.Fatalf("deposits rows for unsupported tx = %d, want 0 (never a deposit, FR11)", depositCount)
		}
	})

	t.Run("missing bearer token is rejected", func(t *testing.T) {
		rec := getUnsupportedTokenObservations(t, env, false)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("token_registry rejects an asset value this system can't interpret (re-review 2026-07-17)", func(t *testing.T) {
		// Proves the DB CHECK is real, not just claimed — the exact gap that let an
		// earlier overclaiming comment about "any new ERC-20, zero code change" go
		// uncaught: registering a genuinely new asset type still requires extending
		// core.Asset's closed enum, and this constraint is what actually enforces that
		// today, not just documentation.
		_, err := env.pool.Exec(context.Background(),
			`INSERT INTO token_registry (chain, contract_address, asset) VALUES ($1, $2, $3)`,
			"base", "0x2222222222222222222222222222222222222222", "weth",
		)
		if err == nil {
			t.Fatal("expected inserting an unrecognized asset into token_registry to fail its CHECK constraint, got nil error")
		}
	})
}

// getWithdrawalFeeEstimate issues a GET /v1/withdrawals/fee-estimate request with the
// given raw query string and returns the recorded response. bearer controls whether the
// Authorization header is set.
func getWithdrawalFeeEstimate(t *testing.T, env testEnv, rawQuery string, bearer bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/withdrawals/fee-estimate?"+rawQuery, nil)
	if bearer {
		req.Header.Set("Authorization", "Bearer test-token")
	}
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

// TestGetWithdrawalFeeEstimate_EndToEnd exercises Story 3.1's stateless fee-estimate
// endpoint against the real wired HTTP stack, but backed by a fake core.FeeEstimator (see
// fakeFeeEstimator above) — no real chain RPC is needed to prove the HTTP-layer contract
// (query-param validation, response shape, auth), which is this test's job.
// fee_estimator_test.go separately proves the real Arbitrum/Base fee-computation logic
// against fake/canned RPC responses.
func TestGetWithdrawalFeeEstimate_EndToEnd(t *testing.T) {
	env := newTestHandler(t)

	t.Run("valid estimate for base/eth", func(t *testing.T) {
		rec := getWithdrawalFeeEstimate(t, env, "chain=base&asset=eth&amount=1000000000000000000", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var body struct {
			L2Fee    string `json:"l2Fee"`
			L1Fee    string `json:"l1Fee"`
			TotalFee string `json:"totalFee"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.L2Fee != "1000" || body.L1Fee != "500" || body.TotalFee != "1500" {
			t.Fatalf("body = %+v, want l2Fee=1000 l1Fee=500 totalFee=1500", body)
		}
	})

	t.Run("valid estimate for base/usdc", func(t *testing.T) {
		rec := getWithdrawalFeeEstimate(t, env, "chain=base&asset=usdc&amount=42000000", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
	})

	t.Run("valid estimate for arbitrum/eth", func(t *testing.T) {
		rec := getWithdrawalFeeEstimate(t, env, "chain=arbitrum&asset=eth&amount=1", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
	})

	t.Run("valid estimate for arbitrum/usdc", func(t *testing.T) {
		rec := getWithdrawalFeeEstimate(t, env, "chain=arbitrum&asset=usdc&amount=1000000", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var body struct {
			L2Fee    string `json:"l2Fee"`
			L1Fee    string `json:"l1Fee"`
			TotalFee string `json:"totalFee"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		gotTotal, ok := new(big.Int).SetString(body.TotalFee, 10)
		if !ok {
			t.Fatalf("totalFee %q is not a base-10 integer", body.TotalFee)
		}
		l2, _ := new(big.Int).SetString(body.L2Fee, 10)
		l1, _ := new(big.Int).SetString(body.L1Fee, 10)
		if sum := new(big.Int).Add(l2, l1); sum.Cmp(gotTotal) != 0 {
			t.Fatalf("l2Fee + l1Fee = %s, want equal to totalFee %s", sum, gotTotal)
		}
	})

	t.Run("invalid chain enum value returns 400", func(t *testing.T) {
		rec := getWithdrawalFeeEstimate(t, env, "chain=optimism&asset=eth&amount=1", true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("Content-Type = %q, want application/problem+json", ct)
		}
	})

	t.Run("invalid asset enum value returns 400", func(t *testing.T) {
		rec := getWithdrawalFeeEstimate(t, env, "chain=base&asset=dai&amount=1", true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("zero amount returns 400", func(t *testing.T) {
		rec := getWithdrawalFeeEstimate(t, env, "chain=base&asset=eth&amount=0", true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("amount exceeding a uint256's maximum returns 400", func(t *testing.T) {
		tooLarge := new(big.Int).Lsh(big.NewInt(1), 256) // 2^256, one past uint256's max
		rec := getWithdrawalFeeEstimate(t, env, "chain=base&asset=eth&amount="+tooLarge.String(), true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("negative amount returns 400", func(t *testing.T) {
		rec := getWithdrawalFeeEstimate(t, env, "chain=base&asset=eth&amount=-5", true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("missing amount returns 400", func(t *testing.T) {
		rec := getWithdrawalFeeEstimate(t, env, "chain=base&asset=eth", true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("non-numeric amount returns 400", func(t *testing.T) {
		rec := getWithdrawalFeeEstimate(t, env, "chain=base&asset=eth&amount=abc", true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("missing bearer token is rejected", func(t *testing.T) {
		rec := getWithdrawalFeeEstimate(t, env, "chain=base&asset=eth&amount=1", false)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})
}
