// This file exercises Story 3.4's new WithdrawalRepository methods (ClaimApprovedWithdrawal,
// RecordBroadcastTxHash, ListBroadcastWithdrawals, SettleConfirmedWithdrawal,
// SettleFailedWithdrawal) against a real PostgreSQL container — reusing newTestPool and the
// customer/balance fixtures already established in withdrawal_repo_test.go (same
// postgres_test package).
package postgres_test

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andborges/digital-asset-wallet-platform/internal/adapter/postgres"
	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// createApprovedWithdrawal creates a real withdrawal row already routed straight to
// approved (via the real CreateWithdrawal, not a hand-written fixture INSERT) — the
// starting state every one of this file's tests needs, since ClaimApprovedWithdrawal only
// ever selects WithdrawalStatusApproved rows.
func createApprovedWithdrawal(t *testing.T, pool *pgxpool.Pool, txBeginner *postgres.TxBeginner, customerID, chain, asset, amount string) core.Withdrawal {
	t.Helper()
	ctx := context.Background()
	withdrawalRepo := postgres.NewWithdrawalRepository()

	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	withdrawal, err := withdrawalRepo.CreateWithdrawal(txCtx, core.WithdrawalRequest{
		CustomerID:         customerID,
		Chain:              core.Chain(chain),
		Asset:              core.Asset(asset),
		Amount:             mustParseBigInt(t, amount),
		DestinationAddress: "0x00000000000000000000000000000000000000AA",
		IdempotencyKey:     "approved-fixture-" + uuid.New().String(),
	}, big.NewInt(0), core.WithdrawalStatusApproved)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("create approved withdrawal fixture: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}
	return withdrawal
}

// chainNextNonce reads chain_nonce_state.next_nonce directly, independent of the
// repository under test.
func chainNextNonce(t *testing.T, pool *pgxpool.Pool, chain string) int64 {
	t.Helper()
	var next int64
	if err := pool.QueryRow(context.Background(), `SELECT next_nonce FROM chain_nonce_state WHERE chain = $1`, chain).Scan(&next); err != nil {
		t.Fatalf("query chain_nonce_state: %v", err)
	}
	return next
}

// broadcastAttemptRow reads a withdrawal's own broadcast_attempts row directly.
func broadcastAttemptRow(t *testing.T, pool *pgxpool.Pool, withdrawalID string) (nonce int64, txHash *string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT nonce, tx_hash FROM broadcast_attempts WHERE withdrawal_id = $1`, withdrawalID,
	).Scan(&nonce, &txHash); err != nil {
		t.Fatalf("query broadcast_attempts: %v", err)
	}
	return nonce, txHash
}

func TestClaimApprovedWithdrawal_HappyPath_AllocatesNonceAndTransitionsToSigned(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	withdrawal := createApprovedWithdrawal(t, pool, txBeginner, customerID, "base", "eth", "100")

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	claimed, ok, err := withdrawalRepo.ClaimApprovedWithdrawal(txCtx, core.ChainBase)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("claim approved withdrawal: %v", err)
	}
	if !ok {
		_ = tx.Rollback(ctx)
		t.Fatal("ok = false, want true (an approved withdrawal exists)")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	if claimed.ID != withdrawal.ID {
		t.Fatalf("claimed id = %q, want %q", claimed.ID, withdrawal.ID)
	}
	if claimed.Status != core.WithdrawalStatusSigned {
		t.Fatalf("claimed status = %q, want %q", claimed.Status, core.WithdrawalStatusSigned)
	}
	if claimed.Nonce == nil || *claimed.Nonce != 0 {
		t.Fatalf("claimed nonce = %v, want 0 (the seeded starting nonce for base)", claimed.Nonce)
	}

	var dbStatus string
	var dbNonce int64
	if err := pool.QueryRow(ctx, `SELECT status, nonce FROM withdrawals WHERE id = $1`, withdrawal.ID).Scan(&dbStatus, &dbNonce); err != nil {
		t.Fatalf("query withdrawal: %v", err)
	}
	if dbStatus != core.WithdrawalStatusSigned || dbNonce != 0 {
		t.Fatalf("db (status, nonce) = (%q, %d), want (%q, 0)", dbStatus, dbNonce, core.WithdrawalStatusSigned)
	}

	nonce, txHash := broadcastAttemptRow(t, pool, withdrawal.ID)
	if nonce != 0 {
		t.Fatalf("broadcast_attempts.nonce = %d, want 0", nonce)
	}
	if txHash != nil {
		t.Fatalf("broadcast_attempts.tx_hash = %v, want nil (not yet broadcast)", txHash)
	}

	if got := chainNextNonce(t, pool, "base"); got != 1 {
		t.Fatalf("chain_nonce_state.next_nonce for base = %d, want 1 (advanced past the allocated nonce)", got)
	}
}

// TestClaimApprovedWithdrawal_NoChainNonceState_FailsLoud proves the registry-gap row this
// method's own doc comment promises (re-review 2026-07-21, edge-case review: previously
// untested, unlike ErrNoTreasuryAccount's own dedicated test below): with the chain's
// chain_nonce_state row removed (an artificially induced gap — migration 0011 always seeds
// one per supported chain in a correctly migrated deployment), claiming fails loud with
// postgres.ErrNoChainNonceState and leaves the withdrawal at WithdrawalStatusApproved for
// the next poll to retry, rather than silently allocating no nonce or crashing.
func TestClaimApprovedWithdrawal_NoChainNonceState_FailsLoud(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	withdrawal := createApprovedWithdrawal(t, pool, txBeginner, customerID, "base", "eth", "100")

	if _, err := pool.Exec(context.Background(), `DELETE FROM chain_nonce_state WHERE chain = 'base'`); err != nil {
		t.Fatalf("delete chain_nonce_state fixture: %v", err)
	}

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, _, err = withdrawalRepo.ClaimApprovedWithdrawal(txCtx, core.ChainBase)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, postgres.ErrNoChainNonceState) {
		t.Fatalf("err = %v, want postgres.ErrNoChainNonceState", err)
	}

	var dbStatus string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM withdrawals WHERE id = $1`, withdrawal.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query withdrawal: %v", err)
	}
	if dbStatus != core.WithdrawalStatusApproved {
		t.Fatalf("status = %q, want %q (unchanged, retried next poll)", dbStatus, core.WithdrawalStatusApproved)
	}
}

