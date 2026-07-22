// Package postgres_test exercises WithdrawalRepository against a real PostgreSQL
// container — this project's stated thesis is rigor over shortcuts (PRD Success Metric
// 5), so the concurrency guarantee this file proves (Story 3.2's own I/O matrix entry:
// "Concurrent requests, same customer, same (chain, asset), different keys" must never
// lose an update) is exercised against the real lock-ordering behavior of a real
// database, not a mocked repository. It lives in an external test package so it can
// import both internal/adapter/postgres and internal/adapter/evm (for a real, RPC-free
// deposit address deriver) without either adapter importing the other (AD-1, AD-2) —
// mirroring internal/adapter/api/integration_test.go's own composition-root role.
package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/andborges/digital-asset-wallet-platform/internal/adapter/evm"
	"github.com/andborges/digital-asset-wallet-platform/internal/adapter/postgres"
	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// newTestPool starts a throwaway Postgres 18 container (matching this project's other
// Docker-backed integration tests), migrates it, and returns a connected pool.
func newTestPool(t *testing.T) *pgxpool.Pool {
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
	return pool
}

// createTestCustomerWithBalance provisions a real customer (available+hold accounts for
// every SupportedChainAssetPairs entry, via the real CreateCustomer use case) and credits
// its (chain, asset) available account directly via a fixture posting — the same
// technique internal/adapter/api/integration_test.go's own creditAccount helper uses to
// give a test a starting balance without depending on any other write path.
func createTestCustomerWithBalance(t *testing.T, pool *pgxpool.Pool, txBeginner *postgres.TxBeginner, chain, asset, amount string) string {
	t.Helper()
	ctx := context.Background()

	customerRepo := postgres.NewCustomerRepository()
	addressDeriver := evm.NewDepositAddressDeriver()
	createCustomer := core.NewCreateCustomer(customerRepo, addressDeriver)

	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	customer, err := createCustomer.Execute(txCtx)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("create customer: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	var accountID string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM accounts WHERE customer_id = $1 AND chain = $2 AND asset = $3 AND account_type = 'available'`,
		customer.ID, chain, asset,
	).Scan(&accountID); err != nil {
		t.Fatalf("look up available account: %v", err)
	}

	journalEntryID := uuid.New().String()
	if _, err := pool.Exec(ctx,
		`INSERT INTO journal_entries (id, cause_type, cause_id) VALUES ($1, 'test_fixture', $2)`,
		journalEntryID, journalEntryID,
	); err != nil {
		t.Fatalf("insert journal_entries fixture row: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO postings (id, journal_entry_id, account_id, amount) VALUES ($1, $2, $3, $4)`,
		uuid.New().String(), journalEntryID, accountID, amount,
	); err != nil {
		t.Fatalf("insert postings fixture row: %v", err)
	}

	return customer.ID
}

// accountBalance sums postings directly for customerID's (chain, asset, accountType)
// account — a direct-to-Postgres check independent of BalanceRepository/the balances
// endpoint (which only ever surfaces the available account, Story 3.2), so this test can
// also assert on the hold account's balance.
func accountBalance(t *testing.T, pool *pgxpool.Pool, customerID, chain, asset, accountType string) *big.Int {
	t.Helper()
	var text string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(p.amount), 0)::text
		 FROM accounts a LEFT JOIN postings p ON p.account_id = a.id
		 WHERE a.customer_id = $1 AND a.chain = $2 AND a.asset = $3 AND a.account_type = $4`,
		customerID, chain, asset, accountType,
	).Scan(&text); err != nil {
		t.Fatalf("sum %s balance: %v", accountType, err)
	}
	amount, ok := new(big.Int).SetString(text, 10)
	if !ok {
		t.Fatalf("parse balance %q as integer", text)
	}
	return amount
}

func mustParseBigInt(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("parse %q as integer", s)
	}
	return n
}

// TestCreateWithdrawal_ConcurrentRequestsNeverLoseAnUpdate mirrors CreateTransfer's own
// lock-ordering assurance (Story 1.3) for withdrawals (Story 3.2's own I/O matrix entry
// "Concurrent requests, same customer, same (chain, asset), different keys"): many
// concurrent withdrawal requests for the SAME customer/chain/asset, each with its own
// distinct Idempotency-Key, must all succeed serially (row-locked via
// "...FOR UPDATE") with no lost update — every requested amount is reflected in the final
// available and hold balances, and every withdrawals row is written exactly once.
func TestCreateWithdrawal_ConcurrentRequestsNeverLoseAnUpdate(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	const numWithdrawals = 20
	const perWithdrawalAmount = 5
	startingBalance := fmt.Sprintf("%d", numWithdrawals*perWithdrawalAmount*2) // ample headroom

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", startingBalance)

	var wg sync.WaitGroup
	errs := make([]error, numWithdrawals)
	for i := 0; i < numWithdrawals; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			txCtx, tx, err := txBeginner.Begin(ctx)
			if err != nil {
				errs[i] = fmt.Errorf("begin tx: %w", err)
				return
			}
			_, err = withdrawalRepo.CreateWithdrawal(txCtx, core.WithdrawalRequest{
				CustomerID:         customerID,
				Chain:              core.ChainBase,
				Asset:              core.AssetETH,
				Amount:             big.NewInt(perWithdrawalAmount),
				DestinationAddress: "0x00000000000000000000000000000000000000AA",
				IdempotencyKey:     fmt.Sprintf("concurrent-withdrawal-key-%d", i),
			}, big.NewInt(0), core.WithdrawalStatusApproved)
			if err != nil {
				_ = tx.Rollback(ctx)
				errs[i] = err
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs[i] = fmt.Errorf("commit: %w", err)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("withdrawal %d failed: %v", i, err)
		}
	}

	wantAvailable := new(big.Int).Sub(mustParseBigInt(t, startingBalance), big.NewInt(numWithdrawals*perWithdrawalAmount))
	gotAvailable := accountBalance(t, pool, customerID, "base", "eth", "available")
	if gotAvailable.Cmp(wantAvailable) != 0 {
		t.Fatalf("available balance = %s, want %s (no lost update)", gotAvailable, wantAvailable)
	}

	wantHold := big.NewInt(numWithdrawals * perWithdrawalAmount)
	gotHold := accountBalance(t, pool, customerID, "base", "eth", "hold")
	if gotHold.Cmp(wantHold) != 0 {
		t.Fatalf("hold balance = %s, want %s (no lost update)", gotHold, wantHold)
	}

	var withdrawalCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM withdrawals WHERE customer_id = $1`, customerID,
	).Scan(&withdrawalCount); err != nil {
		t.Fatalf("count withdrawals: %v", err)
	}
	if withdrawalCount != numWithdrawals {
		t.Fatalf("withdrawals row count = %d, want %d (one row per concurrent request, none lost or duplicated)", withdrawalCount, numWithdrawals)
	}

	var journalCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM journal_entries WHERE cause_type = 'withdrawal_hold'`,
	).Scan(&journalCount); err != nil {
		t.Fatalf("count journal_entries: %v", err)
	}
	if journalCount != numWithdrawals {
		t.Fatalf("withdrawal_hold journal entries = %d, want %d", journalCount, numWithdrawals)
	}
}

// TestCreateWithdrawal_InsufficientBalance_RejectsWithoutPartialWrite proves the
// available-balance check runs after the lock is acquired and before any write — a
// rejected withdrawal request leaves no journal entry, no postings, and no withdrawals
// row behind (Story 3.2 Acceptance Criteria: "no hold is placed").
func TestCreateWithdrawal_InsufficientBalance_RejectsWithoutPartialWrite(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "100")

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, err = withdrawalRepo.CreateWithdrawal(txCtx, core.WithdrawalRequest{
		CustomerID:         customerID,
		Chain:              core.ChainBase,
		Asset:              core.AssetETH,
		Amount:             big.NewInt(101),
		DestinationAddress: "0x00000000000000000000000000000000000000AA",
		IdempotencyKey:     "insufficient-balance-key",
	}, big.NewInt(0), core.WithdrawalStatusApproved)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, core.ErrInsufficientBalance) {
		t.Fatalf("err = %v, want core.ErrInsufficientBalance", err)
	}

	var withdrawalCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM withdrawals WHERE customer_id = $1`, customerID).Scan(&withdrawalCount); err != nil {
		t.Fatalf("count withdrawals: %v", err)
	}
	if withdrawalCount != 0 {
		t.Fatalf("withdrawals row count = %d, want 0 (no hold placed on a rejected request)", withdrawalCount)
	}

	gotAvailable := accountBalance(t, pool, customerID, "base", "eth", "available")
	if gotAvailable.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("available balance = %s, want unchanged 100", gotAvailable)
	}
	gotHold := accountBalance(t, pool, customerID, "base", "eth", "hold")
	if gotHold.Sign() != 0 {
		t.Fatalf("hold balance = %s, want 0 (no hold placed)", gotHold)
	}
}

