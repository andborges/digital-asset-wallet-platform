package core_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// recordingTxBeginner is a core.TxBeginner test double that records EVERY transaction it
// opens (unlike fakeTxBeginner in track_deposits_test.go, which only remembers the last
// one) — SignAndBroadcastWithdrawal/PollWithdrawalReceipts open more than one transaction
// per Execute call, and these tests need to assert on each one's commit/rollback outcome
// individually.
type recordingTxBeginner struct {
	txs []*fakeTx
	err error
}

func (b *recordingTxBeginner) Begin(ctx context.Context) (context.Context, core.Tx, error) {
	if b.err != nil {
		return ctx, nil, b.err
	}
	tx := &fakeTx{}
	b.txs = append(b.txs, tx)
	return ctx, tx, nil
}

// fakeSignAndBroadcastRepo is a test double for core.WithdrawalRepository, used only by
// SignAndBroadcastWithdrawal's and PollWithdrawalReceipts' own tests. Every method this
// story's use cases don't call panics, catching an accidental cross-wire.
type fakeSignAndBroadcastRepo struct {
	claimResult core.Withdrawal
	claimOK     bool
	claimErr    error
	claimCalls  int

	// listSignedResult/listSignedErr back ListSignedWithdrawals (Story 3.5's own resume
	// set) — deliberately named distinctly from listResult/listErr below (which remain
	// ListBroadcastWithdrawals' own fields, unchanged, since poll_withdrawal_receipts_test.go
	// already references them by that name).
	listSignedResult []core.Withdrawal
	listSignedErr    error
	listSignedCalls  int

	recordSignedErr       error
	recordSignedCalls     int
	gotRecordSignedID     string
	gotRecordSignedTxHash string
	gotRecordSignedTxHex  string

	markBroadcastErr   error
	markBroadcastCalls []string

	listResult []core.Withdrawal
	listErr    error

	// settleErrByID, when it has an entry for a given withdrawal id, overrides
	// settleConfirmedErr/settleFailedErr for that one call — lets multi-withdrawal tests
	// make exactly one settlement fail while the rest succeed.
	settleErrByID        map[string]error
	settleConfirmedErr   error
	settleFailedErr      error
	settleConfirmedCalls []string
	settleFailedCalls    []string

	listStuckResult       []core.Withdrawal
	listStuckErr          error
	gotListStuckOlderThan time.Duration

	// markStuckErrByID, when it has an entry for a given withdrawal id, overrides
	// markStuckErr for that one call — mirrors settleErrByID's identical purpose.
	markStuckErrByID map[string]error
	markStuckErr     error
	markStuckCalls   []string
}

func (f *fakeSignAndBroadcastRepo) CreateWithdrawal(context.Context, core.WithdrawalRequest, *big.Int, string) (core.Withdrawal, error) {
	panic("fakeSignAndBroadcastRepo.CreateWithdrawal must not be called by these tests")
}

func (f *fakeSignAndBroadcastRepo) ApproveWithdrawal(context.Context, string, string, string) (core.Withdrawal, error) {
	panic("fakeSignAndBroadcastRepo.ApproveWithdrawal must not be called by these tests")
}

func (f *fakeSignAndBroadcastRepo) ClaimApprovedWithdrawal(_ context.Context, _ core.Chain) (core.Withdrawal, bool, error) {
	f.claimCalls++
	return f.claimResult, f.claimOK, f.claimErr
}

func (f *fakeSignAndBroadcastRepo) RecordSignedTx(_ context.Context, id, txHash, signedTxHex string) error {
	f.recordSignedCalls++
	f.gotRecordSignedID, f.gotRecordSignedTxHash, f.gotRecordSignedTxHex = id, txHash, signedTxHex
	return f.recordSignedErr
}

func (f *fakeSignAndBroadcastRepo) MarkBroadcast(_ context.Context, id string) error {
	f.markBroadcastCalls = append(f.markBroadcastCalls, id)
	return f.markBroadcastErr
}

func (f *fakeSignAndBroadcastRepo) ListSignedWithdrawals(context.Context, core.Chain) ([]core.Withdrawal, error) {
	f.listSignedCalls++
	return f.listSignedResult, f.listSignedErr
}

func (f *fakeSignAndBroadcastRepo) ListBroadcastWithdrawals(context.Context, core.Chain) ([]core.Withdrawal, error) {
	return f.listResult, f.listErr
}