func TestClaimApprovedWithdrawal_NoApprovedWithdrawal_ReturnsOkFalse(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, ok, err := withdrawalRepo.ClaimApprovedWithdrawal(txCtx, core.ChainBase)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Fatal("ok = true, want false — no approved withdrawal exists")
	}
}

func TestClaimApprovedWithdrawal_ScopedToRequestedChain(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "arbitrum", "eth", "10000")
	createApprovedWithdrawal(t, pool, txBeginner, customerID, "arbitrum", "eth", "100")

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, ok, err := withdrawalRepo.ClaimApprovedWithdrawal(txCtx, core.ChainBase)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Fatal("ok = true, want false — the only approved withdrawal is on arbitrum, not base")
	}
}

func TestClaimApprovedWithdrawal_SecondCallAllocatesNextNonce(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	createApprovedWithdrawal(t, pool, txBeginner, customerID, "base", "eth", "100")
	createApprovedWithdrawal(t, pool, txBeginner, customerID, "base", "eth", "200")

	ctx := context.Background()
	claimOne := func() core.Withdrawal {
		txCtx, tx, err := txBeginner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		w, ok, err := withdrawalRepo.ClaimApprovedWithdrawal(txCtx, core.ChainBase)
		if err != nil || !ok {
			_ = tx.Rollback(ctx)
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return w
	}

	first := claimOne()
	second := claimOne()

	if first.Nonce == nil || second.Nonce == nil {
		t.Fatal("both claims should have a non-nil nonce")
	}
	if *first.Nonce != 0 || *second.Nonce != 1 {
		t.Fatalf("nonces = (%d, %d), want (0, 1)", *first.Nonce, *second.Nonce)
	}
	if first.ID == second.ID {
		t.Fatal("the two claims should be two different withdrawals")
	}
}

func TestRecordBroadcastTxHash_TransitionsToBroadcast(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	withdrawal := createApprovedWithdrawal(t, pool, txBeginner, customerID, "base", "eth", "100")

	ctx := context.Background()
	claimCtx, claimTx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, ok, err := withdrawalRepo.ClaimApprovedWithdrawal(claimCtx, core.ChainBase); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := claimTx.Commit(ctx); err != nil {
		t.Fatalf("commit claim: %v", err)
	}

	recordCtx, recordTx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := withdrawalRepo.RecordBroadcastTxHash(recordCtx, withdrawal.ID, "0xdeadbeef"); err != nil {
		_ = recordTx.Rollback(ctx)
		t.Fatalf("record broadcast tx hash: %v", err)
	}
	if err := recordTx.Commit(ctx); err != nil {
		t.Fatalf("commit record: %v", err)
	}

	var dbStatus, dbTxHash string
	if err := pool.QueryRow(ctx, `SELECT status, tx_hash FROM withdrawals WHERE id = $1`, withdrawal.ID).Scan(&dbStatus, &dbTxHash); err != nil {
		t.Fatalf("query withdrawal: %v", err)
	}
	if dbStatus != core.WithdrawalStatusBroadcast || dbTxHash != "0xdeadbeef" {
		t.Fatalf("db (status, tx_hash) = (%q, %q), want (%q, %q)", dbStatus, dbTxHash, core.WithdrawalStatusBroadcast, "0xdeadbeef")
	}

	_, txHash := broadcastAttemptRow(t, pool, withdrawal.ID)
	if txHash == nil || *txHash != "0xdeadbeef" {
		t.Fatalf("broadcast_attempts.tx_hash = %v, want 0xdeadbeef", txHash)
	}
}

func TestRecordBroadcastTxHash_NotSigned_ReturnsError(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	// Never claimed — still 'approved', not 'signed'.
	withdrawal := createApprovedWithdrawal(t, pool, txBeginner, customerID, "base", "eth", "100")

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	err = withdrawalRepo.RecordBroadcastTxHash(txCtx, withdrawal.ID, "0xdeadbeef")
	_ = tx.Rollback(ctx)
	if !errors.Is(err, core.ErrWithdrawalNotSigned) {
		t.Fatalf("err = %v, want core.ErrWithdrawalNotSigned", err)
	}
}

func TestRecordBroadcastTxHash_UnknownID_ReturnsErrWithdrawalNotFound(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	err = withdrawalRepo.RecordBroadcastTxHash(txCtx, uuid.New().String(), "0xdeadbeef")
	_ = tx.Rollback(ctx)
	if !errors.Is(err, core.ErrWithdrawalNotFound) {
		t.Fatalf("err = %v, want core.ErrWithdrawalNotFound", err)
	}
}

// claimAndBroadcast drives a fresh approved withdrawal all the way to 'broadcast' via the
// real repository methods (never a hand-written fixture INSERT), giving settlement tests a
// realistic starting row.
func claimAndBroadcast(t *testing.T, pool *pgxpool.Pool, txBeginner *postgres.TxBeginner, customerID, chain, asset, amount, txHash string) core.Withdrawal {
	t.Helper()
	ctx := context.Background()
	withdrawalRepo := postgres.NewWithdrawalRepository()

	withdrawal := createApprovedWithdrawal(t, pool, txBeginner, customerID, chain, asset, amount)

	claimCtx, claimTx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	claimed, ok, err := withdrawalRepo.ClaimApprovedWithdrawal(claimCtx, core.Chain(chain))
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := claimTx.Commit(ctx); err != nil {
		t.Fatalf("commit claim: %v", err)
	}

	recordCtx, recordTx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := withdrawalRepo.RecordBroadcastTxHash(recordCtx, claimed.ID, txHash); err != nil {
		t.Fatalf("record broadcast tx hash: %v", err)
	}
	if err := recordTx.Commit(ctx); err != nil {
		t.Fatalf("commit record: %v", err)
	}

	return core.Withdrawal{ID: withdrawal.ID, CustomerID: customerID, Chain: core.Chain(chain), Asset: core.Asset(asset), Amount: mustParseBigInt(t, amount), TxHash: txHash}
}

func TestListBroadcastWithdrawals_ReturnsOnlyBroadcastWithTxHash(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")

	broadcastWithdrawal := claimAndBroadcast(t, pool, txBeginner, customerID, "base", "eth", "100", "0xbroadcasttxhash")
	// A second withdrawal, claimed but never recorded as broadcast — still 'signed', must
	// NOT show up in ListBroadcastWithdrawals.
	stillSignedWithdrawal := createApprovedWithdrawal(t, pool, txBeginner, customerID, "base", "eth", "200")
	claimCtx, claimTx, err := txBeginner.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, ok, err := withdrawalRepo.ClaimApprovedWithdrawal(claimCtx, core.ChainBase); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := claimTx.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}

	ctx := context.Background()
	listCtx, listTx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	got, err := withdrawalRepo.ListBroadcastWithdrawals(listCtx, core.ChainBase)
	_ = listTx.Rollback(ctx)
	if err != nil {
		t.Fatalf("list broadcast withdrawals: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d withdrawals, want exactly 1 (only the broadcast one)", len(got))
	}
	if got[0].ID != broadcastWithdrawal.ID {
		t.Fatalf("got withdrawal %s, want %s", got[0].ID, broadcastWithdrawal.ID)
	}
	if got[0].TxHash != "0xbroadcasttxhash" {
		t.Fatalf("got TxHash %q, want %q", got[0].TxHash, "0xbroadcasttxhash")
	}
	for _, w := range got {
		if w.ID == stillSignedWithdrawal.ID {
			t.Fatal("the still-signed withdrawal must not appear in ListBroadcastWithdrawals")
		}
	}
}

