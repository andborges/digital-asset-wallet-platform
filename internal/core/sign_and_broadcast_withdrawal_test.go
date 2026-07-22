package core_test

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"testing"

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

	recordErr       error
	recordCalls     int
	gotRecordID     string
	gotRecordTxHash string

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

func (f *fakeSignAndBroadcastRepo) RecordBroadcastTxHash(_ context.Context, id, txHash string) error {
	f.recordCalls++
	f.gotRecordID, f.gotRecordTxHash = id, txHash
	return f.recordErr
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

func TestSignAndBroadcastWithdrawal_Execute_NothingToClaim(t *testing.T) {
	repo := &fakeSignAndBroadcastRepo{claimOK: false}
	broadcaster := &fakeBroadcaster{}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, broadcaster, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if claimed {
		t.Fatal("claimed = true, want false when nothing was approved to claim")
	}
	if repo.claimCalls != 1 {
		t.Fatalf("claimCalls = %d, want 1", repo.claimCalls)
	}
	if broadcaster.buildCalls != 0 {
		t.Fatal("BuildUnsignedWithdrawal must not be called when nothing was claimed")
	}
	if len(txBeginner.txs) != 1 {
		t.Fatalf("opened %d transactions, want exactly 1 (the claim attempt)", len(txBeginner.txs))
	}
	if txBeginner.txs[0].committed || !txBeginner.txs[0].rolledBack {
		t.Fatal("the claim transaction should roll back, never commit, when nothing was claimed")
	}
}

func TestSignAndBroadcastWithdrawal_Execute_ClaimError(t *testing.T) {
	repo := &fakeSignAndBroadcastRepo{claimErr: errors.New("db unavailable")}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, &fakeBroadcaster{}, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase)
	if err == nil {
		t.Fatal("err = nil, want a wrapped claim error")
	}
	if claimed {
		t.Fatal("claimed = true, want false on a claim error")
	}
	if len(txBeginner.txs) != 1 || !txBeginner.txs[0].rolledBack {
		t.Fatal("the claim transaction should roll back on a claim error")
	}
}

func TestSignAndBroadcastWithdrawal_Execute_HappyPath(t *testing.T) {
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

	claimed, err := uc.Execute(context.Background(), core.ChainBase)
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

	if broadcaster.sendCalls != 1 || broadcaster.gotSendChain != core.ChainBase || !bytes.Equal(broadcaster.gotSendTx, broadcaster.assembleSignedTx) {
		t.Fatal("SendRawTransaction was not called with the assembled signed tx bytes")
	}

	if repo.recordCalls != 1 || repo.gotRecordID != withdrawal.ID || repo.gotRecordTxHash != broadcaster.assembleTxHash {
		t.Fatalf("RecordBroadcastTxHash called with (%q, %q), want (%q, %q)", repo.gotRecordID, repo.gotRecordTxHash, withdrawal.ID, broadcaster.assembleTxHash)
	}

	if len(txBeginner.txs) != 2 {
		t.Fatalf("opened %d transactions, want exactly 2 (claim, then record-broadcast)", len(txBeginner.txs))
	}
	if !txBeginner.txs[0].committed || !txBeginner.txs[1].committed {
		t.Fatal("both the claim and the record-broadcast transactions should commit on the happy path")
	}
}

func TestSignAndBroadcastWithdrawal_Execute_BuildFails_ClaimStillCommitted(t *testing.T) {
	nonce := int64(1)
	withdrawal := core.Withdrawal{ID: "withdrawal-1", DestinationAddress: "0xabc", Amount: big.NewInt(1), Nonce: &nonce}
	repo := &fakeSignAndBroadcastRepo{claimResult: withdrawal, claimOK: true}
	broadcaster := &fakeBroadcaster{buildErr: errors.New("rpc endpoint unreachable")}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewSignAndBroadcastWithdrawal(repo, &fakeSigner{}, broadcaster, txBeginner)

	claimed, err := uc.Execute(context.Background(), core.ChainBase)
	if err == nil {
		t.Fatal("err = nil, want a wrapped build error")
	}
	// AD-11: the nonce + broadcast_attempts row is already durably committed by the time
	// BuildUnsignedWithdrawal is even called — a downstream failure here must never be
	// reported as "nothing was claimed."
	if !claimed {
		t.Fatal("claimed = false, want true — the withdrawal was durably claimed before the build failure")
	}
	if len(txBeginner.txs) != 1 {
		t.Fatalf("opened %d transactions, want exactly 1 (only the claim — a build failure never opens a record-broadcast transaction)", len(txBeginner.txs))
	}
	if !txBeginner.txs[0].committed {
		t.Fatal("the claim transaction should have committed BEFORE the build call ran")
	}
	if repo.recordCalls != 0 {
		t.Fatal("RecordBroadcastTxHash must not be called when BuildUnsignedWithdrawal fails")
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

	claimed, err := uc.Execute(context.Background(), core.ChainBase)
	if err == nil {
		t.Fatal("err = nil, want a wrapped sign error")
	}
	if !claimed {
		t.Fatal("claimed = false, want true")
	}
	if broadcaster.assembleCalls != 0 || broadcaster.sendCalls != 0 {
		t.Fatal("AssembleSignedTx/SendRawTransaction must not be called when Sign fails")
	}
	if repo.recordCalls != 0 {
		t.Fatal("RecordBroadcastTxHash must not be called when Sign fails")
	}
	if len(txBeginner.txs) != 1 {
		t.Fatalf("opened %d transactions, want exactly 1", len(txBeginner.txs))
	}
}

func TestSignAndBroadcastWithdrawal_Execute_SendFails_NoTxHashRecorded(t *testing.T) {
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

	claimed, err := uc.Execute(context.Background(), core.ChainBase)
	if err == nil {
		t.Fatal("err = nil, want a wrapped send error")
	}
	if !claimed {
		t.Fatal("claimed = false, want true")
	}
	if repo.recordCalls != 0 {
		t.Fatal("RecordBroadcastTxHash must not be called when SendRawTransaction fails — I/O matrix: the withdrawal stays 'signed' with no tx_hash")
	}
	if len(txBeginner.txs) != 1 {
		t.Fatalf("opened %d transactions, want exactly 1 (no record-broadcast transaction on a send failure)", len(txBeginner.txs))
	}
}