func (f *fakeSignAndBroadcastRepo) SettleConfirmedWithdrawal(_ context.Context, id string) error {
	f.settleConfirmedCalls = append(f.settleConfirmedCalls, id)
	if f.settleErrByID != nil {
		if err, ok := f.settleErrByID[id]; ok {
			return err
		}
	}
	return f.settleConfirmedErr
}

func (f *fakeSignAndBroadcastRepo) SettleFailedWithdrawal(_ context.Context, id string) error {
	f.settleFailedCalls = append(f.settleFailedCalls, id)
	if f.settleErrByID != nil {
		if err, ok := f.settleErrByID[id]; ok {
			return err
		}
	}
	return f.settleFailedErr
}

func (f *fakeSignAndBroadcastRepo) ListStuckCandidates(_ context.Context, _ core.Chain, olderThan time.Duration) ([]core.Withdrawal, error) {
	f.gotListStuckOlderThan = olderThan
	return f.listStuckResult, f.listStuckErr
}

func (f *fakeSignAndBroadcastRepo) MarkStuckAlerted(_ context.Context, id string) error {
	f.markStuckCalls = append(f.markStuckCalls, id)
	if f.markStuckErrByID != nil {
		if err, ok := f.markStuckErrByID[id]; ok {
			return err
		}
	}
	return f.markStuckErr
}

// fakeSigner is a test double for core.Signer.
type fakeSigner struct {
	sig       [65]byte
	err       error
	calls     int
	gotChain  core.Chain
	gotDigest [32]byte
}

func (f *fakeSigner) Sign(_ context.Context, chain core.Chain, digest [32]byte) ([65]byte, error) {
	f.calls++
	f.gotChain, f.gotDigest = chain, digest
	return f.sig, f.err
}

// receiptFixture is one canned GetFinalizedReceipt response, keyed by tx hash in
// fakeBroadcaster.receiptsByTxHash for multi-withdrawal poll tests.
type receiptFixture struct {
	found, success bool
	err            error
}

// fakeBroadcaster is a test double for core.TransactionBroadcaster.
type fakeBroadcaster struct {
	buildDigest   [32]byte
	buildUnsigned []byte
	buildErr      error
	buildCalls    int
	gotChain      core.Chain
	gotAsset      core.Asset
	gotNonce      int64
	gotTo         string
	gotAmount     *big.Int

	assembleSignedTx []byte
	assembleTxHash   string
	assembleErr      error
	assembleCalls    int
	gotUnsignedTx    []byte
	gotSignature     [65]byte

	sendErr      error
	sendCalls    int
	gotSendChain core.Chain
	gotSendTx    []byte

	// Fixed (single-withdrawal-test) receipt response, used when receiptsByTxHash is nil.
	receiptFound     bool
	receiptSuccess   bool
	receiptErr       error
	receiptCalls     int
	gotReceiptChain  core.Chain
	gotReceiptTxHash string
	// receiptsByTxHash, when non-nil, overrides the fixed fields above per tx hash — for
	// multi-withdrawal poll tests where different withdrawals need different outcomes.
	receiptsByTxHash map[string]receiptFixture
}

func (f *fakeBroadcaster) BuildUnsignedWithdrawal(_ context.Context, chain core.Chain, asset core.Asset, nonce int64, to string, amount *big.Int) ([32]byte, []byte, error) {
	f.buildCalls++
	f.gotChain, f.gotAsset, f.gotNonce, f.gotTo, f.gotAmount = chain, asset, nonce, to, amount
	if f.buildErr != nil {
		return [32]byte{}, nil, f.buildErr
	}
	return f.buildDigest, f.buildUnsigned, nil
}

func (f *fakeBroadcaster) AssembleSignedTx(unsignedTx []byte, signature [65]byte) ([]byte, string, error) {
	f.assembleCalls++
	f.gotUnsignedTx, f.gotSignature = unsignedTx, signature
	if f.assembleErr != nil {
		return nil, "", f.assembleErr
	}
	return f.assembleSignedTx, f.assembleTxHash, nil
}

func (f *fakeBroadcaster) SendRawTransaction(_ context.Context, chain core.Chain, signedTx []byte) error {
	f.sendCalls++
	f.gotSendChain, f.gotSendTx = chain, signedTx
	return f.sendErr
}