func TestSettleConfirmedWithdrawal_PostsDebitHoldCreditTreasury(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	withdrawal := claimAndBroadcast(t, pool, txBeginner, customerID, "base", "eth", "1000", "0xconfirmtxhash")

	// Before settlement: the hold placed by CreateWithdrawal is +1000 on the hold account.
	if got := accountBalance(t, pool, customerID, "base", "eth", "hold"); got.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("pre-settlement hold balance = %s, want 1000", got)
	}

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := withdrawalRepo.SettleConfirmedWithdrawal(txCtx, withdrawal.ID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("settle confirmed withdrawal: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var dbStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM withdrawals WHERE id = $1`, withdrawal.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query withdrawal: %v", err)
	}
	if dbStatus != core.WithdrawalStatusConfirmed {
		t.Fatalf("status = %q, want %q", dbStatus, core.WithdrawalStatusConfirmed)
	}

	if got := accountBalance(t, pool, customerID, "base", "eth", "hold"); got.Sign() != 0 {
		t.Fatalf("post-settlement hold balance = %s, want 0 (the hold was extinguished)", got)
	}

	var treasuryBalance string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(p.amount), 0)::text FROM accounts a LEFT JOIN postings p ON p.account_id = a.id
		 WHERE a.customer_id IS NULL AND a.chain = 'base' AND a.asset = 'eth' AND a.account_type = 'treasury'`,
	).Scan(&treasuryBalance); err != nil {
		t.Fatalf("query treasury balance: %v", err)
	}
	if got := mustParseBigInt(t, treasuryBalance); got.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("treasury balance = %s, want 1000 (credited the settled amount)", got)
	}

	if got := outboxEventCount(t, pool, "withdrawal.confirmed", withdrawal.ID); got != 1 {
		t.Fatalf("withdrawal.confirmed outbox events = %d, want exactly 1", got)
	}
}

func TestSettleFailedWithdrawal_PostsDebitHoldCreditAvailable(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	withdrawal := claimAndBroadcast(t, pool, txBeginner, customerID, "base", "eth", "1000", "0xfailedtxhash")

	// Post-hold, pre-settlement: available is 10000 - 1000 = 9000; hold is +1000.
	if got := accountBalance(t, pool, customerID, "base", "eth", "available"); got.Cmp(big.NewInt(9000)) != 0 {
		t.Fatalf("pre-settlement available balance = %s, want 9000", got)
	}

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := withdrawalRepo.SettleFailedWithdrawal(txCtx, withdrawal.ID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("settle failed withdrawal: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var dbStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM withdrawals WHERE id = $1`, withdrawal.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query withdrawal: %v", err)
	}
	if dbStatus != core.WithdrawalStatusFailed {
		t.Fatalf("status = %q, want %q", dbStatus, core.WithdrawalStatusFailed)
	}

	if got := accountBalance(t, pool, customerID, "base", "eth", "hold"); got.Sign() != 0 {
		t.Fatalf("post-settlement hold balance = %s, want 0 (the hold was released)", got)
	}
	if got := accountBalance(t, pool, customerID, "base", "eth", "available"); got.Cmp(big.NewInt(10000)) != 0 {
		t.Fatalf("post-settlement available balance = %s, want 10000 (fully restored)", got)
	}

	if got := outboxEventCount(t, pool, "withdrawal.failed", withdrawal.ID); got != 1 {
		t.Fatalf("withdrawal.failed outbox events = %d, want exactly 1", got)
	}
}

