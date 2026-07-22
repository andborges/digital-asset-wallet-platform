package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

func TestPollWithdrawalReceipts_Execute_NoBroadcastWithdrawals(t *testing.T) {
	repo := &fakeSignAndBroadcastRepo{listResult: nil}
	broadcaster := &fakeBroadcaster{}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewPollWithdrawalReceipts(repo, broadcaster, txBeginner)

	result, err := uc.Execute(context.Background(), core.ChainBase)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if result.Checked != 0 || result.Confirmed != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v, want all zero", result)
	}
	if broadcaster.receiptCalls != 0 {
		t.Fatal("GetFinalizedReceipt must not be called when there is nothing to check")
	}
	if len(txBeginner.txs) != 1 || txBeginner.txs[0].committed || !txBeginner.txs[0].rolledBack {
		t.Fatal("the list transaction is a plain read — it should roll back, never commit")
	}
}

func TestPollWithdrawalReceipts_Execute_ReceiptNotYetFound_LeavesWithdrawalUntouched(t *testing.T) {
	w := core.Withdrawal{ID: "withdrawal-1", TxHash: "0xabc"}
	repo := &fakeSignAndBroadcastRepo{listResult: []core.Withdrawal{w}}
	broadcaster := &fakeBroadcaster{receiptFound: false}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewPollWithdrawalReceipts(repo, broadcaster, txBeginner)

	result, err := uc.Execute(context.Background(), core.ChainBase)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if result.Checked != 1 || result.Confirmed != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v, want Checked=1, Confirmed=0, Failed=0", result)
	}
	if len(repo.settleConfirmedCalls) != 0 || len(repo.settleFailedCalls) != 0 {
		t.Fatal("no settlement should be attempted when found=false")
	}
	// Only the list transaction — no settlement transaction opened at all.
	if len(txBeginner.txs) != 1 {
		t.Fatalf("opened %d transactions, want exactly 1", len(txBeginner.txs))
	}
}

func TestPollWithdrawalReceipts_Execute_SuccessfulReceipt_SettlesConfirmed(t *testing.T) {
	w := core.Withdrawal{ID: "withdrawal-1", TxHash: "0xabc"}
	repo := &fakeSignAndBroadcastRepo{listResult: []core.Withdrawal{w}}
	broadcaster := &fakeBroadcaster{receiptFound: true, receiptSuccess: true}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewPollWithdrawalReceipts(repo, broadcaster, txBeginner)

	result, err := uc.Execute(context.Background(), core.ChainBase)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if result.Checked != 1 || result.Confirmed != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v, want Checked=1, Confirmed=1, Failed=0", result)
	}
	if len(repo.settleConfirmedCalls) != 1 || repo.settleConfirmedCalls[0] != w.ID {
		t.Fatalf("settleConfirmedCalls = %v, want [%q]", repo.settleConfirmedCalls, w.ID)
	}
	if len(repo.settleFailedCalls) != 0 {
		t.Fatal("SettleFailedWithdrawal must not be called for a successful receipt")
	}
	if len(txBeginner.txs) != 2 {
		t.Fatalf("opened %d transactions, want exactly 2 (list, then settle)", len(txBeginner.txs))
	}
	if !txBeginner.txs[1].committed {
		t.Fatal("the settlement transaction should commit")
	}
}

func TestPollWithdrawalReceipts_Execute_RevertedReceipt_SettlesFailed(t *testing.T) {
	w := core.Withdrawal{ID: "withdrawal-1", TxHash: "0xabc"}
	repo := &fakeSignAndBroadcastRepo{listResult: []core.Withdrawal{w}}
	broadcaster := &fakeBroadcaster{receiptFound: true, receiptSuccess: false}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewPollWithdrawalReceipts(repo, broadcaster, txBeginner)

	result, err := uc.Execute(context.Background(), core.ChainBase)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if result.Checked != 1 || result.Confirmed != 0 || result.Failed != 1 {
		t.Fatalf("result = %+v, want Checked=1, Confirmed=0, Failed=1", result)
	}
	if len(repo.settleFailedCalls) != 1 || repo.settleFailedCalls[0] != w.ID {
		t.Fatalf("settleFailedCalls = %v, want [%q]", repo.settleFailedCalls, w.ID)
	}
	if len(repo.settleConfirmedCalls) != 0 {
		t.Fatal("SettleConfirmedWithdrawal must not be called for a reverted receipt")
	}
}