func (f *fakeBroadcaster) GetFinalizedReceipt(_ context.Context, chain core.Chain, txHash string) (bool, bool, error) {
	f.receiptCalls++
	f.gotReceiptChain, f.gotReceiptTxHash = chain, txHash
	if f.receiptsByTxHash != nil {
		fx, ok := f.receiptsByTxHash[txHash]
		if !ok {
			panic("fakeBroadcaster.GetFinalizedReceipt: no fixture registered for tx hash " + txHash)
		}
		return fx.found, fx.success, fx.err
	}
	return f.receiptFound, f.receiptSuccess, f.receiptErr
}

var _ core.TransactionBroadcaster = (*fakeBroadcaster)(nil)
var _ core.Signer = (*fakeSigner)(nil)

func TestSignAndBroadcastWithdrawal_Execute_NothingToResumeOrClaim(t *testing.T) {
	repo := &fakeSignAndBroadcastRepo{claimOK: false}
	broadcaster := &fakeBroadcaster{}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, broadcaster, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, true)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if claimed {
		t.Fatal("claimed = true, want false when nothing was resumable or approved to claim")
	}
	if repo.listSignedCalls != 1 {
		t.Fatalf("listSignedCalls = %d, want 1 (resume is always checked first)", repo.listSignedCalls)
	}
	if repo.claimCalls != 1 {
		t.Fatalf("claimCalls = %d, want 1", repo.claimCalls)
	}
	if broadcaster.buildCalls != 0 {
		t.Fatal("BuildUnsignedWithdrawal must not be called when nothing was claimed")
	}
	// [0] the list-signed read, [1] the claim attempt — both roll back, nothing to commit.
	if len(txBeginner.txs) != 2 {
		t.Fatalf("opened %d transactions, want exactly 2 (list-signed, then claim)", len(txBeginner.txs))
	}
	for i, tx := range txBeginner.txs {
		if tx.committed || !tx.rolledBack {
			t.Fatalf("transaction %d should roll back, never commit, when nothing was resumed or claimed", i)
		}
	}
}

func TestSignAndBroadcastWithdrawal_Execute_ListSignedError(t *testing.T) {
	repo := &fakeSignAndBroadcastRepo{listSignedErr: errors.New("db unavailable")}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, &fakeBroadcaster{}, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, true)
	if err == nil {
		t.Fatal("err = nil, want a wrapped list-signed error")
	}
	if claimed {
		t.Fatal("claimed = true, want false on a list-signed error")
	}
	if repo.claimCalls != 0 {
		t.Fatal("ClaimApprovedWithdrawal must not be called when listing signed withdrawals itself failed")
	}
}

// TestSignAndBroadcastWithdrawal_Execute_AllowClaimFalse_NothingToResume_NeverClaims proves
// allowClaim's own contract directly (re-review 2026-07-22, both an adversarial and an
// edge-case review pass independently flagged the original version's lack of this
// distinction): with nothing resumable and allowClaim=false (the broadcaster's liveness
// gate, cmd/walletd's runBroadcaster, reporting the watcher cursor is stale), Execute must
// return claimed=false, err=nil WITHOUT ever calling ClaimApprovedWithdrawal — claiming
// allocates a brand-new nonce, exactly what the liveness gate exists to prevent.
func TestSignAndBroadcastWithdrawal_Execute_AllowClaimFalse_NothingToResume_NeverClaims(t *testing.T) {
	repo := &fakeSignAndBroadcastRepo{}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, &fakeBroadcaster{}, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, false)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if claimed {
		t.Fatal("claimed = true, want false")
	}
	if repo.claimCalls != 0 {
		t.Fatalf("claimCalls = %d, want 0 — allowClaim=false must never fall back to ClaimApprovedWithdrawal", repo.claimCalls)
	}
	// Only the list-signed read happens; no claim attempt is ever opened.
	if len(txBeginner.txs) != 1 {
		t.Fatalf("opened %d transactions, want exactly 1 (list-signed only)", len(txBeginner.txs))
	}
}

