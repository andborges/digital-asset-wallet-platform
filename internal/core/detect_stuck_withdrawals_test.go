package core_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

func TestDetectStuckWithdrawals_Execute_NoCandidates(t *testing.T) {
	repo := &fakeSignAndBroadcastRepo{listStuckResult: nil}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewDetectStuckWithdrawals(repo, txBeginner)

	result, err := uc.Execute(context.Background(), core.ChainBase, 30*time.Minute)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if result.Checked != 0 || result.Alerted != 0 {
		t.Fatalf("result = %+v, want all zero", result)
	}
	if len(repo.markStuckCalls) != 0 {
		t.Fatal("MarkStuckAlerted must not be called when there are no candidates")
	}
	// Only the list transaction — a plain read, rolled back, never committed.
	if len(txBeginner.txs) != 1 || txBeginner.txs[0].committed || !txBeginner.txs[0].rolledBack {
		t.Fatal("the list transaction is a plain read — it should roll back, never commit")
	}
}

func TestDetectStuckWithdrawals_Execute_PassesThresholdThrough(t *testing.T) {
	repo := &fakeSignAndBroadcastRepo{}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewDetectStuckWithdrawals(repo, txBeginner)

	threshold := 45 * time.Minute
	if _, err := uc.Execute(context.Background(), core.ChainBase, threshold); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if repo.gotListStuckOlderThan != threshold {
		t.Fatalf("ListStuckCandidates olderThan = %v, want %v", repo.gotListStuckOlderThan, threshold)
	}
}

func TestDetectStuckWithdrawals_Execute_AlertsEachCandidateExactlyOnce(t *testing.T) {
	w1 := core.Withdrawal{ID: "withdrawal-1"}
	w2 := core.Withdrawal{ID: "withdrawal-2"}
	repo := &fakeSignAndBroadcastRepo{listStuckResult: []core.Withdrawal{w1, w2}}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewDetectStuckWithdrawals(repo, txBeginner)

	result, err := uc.Execute(context.Background(), core.ChainBase, 30*time.Minute)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if result.Checked != 2 || result.Alerted != 2 {
		t.Fatalf("result = %+v, want Checked=2, Alerted=2", result)
	}
	if len(repo.markStuckCalls) != 2 || repo.markStuckCalls[0] != w1.ID || repo.markStuckCalls[1] != w2.ID {
		t.Fatalf("markStuckCalls = %v, want [%q, %q]", repo.markStuckCalls, w1.ID, w2.ID)
	}
	// [0] list (rolled back), [1] and [2] one mark-alerted transaction per candidate, each
	// committed.
	if len(txBeginner.txs) != 3 {
		t.Fatalf("opened %d transactions, want exactly 3 (list, then one per candidate)", len(txBeginner.txs))
	}
	if !txBeginner.txs[1].committed || !txBeginner.txs[2].committed {
		t.Fatal("both mark-alerted transactions should commit")
	}
}

// TestDetectStuckWithdrawals_Execute_OneFailureDoesNotBlockTheRest mirrors
// PollWithdrawalReceipts' identical batch-resilience discipline: one candidate's
// MarkStuckAlerted failure must not prevent every other candidate in the same poll cycle
// from still being alerted.
func TestDetectStuckWithdrawals_Execute_OneFailureDoesNotBlockTheRest(t *testing.T) {
	w1 := core.Withdrawal{ID: "withdrawal-1"}
	w2 := core.Withdrawal{ID: "withdrawal-2"}
	repo := &fakeSignAndBroadcastRepo{
		listStuckResult:  []core.Withdrawal{w1, w2},
		markStuckErrByID: map[string]error{w1.ID: errors.New("outbox insert failed")},
	}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewDetectStuckWithdrawals(repo, txBeginner)

	result, err := uc.Execute(context.Background(), core.ChainBase, 30*time.Minute)
	if err == nil {
		t.Fatal("err = nil, want a non-nil aggregated error (withdrawal-1's alert failed)")
	}
	if result.Checked != 2 {
		t.Fatalf("Checked = %d, want 2 (both candidates were attempted)", result.Checked)
	}
	if result.Alerted != 1 {
		t.Fatalf("Alerted = %d, want 1 (withdrawal-2 still alerted despite withdrawal-1's failure)", result.Alerted)
	}
	if len(repo.markStuckCalls) != 2 {
		t.Fatalf("markStuckCalls = %v, want both candidates attempted", repo.markStuckCalls)
	}
}

func TestDetectStuckWithdrawals_Execute_ListError(t *testing.T) {
	repo := &fakeSignAndBroadcastRepo{listStuckErr: errors.New("db unavailable")}
	txBeginner := &recordingTxBeginner{}
	uc := core.NewDetectStuckWithdrawals(repo, txBeginner)

	_, err := uc.Execute(context.Background(), core.ChainBase, 30*time.Minute)
	if err == nil {
		t.Fatal("err = nil, want a wrapped list error")
	}
	if len(repo.markStuckCalls) != 0 {
		t.Fatal("MarkStuckAlerted must not be called when listing candidates itself failed")
	}
}