func TestSettleConfirmedWithdrawal_NotBroadcast_ReturnsError(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	// Still 'approved' — never claimed, never broadcast.
	withdrawal := createApprovedWithdrawal(t, pool, txBeginner, customerID, "base", "eth", "100")

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	err = withdrawalRepo.SettleConfirmedWithdrawal(txCtx, withdrawal.ID)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, core.ErrWithdrawalNotBroadcast) {
		t.Fatalf("err = %v, want core.ErrWithdrawalNotBroadcast", err)
	}
}

func TestSettleConfirmedWithdrawal_UnknownID_ReturnsErrWithdrawalNotFound(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	err = withdrawalRepo.SettleConfirmedWithdrawal(txCtx, uuid.New().String())
	_ = tx.Rollback(ctx)
	if !errors.Is(err, core.ErrWithdrawalNotFound) {
		t.Fatalf("err = %v, want core.ErrWithdrawalNotFound", err)
	}
}

// TestSettleConfirmedWithdrawal_NoTreasuryAccount_FailsLoud proves the I/O & Edge-Case
// Matrix's registry-gap row: with the (chain, asset)'s treasury account row removed (an
// artificially induced gap — migration 0011 always seeds one in a correctly migrated
// deployment), settlement fails loud with postgres.ErrNoTreasuryAccount and leaves the
// withdrawal at WithdrawalStatusBroadcast for the next poll to retry, rather than silently
// crediting nowhere or crashing.
func TestSettleConfirmedWithdrawal_NoTreasuryAccount_FailsLoud(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	withdrawal := claimAndBroadcast(t, pool, txBeginner, customerID, "base", "eth", "100", "0xnotreasurytxhash")

	if _, err := pool.Exec(context.Background(),
		`DELETE FROM accounts WHERE customer_id IS NULL AND chain = 'base' AND asset = 'eth' AND account_type = 'treasury'`,
	); err != nil {
		t.Fatalf("delete treasury account fixture: %v", err)
	}

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	err = withdrawalRepo.SettleConfirmedWithdrawal(txCtx, withdrawal.ID)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, postgres.ErrNoTreasuryAccount) {
		t.Fatalf("err = %v, want postgres.ErrNoTreasuryAccount", err)
	}

	var dbStatus string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM withdrawals WHERE id = $1`, withdrawal.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query withdrawal: %v", err)
	}
	if dbStatus != core.WithdrawalStatusBroadcast {
		t.Fatalf("status = %q, want %q (unchanged, retried next poll)", dbStatus, core.WithdrawalStatusBroadcast)
	}
}

// TestSettleConfirmedWithdrawal_NoHoldAccount_FailsLoud and
// TestSettleFailedWithdrawal_NoAvailableAccount_FailsLoud prove settleWithdrawal's other two
// registry-gap sentinels (re-review 2026-07-21: previously ad hoc fmt.Errorf with no
// matchable sentinel, unlike ErrNoTreasuryAccount's own dedicated test above) — a missing
// customer account for either side of the settlement fails loud and leaves the withdrawal
// unchanged for the next poll to retry, exactly mirroring the treasury case.
func TestSettleConfirmedWithdrawal_NoHoldAccount_FailsLoud(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	withdrawal := claimAndBroadcast(t, pool, txBeginner, customerID, "base", "eth", "100", "0xnoholdtxhash")

	// The hold account already has a posting (Story 3.2's own hold-placement leg) — delete
	// it first, otherwise the FK from postings.account_id would block deleting the account
	// row below. Breaking the double-entry invariant here is fine: this is purely an
	// artificially induced gap for this test, never a realistic production state.
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM postings WHERE account_id = (SELECT id FROM accounts WHERE customer_id = $1 AND chain = 'base' AND asset = 'eth' AND account_type = 'hold')`,
		customerID,
	); err != nil {
		t.Fatalf("delete hold account's posting fixture: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM accounts WHERE customer_id = $1 AND chain = 'base' AND asset = 'eth' AND account_type = 'hold'`,
		customerID,
	); err != nil {
		t.Fatalf("delete hold account fixture: %v", err)
	}

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	err = withdrawalRepo.SettleConfirmedWithdrawal(txCtx, withdrawal.ID)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, postgres.ErrNoHoldAccount) {
		t.Fatalf("err = %v, want postgres.ErrNoHoldAccount", err)
	}

	var dbStatus string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM withdrawals WHERE id = $1`, withdrawal.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query withdrawal: %v", err)
	}
	if dbStatus != core.WithdrawalStatusBroadcast {
		t.Fatalf("status = %q, want %q (unchanged, retried next poll)", dbStatus, core.WithdrawalStatusBroadcast)
	}
}

func TestSettleFailedWithdrawal_NoAvailableAccount_FailsLoud(t *testing.T) {
	pool := newTestPool(t)
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()

	customerID := createTestCustomerWithBalance(t, pool, txBeginner, "base", "eth", "10000")
	withdrawal := claimAndBroadcast(t, pool, txBeginner, customerID, "base", "eth", "100", "0xnoavailabletxhash")

	// The available account already has postings (the seed balance and Story 3.2's own
	// hold-placement debit leg) — delete them first, otherwise the FK from
	// postings.account_id would block deleting the account row below (same reasoning as
	// TestSettleConfirmedWithdrawal_NoHoldAccount_FailsLoud above).
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM postings WHERE account_id = (SELECT id FROM accounts WHERE customer_id = $1 AND chain = 'base' AND asset = 'eth' AND account_type = 'available')`,
		customerID,
	); err != nil {
		t.Fatalf("delete available account's postings fixture: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM accounts WHERE customer_id = $1 AND chain = 'base' AND asset = 'eth' AND account_type = 'available'`,
		customerID,
	); err != nil {
		t.Fatalf("delete available account fixture: %v", err)
	}

	ctx := context.Background()
	txCtx, tx, err := txBeginner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	err = withdrawalRepo.SettleFailedWithdrawal(txCtx, withdrawal.ID)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, postgres.ErrNoAvailableAccount) {
		t.Fatalf("err = %v, want postgres.ErrNoAvailableAccount", err)
	}

	var dbStatus string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM withdrawals WHERE id = $1`, withdrawal.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query withdrawal: %v", err)
	}
	if dbStatus != core.WithdrawalStatusBroadcast {
		t.Fatalf("status = %q, want %q (unchanged, retried next poll)", dbStatus, core.WithdrawalStatusBroadcast)
	}
}