// TestSignAndBroadcastWithdrawal_Execute_AllowClaimFalse_StillResumes proves the other half
// of allowClaim's contract: a stale liveness gate (allowClaim=false) must NOT block
// resuming an already-signed withdrawal — resuming allocates no new nonce and strands
// nothing new, so it proceeds exactly as it would with allowClaim=true.
func TestSignAndBroadcastWithdrawal_Execute_AllowClaimFalse_StillResumes(t *testing.T) {
	nonce := int64(9)
	persistedSignedTx := []byte("already-signed-bytes")
	resumed := core.Withdrawal{
		ID:                 "withdrawal-resume-live-gate",
		Chain:              core.ChainBase,
		Asset:              core.AssetETH,
		Amount:             big.NewInt(500),
		DestinationAddress: "0xabc",
		Status:             core.WithdrawalStatusSigned,
		Nonce:              &nonce,
		TxHash:             "0xalreadyhash",
		SignedTx:           persistedSignedTx,
	}
	repo := &fakeSignAndBroadcastRepo{listSignedResult: []core.Withdrawal{resumed}}
	broadcaster := &fakeBroadcaster{}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, broadcaster, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, false)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !claimed {
		t.Fatal("claimed = false, want true — resuming must proceed even when allowClaim is false")
	}
	if repo.claimCalls != 0 {
		t.Fatal("ClaimApprovedWithdrawal must not be called — a resumable withdrawal already existed")
	}
	if broadcaster.sendCalls != 1 || !bytes.Equal(broadcaster.gotSendTx, persistedSignedTx) {
		t.Fatal("SendRawTransaction must still be called with the already-persisted signed bytes despite allowClaim=false")
	}
	if len(repo.markBroadcastCalls) != 1 || repo.markBroadcastCalls[0] != resumed.ID {
		t.Fatalf("markBroadcastCalls = %v, want [%q]", repo.markBroadcastCalls, resumed.ID)
	}
}

func TestSignAndBroadcastWithdrawal_Execute_ClaimError(t *testing.T) {
	repo := &fakeSignAndBroadcastRepo{claimErr: errors.New("db unavailable")}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, &fakeBroadcaster{}, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, true)
	if err == nil {
		t.Fatal("err = nil, want a wrapped claim error")
	}
	if claimed {
		t.Fatal("claimed = true, want false on a claim error")
	}
	if len(txBeginner.txs) != 2 || !txBeginner.txs[1].rolledBack {
		t.Fatal("the claim transaction should roll back on a claim error")
	}
}

