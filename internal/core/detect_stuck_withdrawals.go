package core

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DetectStuckWithdrawalsResult summarizes one DetectStuckWithdrawals.Execute poll cycle —
// operator-facing logging only (cmd/walletd/main.go), never a control-flow decision,
// mirroring PollWithdrawalReceiptsResult's own role.
type DetectStuckWithdrawalsResult struct {
	Checked, Alerted int
}

// DetectStuckWithdrawals writes a one-time "withdrawal.stuck" outbox event for every
// withdrawal on chain that has spent too long at EITHER WithdrawalStatusSigned (unable to
// get a broadcast attempt to succeed) or WithdrawalStatusBroadcast (broadcast, but not yet
// confirmed) — WithdrawalRepository.ListStuckCandidates' own contract, widened by re-review
// 2026-07-22 to cover both statuses (the original version only ever watched
// WithdrawalStatusBroadcast, leaving a withdrawal stranded at WithdrawalStatusSigned with
// zero monitoring coverage — both an adversarial and an edge-case review pass independently
// caught this). A monitoring signal layered on the existing status, never a new terminal
// state and never itself a status transition (Design Notes): the withdrawal can still
// resolve normally afterward (to broadcast then confirmed/failed, or straight to
// confirmed/failed), independent of this use case.
type DetectStuckWithdrawals struct {
	repo       WithdrawalRepository
	txBeginner TxBeginner
}

// NewDetectStuckWithdrawals constructs the use case against the given ports.
func NewDetectStuckWithdrawals(repo WithdrawalRepository, txBeginner TxBeginner) *DetectStuckWithdrawals {
	return &DetectStuckWithdrawals{repo: repo, txBeginner: txBeginner}
}

// Execute lists every stuck candidate on chain (WithdrawalStatusBroadcast, broadcast longer
// than olderThan ago, never yet alerted — WithdrawalRepository.ListStuckCandidates' own
// contract) and alerts each one exactly once. A single withdrawal's alert failure is
// collected via errors.Join and does NOT stop the rest of the batch from being alerted —
// mirroring PollWithdrawalReceipts.Execute's identical "one failure doesn't block the rest"
// discipline. The caller (cmd/walletd's broadcaster subcommand) logs any returned error and
// continues to the next tick; a withdrawal whose alert failed this cycle simply stays
// un-alerted (stuck_alerted_at still NULL) and is retried next poll.
func (uc *DetectStuckWithdrawals) Execute(ctx context.Context, chain Chain, olderThan time.Duration) (DetectStuckWithdrawalsResult, error) {
	listCtx, listTx, err := uc.txBeginner.Begin(ctx)
	if err != nil {
		return DetectStuckWithdrawalsResult{}, fmt.Errorf("begin list transaction: %w", err)
	}
	candidates, err := uc.repo.ListStuckCandidates(listCtx, chain, olderThan)
	// A plain read — nothing to commit either way, mirroring PollWithdrawalReceipts.Execute's
	// identical list-then-rollback shape.
	_ = listTx.Rollback(context.WithoutCancel(listCtx))
	if err != nil {
		return DetectStuckWithdrawalsResult{}, fmt.Errorf("list stuck candidates: %w", err)
	}

	var result DetectStuckWithdrawalsResult
	var errs []error
	for _, w := range candidates {
		result.Checked++
		if err := uc.markAlerted(ctx, w.ID); err != nil {
			errs = append(errs, fmt.Errorf("mark withdrawal %s stuck-alerted: %w", w.ID, err))
			continue
		}
		result.Alerted++
	}

	return result, errors.Join(errs...)
}

// markAlerted runs MarkStuckAlerted inside its own transaction, one per withdrawal — a
// failure never leaves a partial write behind (the transaction rolls back) and never affects
// any other withdrawal's own alert this poll cycle, mirroring PollWithdrawalReceipts.settle's
// identical per-item transaction shape.
func (uc *DetectStuckWithdrawals) markAlerted(ctx context.Context, withdrawalID string) error {
	txCtx, tx, err := uc.txBeginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin mark-alerted transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(txCtx))
		}
	}()
	if err := uc.repo.MarkStuckAlerted(txCtx, withdrawalID); err != nil {
		return err
	}
	if err := tx.Commit(context.WithoutCancel(txCtx)); err != nil {
		return fmt.Errorf("commit mark-alerted transaction: %w", err)
	}
	committed = true
	return nil
}