// outboxEventCount returns how many outbox_events rows of eventType carry withdrawalId in
// their jsonb payload.
func outboxEventCount(t *testing.T, pool *pgxpool.Pool, eventType, withdrawalID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE event_type = $1 AND payload->>'withdrawalId' = $2`,
		eventType, withdrawalID,
	).Scan(&count); err != nil {
		t.Fatalf("count %s outbox events: %v", eventType, err)
	}
	return count
}

// outboxEventPayloadFields is the shape outboxEventPayload decodes a raw jsonb payload
// into — the fields every withdrawal outbox event this story writes may carry, whichever
// write path (CreateWithdrawal's routing, or ApproveWithdrawal's operator action) produced
// it. Fields absent from the raw JSON (omitempty on the write side) decode to Go's zero
// value ("") here, which is exactly what re-review's payload-shape test below asserts on.
type outboxEventPayloadFields struct {
	WithdrawalID       string `json:"withdrawalId"`
	CustomerID         string `json:"customerId"`
	Chain              string `json:"chain"`
	Asset              string `json:"asset"`
	Amount             string `json:"amount"`
	DestinationAddress string `json:"destinationAddress"`
	ApprovedBy         string `json:"approvedBy"`
	ApprovalReason     string `json:"approvalReason"`
}

// outboxEventPayload fetches and decodes the single outbox_events row of eventType
// carrying withdrawalId in its payload (re-review 2026-07-21: outboxEventCount above only
// ever checked presence/count, never field contents — a schema mismatch or a swapped field
// would have passed every existing test). Fails the test outright if zero or more than one
// row matches, since every call site expects exactly one.
func outboxEventPayload(t *testing.T, pool *pgxpool.Pool, eventType, withdrawalID string) outboxEventPayloadFields {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT payload FROM outbox_events WHERE event_type = $1 AND payload->>'withdrawalId' = $2`,
		eventType, withdrawalID,
	).Scan(&raw); err != nil {
		t.Fatalf("fetch %s outbox payload for withdrawal %s: %v", eventType, withdrawalID, err)
	}
	var fields outboxEventPayloadFields
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode %s outbox payload for withdrawal %s: %v (raw: %s)", eventType, withdrawalID, err, raw)
	}
	return fields
}