// TestSignAndBroadcastWithdrawal_Execute_FreshClaim_HappyPath exercises Story 3.5's fresh-
// claim branch: nothing to resume, so ClaimApprovedWithdrawal runs, followed by the full
// build -> sign -> assemble -> RecordSignedTx -> send -> MarkBroadcast pipeline — with
// RecordSignedTx now committing BEFORE the send call (Boundaries & Constraints), not after.
func TestSignAndBroadcastWithdrawal_Execute_FreshClaim_HappyPath(t *testing.T) {
	nonce := int64(7)
	withdrawal := core.Withdrawal{
		ID:                 "withdrawal-1",
		CustomerID:         "customer-1",
		Chain:              core.ChainBase,
		Asset:              core.AssetETH,
		Amount:             big.NewInt(1_000_000),
		DestinationAddress: "0x00000000000000000000000000000000000000AA",
		Status:             core.WithdrawalStatusSigned,
		Nonce:              &nonce,
	}
	repo := &fakeSignAndBroadcastRepo{claimResult: withdrawal, claimOK: true}
	signer := &fakeSigner{sig: [65]byte{1, 2, 3, 4, 5}}
	broadcaster := &fakeBroadcaster{
		buildDigest:      [32]byte{9, 9, 9},
		buildUnsigned:    []byte("unsigned-tx-bytes"),
		assembleSignedTx: []byte("signed-tx-bytes"),
		assembleTxHash:   "0xdeadbeef",
	}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, signer, broadcaster, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, true)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !claimed {
		t.Fatal("claimed = false, want true")
	}

	if broadcaster.buildCalls != 1 {
		t.Fatalf("BuildUnsignedWithdrawal calls = %d, want 1", broadcaster.buildCalls)
	}
	if broadcaster.gotChain != core.ChainBase || broadcaster.gotAsset != core.AssetETH || broadcaster.gotNonce != nonce ||
		broadcaster.gotTo != withdrawal.DestinationAddress || broadcaster.gotAmount.Cmp(withdrawal.Amount) != 0 {
		t.Fatalf("BuildUnsignedWithdrawal args = (%v, %v, %d, %q, %v), want (%v, %v, %d, %q, %v)",
			broadcaster.gotChain, broadcaster.gotAsset, broadcaster.gotNonce, broadcaster.gotTo, broadcaster.gotAmount,
			core.ChainBase, core.AssetETH, nonce, withdrawal.DestinationAddress, withdrawal.Amount)
	}

	if signer.calls != 1 || signer.gotChain != core.ChainBase || signer.gotDigest != broadcaster.buildDigest {
		t.Fatalf("Sign called with (%v, %v), want (%v, %v)", signer.gotChain, signer.gotDigest, core.ChainBase, broadcaster.buildDigest)
	}

	if broadcaster.assembleCalls != 1 || !bytes.Equal(broadcaster.gotUnsignedTx, broadcaster.buildUnsigned) || broadcaster.gotSignature != signer.sig {
		t.Fatal("AssembleSignedTx was not called with the unsigned tx bytes and signature this call produced")
	}

	if repo.recordSignedCalls != 1 || repo.gotRecordSignedID != withdrawal.ID || repo.gotRecordSignedTxHash != broadcaster.assembleTxHash {
		t.Fatalf("RecordSignedTx called with (%q, %q), want (%q, %q)", repo.gotRecordSignedID, repo.gotRecordSignedTxHash, withdrawal.ID, broadcaster.assembleTxHash)
	}
	if wantHex := hex.EncodeToString(broadcaster.assembleSignedTx); repo.gotRecordSignedTxHex != wantHex {
		t.Fatalf("RecordSignedTx signedTxHex = %q, want %q (hex of the assembled signed bytes)", repo.gotRecordSignedTxHex, wantHex)
	}

	if broadcaster.sendCalls != 1 || broadcaster.gotSendChain != core.ChainBase || !bytes.Equal(broadcaster.gotSendTx, broadcaster.assembleSignedTx) {
		t.Fatal("SendRawTransaction was not called with the assembled signed tx bytes")
	}

	if len(repo.markBroadcastCalls) != 1 || repo.markBroadcastCalls[0] != withdrawal.ID {
		t.Fatalf("markBroadcastCalls = %v, want [%q]", repo.markBroadcastCalls, withdrawal.ID)
	}

	// [0] list-signed (rolled back, nothing resumable), [1] claim (commit), [2] record-signed
	// (commit — BEFORE the send call above), [3] mark-broadcast (commit).
	if len(txBeginner.txs) != 4 {
		t.Fatalf("opened %d transactions, want exactly 4 (list-signed, claim, record-signed, mark-broadcast)", len(txBeginner.txs))
	}
	if txBeginner.txs[0].committed {
		t.Fatal("the list-signed transaction should never commit")
	}
	for i := 1; i < 4; i++ {
		if !txBeginner.txs[i].committed {
			t.Fatalf("transaction %d should commit on the happy path", i)
		}
	}
}

func TestSignAndBroadcastWithdrawal_Execute_BuildFails_ClaimStillCommitted(t *testing.T) {
	nonce := int64(1)
	withdrawal := core.Withdrawal{ID: "withdrawal-1", DestinationAddress: "0xabc", Amount: big.NewInt(1), Nonce: &nonce}
	repo := &fakeSignAndBroadcastRepo{claimResult: withdrawal, claimOK: true}
	broadcaster := &fakeBroadcaster{buildErr: errors.New("rpc endpoint unreachable")}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, broadcaster, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, true)
	if err == nil {
		t.Fatal("err = nil, want a wrapped build error")
	}
	// AD-11: the nonce + broadcast_attempts row is already durably committed by the time
	// BuildUnsignedWithdrawal is even called — a downstream failure here must never be
	// reported as "nothing was claimed."
	if !claimed {
		t.Fatal("claimed = false, want true — the withdrawal was durably claimed before the build failure")
	}
	// [0] list-signed (rolled back), [1] claim (commit) — a build failure never opens a
	// record-signed-tx transaction.
	if len(txBeginner.txs) != 2 {
		t.Fatalf("opened %d transactions, want exactly 2 (list-signed, then claim)", len(txBeginner.txs))
	}
	if !txBeginner.txs[1].committed {
		t.Fatal("the claim transaction should have committed BEFORE the build call ran")
	}
	if repo.recordSignedCalls != 0 {
		t.Fatal("RecordSignedTx must not be called when BuildUnsignedWithdrawal fails")
	}
}