// TestPollWithdrawalReceipts_Execute_OneFailureDoesNotBlockTheRest proves the batch keeps
// going past a single withdrawal's receipt-check error — a transient RPC hiccup on one
// withdrawal must not prevent every other withdrawal in the same poll cycle from being
// checked and settled.
func TestPollWithdrawalReceipts_Execute_OneFailureDoesNotBlockTheRest(t *testing.T) {
	w1 := core.Withdrawal{ID: "withdrawal-1", TxHash: "0xaaa"}
	w2 := core.Withdrawal{ID: "withdrawal-2", TxHash: "0xbbb"}
	repo := &fakeSignAndBroadcastRepo{listResult: []core.Withdrawal{w1, w2}}
	broadcaster := &fakeBroadcaster{
		receiptsByTxHash: map[string]receiptFixture{
			"0xaaa": {err: errors.New("rpc timeout")},
			"0xbbb": {found: true, success: true},
		},
	}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewPollWithdrawalReceipts(repo, broadcaster, txBeginner)

	result, err := uc.Execute(context.Background(), core.ChainBase)
	if err == nil {
		t.Fatal("err = nil, want a non-nil aggregated error (withdrawal-1's receipt check failed)")
	}
	if result.Checked != 2 {
		t.Fatalf("Checked = %d, want 2 (both withdrawals were attempted)", result.Checked)
	}
	if result.Confirmed != 1 {
		t.Fatalf("Confirmed = %d, want 1 (withdrawal-2 still settled despite withdrawal-1's failure)", result.Confirmed)
	}
	if len(repo.settleConfirmedCalls) != 1 || repo.settleConfirmedCalls[0] != w2.ID {
		t.Fatalf("settleConfirmedCalls = %v, want [%q]", repo.settleConfirmedCalls, w2.ID)
	}
}

// TestPollWithdrawalReceipts_Execute_SettlementFailure_LeavesWithdrawalForRetry mirrors the
// I/O & Edge-Case Matrix's "no treasury account" registry-gap scenario: a settlement call
// itself failing (rather than the receipt check) must still be reported as an error, and
// must not count toward Confirmed/Failed — the withdrawal simply stays broadcast for the
// next poll to retry (I/O & Edge-Case Matrix).
func TestPollWithdrawalReceipts_Execute_SettlementFailure_LeavesWithdrawalForRetry(t *testing.T) {
	w := core.Withdrawal{ID: "withdrawal-1", TxHash: "0xabc"}
	repo := &fakeSignAndBroadcastRepo{
		listResult:         []core.Withdrawal{w},
		settleConfirmedErr: errors.New("no platform treasury account configured for this chain/asset"),
	}
	broadcaster := &fakeBroadcaster{receiptFound: true, receiptSuccess: true}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewPollWithdrawalReceipts(repo, broadcaster, txBeginner)

	result, err := uc.Execute(context.Background(), core.ChainBase)
	if err == nil {
		t.Fatal("err = nil, want a non-nil error from the failed settlement")
	}
	if result.Checked != 1 || result.Confirmed != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v, want Checked=1, Confirmed=0, Failed=0 (settlement failed, so it does not count)", result)
	}
	// list tx (rolled back) + the attempted (and rolled-back) settle tx.
	if len(txBeginner.txs) != 2 {
		t.Fatalf("opened %d transactions, want exactly 2", len(txBeginner.txs))
	}
	if txBeginner.txs[1].committed {
		t.Fatal("the settlement transaction must not commit when SettleConfirmedWithdrawal fails")
	}
	if !txBeginner.txs[1].rolledBack {
		t.Fatal("the settlement transaction should roll back when SettleConfirmedWithdrawal fails")
	}
}