// TestCreateWithdrawal_AutoApprovalRouting proves that when core (this test's caller,
// mimicking core.CreateWithdrawal's own decision) passes targetStatus =
// core.WithdrawalStatusApproved, the repository writes that status and a paired
// "withdrawal.approved" outbox event — never the Story 3.2 "withdrawal.created" event,
// which this story's CreateWithdrawal no longer writes.
func TestCreateWithdrawal_AutoApprovalRouting(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	withdrawal, err := withdrawalRepo.CreateWithdrawal(txCtx, core.WithdrawalRequest{
		CustomerID:         customerID,
		Chain:              core.ChainBase,
		Asset:              core.AssetETH,
		Amount:             big.NewInt(100),
		DestinationAddress: "0x00000000000000000000000000000000000000AA",
		IdempotencyKey:     "auto-approval-key",
	}, big.NewInt(500), core.WithdrawalStatusApproved)
	if err != nil {
		t.Fatalf("create withdrawal: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	if withdrawal.Status != core.WithdrawalStatusApproved {
		t.Fatalf("status = %q, want %q", withdrawal.Status, core.WithdrawalStatusApproved)
	}

	var dbStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM withdrawals WHERE id = $1`, withdrawal.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query withdrawal status: %v", err)
	}
	if dbStatus != core.WithdrawalStatusApproved {
		t.Fatalf("db status = %q, want %q", dbStatus, core.WithdrawalStatusApproved)
	}

	if got := outboxEventCount(t, pool, "withdrawal.approved", withdrawal.ID); got != 1 {
		t.Fatalf("withdrawal.approved outbox events = %d, want exactly 1", got)
	}
	if got := outboxEventCount(t, pool, "approval.required", withdrawal.ID); got != 0 {
		t.Fatalf("approval.required outbox events = %d, want 0", got)
	}
	if got := outboxEventCount(t, pool, "withdrawal.created", withdrawal.ID); got != 0 {
		t.Fatalf("withdrawal.created outbox events = %d, want 0 (Story 3.2's event is no longer written)", got)
	}

	// re-review 2026-07-21: the auto-approval path's "withdrawal.approved" payload must
	// carry the full withdrawal shape (chain/asset/amount/destinationAddress/customerId) —
	// the same shape the operator-approval path's payload carries (asserted in
	// TestApproveWithdrawal_TransitionsToApproved below) — so a consumer of
	// "withdrawal.approved" can decode one fixed shape regardless of which path produced
	// it. approvedBy/approvalReason must be absent (omitempty): there is no operator action
	// on this path.
	payload := outboxEventPayload(t, pool, "withdrawal.approved", withdrawal.ID)
	if payload.CustomerID != customerID || payload.Chain != "base" || payload.Asset != "eth" || payload.Amount != "100" || payload.DestinationAddress != "0x00000000000000000000000000000000000000AA" {
		t.Fatalf("withdrawal.approved payload = %+v, want customerId=%q chain=base asset=eth amount=100 destinationAddress=0x...AA", payload, customerID)
	}
	if payload.ApprovedBy != "" || payload.ApprovalReason != "" {
		t.Fatalf("withdrawal.approved payload approvedBy/approvalReason = %q/%q, want both empty (auto-approval has no operator action)", payload.ApprovedBy, payload.ApprovalReason)
	}
}

// TestCreateWithdrawal_AwaitingApprovalRouting mirrors the auto-approval test above for
// the opposite routing outcome.
func TestCreateWithdrawal_AwaitingApprovalRouting(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	withdrawal, err := withdrawalRepo.CreateWithdrawal(txCtx, core.WithdrawalRequest{
		CustomerID:         customerID,
		Chain:              core.ChainBase,
		Asset:              core.AssetETH,
		Amount:             big.NewInt(9000),
		DestinationAddress: "0x00000000000000000000000000000000000000AA",
		IdempotencyKey:     "awaiting-approval-key",
	}, big.NewInt(500), core.WithdrawalStatusAwaitingApproval)
	if err != nil {
		t.Fatalf("create withdrawal: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	if withdrawal.Status != core.WithdrawalStatusAwaitingApproval {
		t.Fatalf("status = %q, want %q", withdrawal.Status, core.WithdrawalStatusAwaitingApproval)
	}

	if got := outboxEventCount(t, pool, "approval.required", withdrawal.ID); got != 1 {
		t.Fatalf("approval.required outbox events = %d, want exactly 1", got)
	}
	if got := outboxEventCount(t, pool, "withdrawal.approved", withdrawal.ID); got != 0 {
		t.Fatalf("withdrawal.approved outbox events = %d, want 0", got)
	}
}

// TestCreateWithdrawal_InsufficientBalanceForFee_RejectsWithoutPartialWrite proves the
// fee check runs after the amount-hold's postings would otherwise be written, but that a
// failure there still leaves no journal entry, no postings, and no withdrawals row behind
// — the whole call is rolled back by the caller (IdempotencyMiddleware, in production;
// this test's own explicit tx.Rollback here), never partially committed (Story 3.3 I/O
// matrix: "hold is NOT committed").
func TestCreateWithdrawal_InsufficientBalanceForFee_RejectsWithoutPartialWrite(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	// Available covers the requested amount (100) comfortably, but not amount + fee: after
	// the hold, only 1 unit of available balance remains — far short of the 1500 fee
	// estimate below.
	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "101")

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, err = withdrawalRepo.CreateWithdrawal(txCtx, core.WithdrawalRequest{
		CustomerID:         customerID,
		Chain:              core.ChainBase,
		Asset:              core.AssetETH,
		Amount:             big.NewInt(100),
		DestinationAddress: "0x00000000000000000000000000000000000000AA",
		IdempotencyKey:     "insufficient-fee-key",
	}, big.NewInt(1500), core.WithdrawalStatusApproved)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, core.ErrInsufficientBalanceForFee) {
		t.Fatalf("err = %v, want core.ErrInsufficientBalanceForFee", err)
	}

	var withdrawalCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM withdrawals WHERE customer_id = $1`, customerID).Scan(&withdrawalCount); err != nil {
		t.Fatalf("count withdrawals: %v", err)
	}
	if withdrawalCount != 0 {
		t.Fatalf("withdrawals row count = %d, want 0 (no partial write on a fee-rejected request)", withdrawalCount)
	}

	var journalCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE cause_type = 'withdrawal_hold'`).Scan(&journalCount); err != nil {
		t.Fatalf("count journal_entries: %v", err)
	}
	if journalCount != 0 {
		t.Fatalf("withdrawal_hold journal entries = %d, want 0 (no partial write)", journalCount)
	}

	gotAvailable := accountBalance(t, pool, customerID, "base", "eth", "available")
	if gotAvailable.Cmp(big.NewInt(101)) != 0 {
		t.Fatalf("available balance = %s, want unchanged 101", gotAvailable)
	}
	gotHold := accountBalance(t, pool, customerID, "base", "eth", "hold")
	if gotHold.Sign() != 0 {
		t.Fatalf("hold balance = %s, want 0 (no hold placed)", gotHold)
	}
}

// createAwaitingApprovalWithdrawal creates a real withdrawal row already routed to
// awaiting-approval (via the real CreateWithdrawal, not a hand-written fixture INSERT) so
// ApproveWithdrawal's own tests exercise a realistic row.
func createAwaitingApprovalWithdrawal(t *testing.T, pool *pgxpool.Pool, txBeginner *postgres.TxBeginner, customerID string) string {
	t.Helper()
	ctx := context.Background()
	withdrawalRepo := postgres.NewWithdrawalRepository()

	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	withdrawal, err := withdrawalRepo.CreateWithdrawal(txCtx, core.WithdrawalRequest{
		CustomerID:         customerID,
		Chain:              core.ChainBase,
		Asset:              core.AssetETH,
		Amount:             big.NewInt(9000),
		DestinationAddress: "0x00000000000000000000000000000000000000AA",
		IdempotencyKey:     fmt.Sprintf("awaiting-approval-fixture-%s", uuid.New().String()),
	}, big.NewInt(0), core.WithdrawalStatusAwaitingApproval)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("create awaiting-approval withdrawal fixture: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}
	return withdrawal.ID
}

// TestApproveWithdrawal_TransitionsToApproved proves the full happy path: an
// awaiting-approval withdrawal, once approved, is Approved with its audit columns
// populated and a paired "withdrawal.approved" outbox event written.
func TestApproveWithdrawal_TransitionsToApproved(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	withdrawalID := createAwaitingApprovalWithdrawal(t, pool, txBeginner, customerID)

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	approved, err := withdrawalRepo.ApproveWithdrawal(txCtx, withdrawalID, "ops-alice", "manually reviewed, looks fine")
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("approve withdrawal: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	if approved.Status != core.WithdrawalStatusApproved {
		t.Fatalf("status = %q, want %q", approved.Status, core.WithdrawalStatusApproved)
	}
	if approved.ApprovedBy != "ops-alice" {
		t.Fatalf("approvedBy = %q, want %q", approved.ApprovedBy, "ops-alice")
	}
	if approved.ApprovalReason != "manually reviewed, looks fine" {
		t.Fatalf("approvalReason = %q, want %q", approved.ApprovalReason, "manually reviewed, looks fine")
	}
	if approved.ApprovedAt == nil {
		t.Fatal("approvedAt = nil, want a populated timestamp")
	}

	var dbStatus, dbApprovedBy, dbApprovalReason string
	var dbApprovedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT status, approved_by, approval_reason, approved_at FROM withdrawals WHERE id = $1`, withdrawalID,
	).Scan(&dbStatus, &dbApprovedBy, &dbApprovalReason, &dbApprovedAt); err != nil {
		t.Fatalf("query withdrawal: %v", err)
	}
	if dbStatus != core.WithdrawalStatusApproved || dbApprovedBy != "ops-alice" || dbApprovalReason != "manually reviewed, looks fine" {
		t.Fatalf("db row = (%q, %q, %q), want (%q, %q, %q)", dbStatus, dbApprovedBy, dbApprovalReason, core.WithdrawalStatusApproved, "ops-alice", "manually reviewed, looks fine")
	}

	if got := outboxEventCount(t, pool, "withdrawal.approved", withdrawalID); got != 1 {
		t.Fatalf("withdrawal.approved outbox events = %d, want exactly 1", got)
	}

	// re-review 2026-07-21: the operator-approval path's "withdrawal.approved" payload
	// must carry the SAME fixed shape as the auto-approval path's (asserted in
	// TestCreateWithdrawal_AutoApprovalRouting above) — customerId/chain/asset/amount/
	// destinationAddress — plus this path's own approvedBy/approvalReason, not a narrower
	// payload with only approvedBy/approvalReason (the pre-fix schema-drift bug).
	payload := outboxEventPayload(t, pool, "withdrawal.approved", withdrawalID)
	if payload.CustomerID != customerID || payload.Chain != "base" || payload.Asset != "eth" {
		t.Fatalf("withdrawal.approved payload = %+v, want customerId=%q chain=base asset=eth", payload, customerID)
	}
	if payload.ApprovedBy != "ops-alice" || payload.ApprovalReason != "manually reviewed, looks fine" {
		t.Fatalf("withdrawal.approved payload approvedBy/approvalReason = %q/%q, want %q/%q", payload.ApprovedBy, payload.ApprovalReason, "ops-alice", "manually reviewed, looks fine")
	}
}