func TestSignAndBroadcastWithdrawal_Execute_SignFails(t *testing.T) {
	nonce := int64(1)
	withdrawal := core.Withdrawal{ID: "withdrawal-1", DestinationAddress: "0xabc", Amount: big.NewInt(1), Nonce: &nonce}
	repo := &fakeSignAndBroadcastRepo{claimResult: withdrawal, claimOK: true}
	signer := &fakeSigner{err: errors.New("kms unavailable")}
	broadcaster := &fakeBroadcaster{buildUnsigned: []byte("unsigned")}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, signer, broadcaster, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, true)
	if err == nil {
		t.Fatal("err = nil, want a wrapped sign error")
	}
	if !claimed {
		t.Fatal("claimed = false, want true")
	}
	if broadcaster.assembleCalls != 0 || broadcaster.sendCalls != 0 {
		t.Fatal("AssembleSignedTx/SendRawTransaction must not be called when Sign fails")
	}
	if repo.recordSignedCalls != 0 {
		t.Fatal("RecordSignedTx must not be called when Sign fails")
	}
	if len(txBeginner.txs) != 2 {
		t.Fatalf("opened %d transactions, want exactly 2 (list-signed, then claim)", len(txBeginner.txs))
	}
}

// TestSignAndBroadcastWithdrawal_Execute_SendFails_LeavesSignedWithSignedTxPersisted proves
// Story 3.5's core restructuring: RecordSignedTx now commits BEFORE the send is attempted,
// so a send failure leaves the signed bytes already durably persisted — never re-signed on
// the next resume — and MarkBroadcast is never called (I/O & Edge-Case Matrix: "stays
// signed; retried next poll; no immediate failed").
func TestSignAndBroadcastWithdrawal_Execute_SendFails_LeavesSignedWithSignedTxPersisted(t *testing.T) {
	nonce := int64(1)
	withdrawal := core.Withdrawal{ID: "withdrawal-1", DestinationAddress: "0xabc", Amount: big.NewInt(1), Nonce: &nonce}
	repo := &fakeSignAndBroadcastRepo{claimResult: withdrawal, claimOK: true}
	broadcaster := &fakeBroadcaster{
		buildUnsigned:    []byte("unsigned"),
		assembleSignedTx: []byte("signed"),
		assembleTxHash:   "0xhash",
		sendErr:          errors.New("connection reset"),
	}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, broadcaster, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, true)
	if err == nil {
		t.Fatal("err = nil, want a wrapped send error")
	}
	if !claimed {
		t.Fatal("claimed = false, want true")
	}
	if repo.recordSignedCalls != 1 {
		t.Fatal("RecordSignedTx SHOULD have been called (and committed) before the send attempt — Story 3.5's own restructuring")
	}
	if len(repo.markBroadcastCalls) != 0 {
		t.Fatal("MarkBroadcast must not be called when SendRawTransaction fails with an unrecognized error")
	}
	// [0] list-signed, [1] claim, [2] record-signed — no mark-broadcast transaction on a
	// send failure that isn't recognized as "already known"/"nonce too low".
	if len(txBeginner.txs) != 3 {
		t.Fatalf("opened %d transactions, want exactly 3 (list-signed, claim, record-signed)", len(txBeginner.txs))
	}
	if !txBeginner.txs[2].committed {
		t.Fatal("the record-signed-tx transaction should still commit even though the subsequent send failed")
	}
}

