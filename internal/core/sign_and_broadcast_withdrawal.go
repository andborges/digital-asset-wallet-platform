package core

import (
	"context"
	"fmt"
)

// SignAndBroadcastWithdrawal claims exactly one approved withdrawal on chain and advances
// it through Story 3.4's build -> sign -> assemble -> persist -> send pipeline in one
// Execute call (Code Map): cmd/walletd's broadcaster subcommand calls Execute repeatedly
// per tick until it reports nothing left to claim, so a backlog of approved withdrawals
// drains within one tick rather than one withdrawal per tick.
//
// AD-11's own wording governs the split below: the nonce allocation and the
// broadcast_attempts row insert (both inside WithdrawalRepository.ClaimApprovedWithdrawal)
// commit in their OWN transaction, opened and committed by this Execute call, BEFORE any
// Signer/TransactionBroadcaster call happens — so a crash, or a signer/broadcast failure,
// after that commit leaves the withdrawal at WithdrawalStatusSigned with no tx_hash: a
// well-defined, resumable state (Story 3.5's job to resume, not this one's).
type SignAndBroadcastWithdrawal struct {
	repo        WithdrawalRepository
	signer      Signer
	broadcaster TransactionBroadcaster
	txBeginner  TxBeginner
}

// NewSignAndBroadcastWithdrawal constructs the use case against the given ports.
func NewSignAndBroadcastWithdrawal(repo WithdrawalRepository, signer Signer, broadcaster TransactionBroadcaster, txBeginner TxBeginner) *SignAndBroadcastWithdrawal {
	return &SignAndBroadcastWithdrawal{repo: repo, signer: signer, broadcaster: broadcaster, txBeginner: txBeginner}
}

// Execute claims one approved withdrawal on chain (returning claimed=false, err=nil if none
// exists — an ordinary "nothing to do" outcome, never an error) and drives it through
// sign/broadcast. A failure anywhere from BuildUnsignedWithdrawal onward is returned as an
// error but is NOT a claim failure: claimed is still true, since the withdrawal really was
// claimed — and durably left at WithdrawalStatusSigned, per the doc comment above — even
// though this call could not finish broadcasting it. The caller (cmd/walletd's broadcaster
// subcommand) logs such an error and moves on; this is never a fatal process crash (I/O &
// Edge-Case Matrix: "Signer returns an error... logged server-side, never a fatal process
// crash").
func (uc *SignAndBroadcastWithdrawal) Execute(ctx context.Context, chain Chain) (claimed bool, err error) {
	txCtx, tx, err := uc.txBeginner.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin claim transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(txCtx))
		}
	}()

	withdrawal, ok, err := uc.repo.ClaimApprovedWithdrawal(txCtx, chain)
	if err != nil {
		return false, fmt.Errorf("claim approved withdrawal: %w", err)
	}
	if !ok {
		return false, nil
	}
	if withdrawal.Nonce == nil {
		// Defensive, not expected validation: ClaimApprovedWithdrawal's own contract
		// always populates Nonce for a successfully claimed withdrawal (ok == true) — a
		// future repository bug returning ok=true with no nonce must fail loud here,
		// never panic later dereferencing a nil pointer.
		return false, fmt.Errorf("claimed withdrawal %s with no nonce allocated", withdrawal.ID)
	}

	if err := tx.Commit(context.WithoutCancel(txCtx)); err != nil {
		return false, fmt.Errorf("commit claim transaction: %w", err)
	}
	committed = true

	// From here on, the nonce + broadcast_attempts row is already durably committed
	// (AD-11) — every remaining step is best-effort against the signer and the chain; any
	// failure below leaves this withdrawal at WithdrawalStatusSigned with no tx_hash,
	// Story 3.5's territory to resume, never this use case's.
	digest, unsignedTx, err := uc.broadcaster.BuildUnsignedWithdrawal(ctx, chain, withdrawal.Asset, *withdrawal.Nonce, withdrawal.DestinationAddress, withdrawal.Amount)
	if err != nil {
		return true, fmt.Errorf("build unsigned withdrawal %s transaction: %w", withdrawal.ID, err)
	}

	// NFR13: the signature returned here is opaque bytes — no key handle or private key
	// material ever crosses this call in either direction, so nothing below (including
	// every error wrap in this function) can ever leak key material into a log line.
	signature, err := uc.signer.Sign(ctx, chain, digest)
	if err != nil {
		return true, fmt.Errorf("sign withdrawal %s: %w", withdrawal.ID, err)
	}

	signedTx, txHash, err := uc.broadcaster.AssembleSignedTx(unsignedTx, signature)
	if err != nil {
		return true, fmt.Errorf("assemble signed withdrawal %s transaction: %w", withdrawal.ID, err)
	}

	if err := uc.broadcaster.SendRawTransaction(ctx, chain, signedTx); err != nil {
		return true, fmt.Errorf("broadcast withdrawal %s transaction: %w", withdrawal.ID, err)
	}

	recordCtx, recordTx, err := uc.txBeginner.Begin(ctx)
	if err != nil {
		return true, fmt.Errorf("begin record-broadcast transaction for withdrawal %s: %w", withdrawal.ID, err)
	}
	recordCommitted := false
	defer func() {
		if !recordCommitted {
			_ = recordTx.Rollback(context.WithoutCancel(recordCtx))
		}
	}()
	if err := uc.repo.RecordBroadcastTxHash(recordCtx, withdrawal.ID, txHash); err != nil {
		return true, fmt.Errorf("record broadcast tx hash for withdrawal %s: %w", withdrawal.ID, err)
	}
	if err := recordTx.Commit(context.WithoutCancel(recordCtx)); err != nil {
		return true, fmt.Errorf("commit record-broadcast transaction for withdrawal %s: %w", withdrawal.ID, err)
	}
	recordCommitted = true

	return true, nil
}