// TestApproveWithdrawal_UnknownID_ReturnsErrWithdrawalNotFound.
func TestApproveWithdrawal_UnknownID_ReturnsErrWithdrawalNotFound(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, err = withdrawalRepo.ApproveWithdrawal(txCtx, uuid.New().String(), "ops-alice", "reason")
	_ = tx.Rollback(ctx)
	if !errors.Is(err, core.ErrWithdrawalNotFound) {
		t.Fatalf("err = %v, want core.ErrWithdrawalNotFound", err)
	}
}

// TestApproveWithdrawal_DoubleApprove_SecondCallRejected proves a withdrawal already
// approved cannot be approved a second time.
func TestApproveWithdrawal_DoubleApprove_SecondCallRejected(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	withdrawalID := createAwaitingApprovalWithdrawal(t, pool, txBeginner, customerID)

	ctx := context.Background()

	txCtx1, tx1, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := withdrawalRepo.ApproveWithdrawal(txCtx1, withdrawalID, "ops-alice", "first approval"); err != nil {
		_ = tx1.Rollback(ctx)
		t.Fatalf("first approve: %v", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit first approve: %v", err)
	}

	txCtx2, tx2, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, err = withdrawalRepo.ApproveWithdrawal(txCtx2, withdrawalID, "ops-bob", "second approval attempt")
	_ = tx2.Rollback(ctx)
	if !errors.Is(err, core.ErrWithdrawalNotAwaitingApproval) {
		t.Fatalf("err = %v, want core.ErrWithdrawalNotAwaitingApproval", err)
	}

	// The first approval's audit columns are untouched by the rejected second attempt.
	var dbApprovedBy string
	if err := pool.QueryRow(ctx, `SELECT approved_by FROM withdrawals WHERE id = $1`, withdrawalID).Scan(&dbApprovedBy); err != nil {
		t.Fatalf("query withdrawal: %v", err)
	}
	if dbApprovedBy != "ops-alice" {
		t.Fatalf("approved_by = %q, want %q (unchanged by the rejected second attempt)", dbApprovedBy, "ops-alice")
	}
}

// TestApproveWithdrawal_ConcurrentApproveRequests_ExactlyOneWins races many concurrent
// approve attempts for the SAME withdrawal — the row lock (FOR UPDATE) must make this
// deterministic: exactly one commits the approved transition, every other sees the
// now-approved status and fails with ErrWithdrawalNotAwaitingApproval (Story 3.3 I/O
// matrix's own concurrent-approve scenario).
func TestApproveWithdrawal_ConcurrentApproveRequests_ExactlyOneWins(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	withdrawalID := createAwaitingApprovalWithdrawal(t, pool, txBeginner, customerID)

	const numAttempts = 10
	var wg sync.WaitGroup
	successes := make([]bool, numAttempts)
	errs := make([]error, numAttempts)
	for i := 0; i < numAttempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			txCtx, tx, err := txBeginner.Begin(ctx)
			if err != nil {
				errs[i] = fmt.Errorf("begin tx: %w", err)
				return
			}
			_, err = withdrawalRepo.ApproveWithdrawal(txCtx, withdrawalID, fmt.Sprintf("ops-%d", i), "concurrent approval attempt")
			if err != nil {
				_ = tx.Rollback(ctx)
				errs[i] = err
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs[i] = fmt.Errorf("commit: %w", err)
				return
			}
			successes[i] = true
		}(i)
	}
	wg.Wait()

	successCount := 0
	for i := range numAttempts {
		if successes[i] {
			successCount++
			continue
		}
		if !errors.Is(errs[i], core.ErrWithdrawalNotAwaitingApproval) {
			t.Fatalf("attempt %d: err = %v, want core.ErrWithdrawalNotAwaitingApproval", i, errs[i])
		}
	}
	if successCount != 1 {
		t.Fatalf("successful approvals = %d, want exactly 1", successCount)
	}

	var dbStatus string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM withdrawals WHERE id = $1`, withdrawalID).Scan(&dbStatus); err != nil {
		t.Fatalf("query withdrawal status: %v", err)
	}
	if dbStatus != core.WithdrawalStatusApproved {
		t.Fatalf("db status = %q, want %q", dbStatus, core.WithdrawalStatusApproved)
	}

	if got := outboxEventCount(t, pool, "withdrawal.approved", withdrawalID); got != 1 {
		t.Fatalf("withdrawal.approved outbox events = %d, want exactly 1 (not one per racing attempt)", got)
	}
}