// TestSignAndBroadcastWithdrawal_Execute_Resume_ResendsPersistedBytesWithoutResigning proves
// the resume path (Story 3.5's core acceptance criterion, AC3): a withdrawal already
// ListSignedWithdrawals-returned with a non-empty SignedTx skips build/sign/assemble
// entirely and resends those EXACT bytes.
func TestSignAndBroadcastWithdrawal_Execute_Resume_ResendsPersistedBytesWithoutResigning(t *testing.T) {
	nonce := int64(3)
	persistedSignedTx := []byte("already-signed-bytes")
	resumed := core.Withdrawal{
		ID:                 "withdrawal-resume-1",
		Chain:              core.ChainBase,
		Asset:              core.AssetETH,
		Amount:             big.NewInt(500),
		DestinationAddress: "0xabc",
		Status:             core.WithdrawalStatusSigned,
		Nonce:              &nonce,
		TxHash:             "0xalreadyhash",
		SignedTx:           persistedSignedTx,
	}
	repo := &fakeSignAndBroadcastRepo{listSignedResult: []core.Withdrawal{resumed}}
	signer := &fakeSigner{}
	broadcaster := &fakeBroadcaster{}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, signer, broadcaster, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, true)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !claimed {
		t.Fatal("claimed = false, want true")
	}
	if repo.claimCalls != 0 {
		t.Fatal("ClaimApprovedWithdrawal must not be called when a resumable signed withdrawal already exists")
	}
	if broadcaster.buildCalls != 0 || signer.calls != 0 || broadcaster.assembleCalls != 0 {
		t.Fatal("build/sign/assemble must NEVER run on the resume path — resending must never re-sign")
	}
	if repo.recordSignedCalls != 0 {
		t.Fatal("RecordSignedTx must not be called again on the resume path — the bytes are already persisted")
	}
	if broadcaster.sendCalls != 1 || !bytes.Equal(broadcaster.gotSendTx, persistedSignedTx) {
		t.Fatal("SendRawTransaction must be called with the exact already-persisted signed bytes")
	}
	if len(repo.markBroadcastCalls) != 1 || repo.markBroadcastCalls[0] != resumed.ID {
		t.Fatalf("markBroadcastCalls = %v, want [%q]", repo.markBroadcastCalls, resumed.ID)
	}
	// [0] list-signed (commit not required — it's a read), [1] mark-broadcast (commit).
	if len(txBeginner.txs) != 2 {
		t.Fatalf("opened %d transactions, want exactly 2 (list-signed, mark-broadcast)", len(txBeginner.txs))
	}
	if !txBeginner.txs[1].committed {
		t.Fatal("the mark-broadcast transaction should commit")
	}
}

// TestSignAndBroadcastWithdrawal_Execute_Resume_NoSignedTxYet_RunsFullPipeline covers the
// OTHER resume case (I/O & Edge-Case Matrix: "Crash after persisting signed_tx, before the
// send call returns" is the with-SignedTx case above; this is the earlier crash point —
// claimed, but interrupted before signing/RecordSignedTx ever ran): resumed via
// ListSignedWithdrawals (never re-claimed) but with an empty SignedTx, so it runs the exact
// same build/sign/assemble/RecordSignedTx/send pipeline a fresh claim would.
func TestSignAndBroadcastWithdrawal_Execute_Resume_NoSignedTxYet_RunsFullPipeline(t *testing.T) {
	nonce := int64(4)
	resumed := core.Withdrawal{
		ID:                 "withdrawal-resume-2",
		Chain:              core.ChainBase,
		Asset:              core.AssetETH,
		Amount:             big.NewInt(500),
		DestinationAddress: "0xabc",
		Status:             core.WithdrawalStatusSigned,
		Nonce:              &nonce,
	}
	repo := &fakeSignAndBroadcastRepo{listSignedResult: []core.Withdrawal{resumed}}
	signer := &fakeSigner{sig: [65]byte{9}}
	broadcaster := &fakeBroadcaster{
		buildUnsigned:    []byte("unsigned"),
		assembleSignedTx: []byte("signed"),
		assembleTxHash:   "0xresumehash",
	}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, signer, broadcaster, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, true)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !claimed {
		t.Fatal("claimed = false, want true")
	}
	if repo.claimCalls != 0 {
		t.Fatal("ClaimApprovedWithdrawal must not be called — the withdrawal was already resumed via ListSignedWithdrawals")
	}
	if broadcaster.buildCalls != 1 || signer.calls != 1 || broadcaster.assembleCalls != 1 {
		t.Fatal("build/sign/assemble must run when the resumed withdrawal has no SignedTx yet")
	}
	if repo.recordSignedCalls != 1 {
		t.Fatal("RecordSignedTx must be called for a resumed withdrawal with no SignedTx yet")
	}
	if len(repo.markBroadcastCalls) != 1 || repo.markBroadcastCalls[0] != resumed.ID {
		t.Fatalf("markBroadcastCalls = %v, want [%q]", repo.markBroadcastCalls, resumed.ID)
	}
}

