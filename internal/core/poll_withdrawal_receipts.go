package core

import (
	"context"
	"errors"
	"fmt"
)

// PollWithdrawalReceiptsResult summarizes one PollWithdrawalReceipts.Execute poll cycle —
// operator-facing logging only (cmd/walletd/main.go), never a control-flow decision,
// mirroring TrackDeposits.ExecuteResult's own role.
type PollWithdrawalReceiptsResult struct {
	Checked, Confirmed, Failed int
}

// PollWithdrawalReceipts checks every WithdrawalStatusBroadcast withdrawal on chain against
// the chain's finalized tag (AD-7's identical tag choice for deposit crediting) and settles
// each one that has reached a receipt there: a successful receipt settles debit-hold/
// credit-treasury (WithdrawalStatusConfirmed); a reverted receipt settles debit-hold/
// credit-available (WithdrawalStatusFailed) — mirroring the Design Notes' postings-must-
// net-to-zero reasoning. A withdrawal with no receipt yet, or a receipt not yet at the
// finalized tag, is left untouched for the next poll cycle.
type PollWithdrawalReceipts struct {
	repo        WithdrawalRepository
	broadcaster TransactionBroadcaster
	txBeginner  TxBeginner
}

// NewPollWithdrawalReceipts constructs the use case against the given ports.
func NewPollWithdrawalReceipts(repo WithdrawalRepository, broadcaster TransactionBroadcaster, txBeginner TxBeginner) *PollWithdrawalReceipts {
	return &PollWithdrawalReceipts{repo: repo, broadcaster: broadcaster, txBeginner: txBeginner}
}

// Execute checks and settles every WithdrawalStatusBroadcast withdrawal on chain. A single
// withdrawal's receipt-check or settlement failure (a transient RPC hiccup, or a
// registry-gap "no treasury account" error, I/O & Edge-Case Matrix) is collected via
// errors.Join and does NOT stop the rest of the batch from being checked — every other
// withdrawal in this poll cycle still gets its own chance to settle. The caller
// (cmd/walletd's broadcaster subcommand) logs any returned error and continues to the next
// tick; a withdrawal whose settlement failed this cycle simply stays at
// WithdrawalStatusBroadcast and is retried next poll (I/O & Edge-Case Matrix: "fail loud,
// logged server-side; withdrawal stays broadcast for retry next poll").
func (uc *PollWithdrawalReceipts) Execute(ctx context.Context, chain Chain) (PollWithdrawalReceiptsResult, error) {
	listCtx, listTx, err := uc.txBeginner.Begin(ctx)
	if err != nil {
		return PollWithdrawalReceiptsResult{}, fmt.Errorf("begin list transaction: %w", err)
	}
	withdrawals, err := uc.repo.ListBroadcastWithdrawals(listCtx, chain)
	// A plain read — nothing to commit either way, mirroring how cmd/walletd's own
	// startup cursor-read block rolls back its short-lived read-only transaction.
	_ = listTx.Rollback(context.WithoutCancel(listCtx))
	if err != nil {
		return PollWithdrawalReceiptsResult{}, fmt.Errorf("list broadcast withdrawals: %w", err)
	}

	var result PollWithdrawalReceiptsResult
	var errs []error
	for _, w := range withdrawals {
		result.Checked++

		found, success, err := uc.broadcaster.GetFinalizedReceipt(ctx, chain, w.TxHash)
		if err != nil {
			errs = append(errs, fmt.Errorf("get finalized receipt for withdrawal %s (tx %s): %w", w.ID, w.TxHash, err))
			continue
		}
		if !found {
			continue
		}

		if success {
			if err := uc.settle(ctx, w.ID, uc.repo.SettleConfirmedWithdrawal); err != nil {
				errs = append(errs, fmt.Errorf("settle confirmed withdrawal %s: %w", w.ID, err))
				continue
			}
			result.Confirmed++
		} else {
			if err := uc.settle(ctx, w.ID, uc.repo.SettleFailedWithdrawal); err != nil {
				errs = append(errs, fmt.Errorf("settle failed withdrawal %s: %w", w.ID, err))
				continue
			}
			result.Failed++
		}
	}

	return result, errors.Join(errs...)
}

// settle runs settleFn (either SettleConfirmedWithdrawal or SettleFailedWithdrawal) inside
// its own transaction, one per withdrawal — a failure never leaves a partial write behind
// (the transaction rolls back) and never affects any other withdrawal's own settlement
// this poll cycle.
func (uc *PollWithdrawalReceipts) settle(ctx context.Context, withdrawalID string, settleFn func(context.Context, string) error) error {
	txCtx, tx, err := uc.txBeginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin settlement transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(txCtx))
		}
	}()
	if err := settleFn(txCtx, withdrawalID); err != nil {
		return err
	}
	if err := tx.Commit(context.WithoutCancel(txCtx)); err != nil {
		return fmt.Errorf("commit settlement transaction: %w", err)
	}
	committed = true
	return nil
}