// TestSignAndBroadcastWithdrawal_Execute_Resend_AlreadyKnownError_StillMarksBroadcast and
// its "nonce too low" sibling prove Boundaries & Constraints' own idempotency rule: a resend
// (or fresh send) erroring with either recognized phrase still transitions to broadcast,
// exactly as if the send had returned no error at all.
func TestSignAndBroadcastWithdrawal_Execute_Resend_AlreadyKnownError_StillMarksBroadcast(t *testing.T) {
	for _, errText := range []string{"already known", "ALREADY KNOWN", "replacement transaction underpriced: already known"} {
		t.Run(errText, func(t *testing.T) {
			nonce := int64(1)
			resumed := core.Withdrawal{ID: "withdrawal-1", Nonce: &nonce, SignedTx: []byte("bytes")}
			repo := &fakeSignAndBroadcastRepo{listSignedResult: []core.Withdrawal{resumed}}
			broadcaster := &fakeBroadcaster{sendErr: errors.New(errText)}
			txBeginner := &recordingTxBeginner{}
			uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, broadcaster, txBeginner)

			claimed, err := uc.Execute(context.Background(), core.ChainBase, true)
			if err != nil {
				t.Fatalf("err = %v, want nil — an 'already known' send error is treated as success", err)
			}
			if !claimed {
				t.Fatal("claimed = false, want true")
			}
			if len(repo.markBroadcastCalls) != 1 || repo.markBroadcastCalls[0] != resumed.ID {
				t.Fatalf("markBroadcastCalls = %v, want [%q]", repo.markBroadcastCalls, resumed.ID)
			}
		})
	}
}

func TestSignAndBroadcastWithdrawal_Execute_Resend_NonceTooLowError_StillMarksBroadcast(t *testing.T) {
	nonce := int64(1)
	resumed := core.Withdrawal{ID: "withdrawal-1", Nonce: &nonce, SignedTx: []byte("bytes")}
	repo := &fakeSignAndBroadcastRepo{listSignedResult: []core.Withdrawal{resumed}}
	broadcaster := &fakeBroadcaster{sendErr: errors.New("nonce too low")}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, broadcaster, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, true)
	if err != nil {
		t.Fatalf("err = %v, want nil — a 'nonce too low' send error is treated as success", err)
	}
	if !claimed {
		t.Fatal("claimed = false, want true")
	}
	if len(repo.markBroadcastCalls) != 1 || repo.markBroadcastCalls[0] != resumed.ID {
		t.Fatalf("markBroadcastCalls = %v, want [%q]", repo.markBroadcastCalls, resumed.ID)
	}
}

// TestSignAndBroadcastWithdrawal_Execute_Resend_OtherError_LeavesSignedNoMarkBroadcast
// proves the negative: an unrecognized send error on resend leaves the withdrawal at
// signed, never calling MarkBroadcast — Boundaries & Constraints' own "any OTHER send error
// just returns without changing status."
func TestSignAndBroadcastWithdrawal_Execute_Resend_OtherError_LeavesSignedNoMarkBroadcast(t *testing.T) {
	nonce := int64(1)
	resumed := core.Withdrawal{ID: "withdrawal-1", Nonce: &nonce, SignedTx: []byte("bytes")}
	repo := &fakeSignAndBroadcastRepo{listSignedResult: []core.Withdrawal{resumed}}
	broadcaster := &fakeBroadcaster{sendErr: errors.New("connection reset by peer")}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, broadcaster, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, true)
	if err == nil {
		t.Fatal("err = nil, want a wrapped send error")
	}
	if !claimed {
		t.Fatal("claimed = false, want true")
	}
	if len(repo.markBroadcastCalls) != 0 {
		t.Fatal("MarkBroadcast must not be called on an unrecognized send error")
	}
}

func TestSignAndBroadcastWithdrawal_Execute_MarkBroadcastFails(t *testing.T) {
	nonce := int64(1)
	resumed := core.Withdrawal{ID: "withdrawal-1", Nonce: &nonce, SignedTx: []byte("bytes")}
	repo := &fakeSignAndBroadcastRepo{listSignedResult: []core.Withdrawal{resumed}, markBroadcastErr: errors.New("db unavailable")}
	broadcaster := &fakeBroadcaster{}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, broadcaster, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase, true)
	if err == nil {
		t.Fatal("err = nil, want a wrapped mark-broadcast error")
	}
	if !claimed {
		t.Fatal("claimed = false, want true")
	}
}
