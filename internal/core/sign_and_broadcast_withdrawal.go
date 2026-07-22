package core

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
)

// SignAndBroadcastWithdrawal claims/resumes exactly one withdrawal on chain and advances it
// through Story 3.4's build -> sign -> assemble -> persist -> send pipeline, restructured by
// Story 3.5 so the signed bytes are persisted BEFORE the send is ever attempted (Boundaries
// & Constraints): cmd/walletd's broadcaster subcommand calls Execute repeatedly per tick
// until it reports nothing left to claim/resume, so a backlog drains within one tick rather
// than one withdrawal per tick.
//
// AD-11's own wording governs the split below: the nonce allocation and the
// broadcast_attempts row insert (both inside WithdrawalRepository.ClaimApprovedWithdrawal)
// commit in their OWN transaction, opened and committed by this Execute call, BEFORE any
// Signer/TransactionBroadcaster call happens — so a crash, or a signer/broadcast failure,
// after that commit leaves the withdrawal at WithdrawalStatusSigned. Story 3.5 adds a second
// such durability point: once signed, the exact signed bytes are persisted (RecordSignedTx)
// BEFORE the network send is attempted, so a crash/failure from THERE on is resumed by
// re-sending those exact bytes, never re-signing (Design Notes: AWS KMS's ECDSA signing is
// not guaranteed deterministic, so a second signature over the same digest could legitimately
// differ from one that already reached the network).
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

// Execute resumes one already-signed withdrawal if any exists on chain, or otherwise — only
// when allowClaim is true — claims one fresh approved withdrawal (returning claimed=false,
// err=nil if neither exists/applies — an ordinary "nothing to do" outcome, never an error),
// and drives it through sign/broadcast.
//
// allowClaim (re-review 2026-07-22, both an adversarial and an edge-case review pass
// independently flagged the original version's absence of this distinction): the
// broadcaster's own liveness gate (cmd/walletd's runBroadcaster, AD-15) exists specifically
// to stop ALLOCATING NEW NONCES while the chain's watcher cursor is stale — it has no
// bearing on resuming a withdrawal that already has one. Resuming re-sends
// already-persisted, already-nonce-committed bytes; it strands nothing new regardless of
// liveness. Passing allowClaim=false therefore still resumes normally, and only skips the
// ClaimApprovedWithdrawal fallback when nothing is left to resume.
//
// Resuming ALWAYS takes priority over claiming new work (Boundaries & Constraints) when
// allowClaim is true: a withdrawal already at WithdrawalStatusSigned — whether freshly
// claimed this very call, or left there by a prior crash/interruption — must be finished
// (signed if not yet, sent if not yet successfully sent) before a new nonce is ever
// allocated for a different withdrawal. Within that resumed set, one whose SignedTx is
// already non-empty (Story 3.5's own persist-before-send bytes) skips straight to
// re-sending those exact bytes, never re-signing; one with no SignedTx yet (claimed but
// interrupted before signing completed, or freshly claimed by this very call) runs the full
// build/sign/assemble/persist/send pipeline.
//
// A failure anywhere in that pipeline is returned as an error but is NOT a claim failure:
// claimed is still true, since the withdrawal really was claimed/resumed — and durably left
// at WithdrawalStatusSigned (with its signed_tx already persisted, once signing has
// succeeded once) — even though this call could not finish broadcasting it. The caller
// (cmd/walletd's broadcaster subcommand) logs such an error and moves on; this is never a
// fatal process crash.
func (uc *SignAndBroadcastWithdrawal) Execute(ctx context.Context, chain Chain, allowClaim bool) (claimed bool, err error) {
	// Resume takes priority over claiming new work (Boundaries & Constraints) — checked via
	// its own short-lived read-only transaction, mirroring PollWithdrawalReceipts.Execute's
	// identical "list, then roll back, nothing to commit" shape.
	listCtx, listTx, err := uc.txBeginner.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin list-signed transaction: %w", err)
	}
	signed, listErr := uc.repo.ListSignedWithdrawals(listCtx, chain)
	_ = listTx.Rollback(context.WithoutCancel(listCtx))
	if listErr != nil {
		return false, fmt.Errorf("list signed withdrawals: %w", listErr)
	}

	if len(signed) > 0 {
		withdrawal := signed[0]
		if withdrawal.Nonce == nil {
			// Defensive, not expected validation: ListSignedWithdrawals' own contract always
			// populates Nonce from the withdrawal's own broadcast_attempts row — a future
			// repository bug returning none must fail loud here, never panic later
			// dereferencing a nil pointer.
			return false, fmt.Errorf("listed signed withdrawal %s with no nonce allocated", withdrawal.ID)
		}
		if len(withdrawal.SignedTx) > 0 {
			return true, uc.sendAndFinalize(ctx, chain, withdrawal)
		}
		return true, uc.signAndSend(ctx, chain, withdrawal)
	}

	// Nothing to resume. Claiming allocates a brand-new nonce — the one thing the liveness
	// gate exists to prevent while the chain's watcher cursor is stale (see allowClaim's own
	// doc comment above) — so respect it here, after resume has already had its chance.
	if !allowClaim {
		return false, nil
	}

	// Claim one fresh approved withdrawal (Story 3.4's own claim step, unchanged): the nonce
	// allocation and broadcast_attempts row insert commit in this own transaction BEFORE any
	// Signer/TransactionBroadcaster call happens.
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
	// (AD-11); a freshly claimed withdrawal never has a SignedTx yet, so it always takes the
	// full build/sign/assemble/persist/send path below.
	return true, uc.signAndSend(ctx, chain, withdrawal)
}

// signAndSend runs the full build -> sign -> assemble -> persist -> send pipeline for a
// withdrawal with no SignedTx yet (either freshly claimed this call, or claimed by a prior
// call but interrupted before signing completed). Every failure from BuildUnsignedWithdrawal
// onward leaves the withdrawal at WithdrawalStatusSigned for the next poll to resume — never
// this call's concern to recover from.
func (uc *SignAndBroadcastWithdrawal) signAndSend(ctx context.Context, chain Chain, withdrawal Withdrawal) error {
	digest, unsignedTx, err := uc.broadcaster.BuildUnsignedWithdrawal(ctx, chain, withdrawal.Asset, *withdrawal.Nonce, withdrawal.DestinationAddress, withdrawal.Amount)
	if err != nil {
		return fmt.Errorf("build unsigned withdrawal %s transaction: %w", withdrawal.ID, err)
	}

	// NFR13: the signature returned here is opaque bytes — no key handle or private key
	// material ever crosses this call in either direction, so nothing below (including
	// every error wrap in this function) can ever leak key material into a log line.
	signature, err := uc.signer.Sign(ctx, chain, digest)
	if err != nil {
		return fmt.Errorf("sign withdrawal %s: %w", withdrawal.ID, err)
	}

	signedTx, txHash, err := uc.broadcaster.AssembleSignedTx(unsignedTx, signature)
	if err != nil {
		return fmt.Errorf("assemble signed withdrawal %s transaction: %w", withdrawal.ID, err)
	}

	// Story 3.5's core restructuring: the exact signed bytes are persisted BEFORE the send is
	// ever attempted (Boundaries & Constraints) — this is what makes resuming after ANY
	// interruption from here on safe: the next poll cycle's ListSignedWithdrawals call finds
	// this exact byte sequence already durable and re-sends it verbatim, never re-signing.
	recordCtx, recordTx, err := uc.txBeginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin record-signed-tx transaction for withdrawal %s: %w", withdrawal.ID, err)
	}
	recordCommitted := false
	defer func() {
		if !recordCommitted {
			_ = recordTx.Rollback(context.WithoutCancel(recordCtx))
		}
	}()
	if err := uc.repo.RecordSignedTx(recordCtx, withdrawal.ID, txHash, hex.EncodeToString(signedTx)); err != nil {
		return fmt.Errorf("record signed tx for withdrawal %s: %w", withdrawal.ID, err)
	}
	if err := recordTx.Commit(context.WithoutCancel(recordCtx)); err != nil {
		return fmt.Errorf("commit record-signed-tx transaction for withdrawal %s: %w", withdrawal.ID, err)
	}
	recordCommitted = true

	withdrawal.SignedTx = signedTx
	withdrawal.TxHash = txHash
	return uc.sendAndFinalize(ctx, chain, withdrawal)
}

// sendAndFinalize sends withdrawal's already-persisted SignedTx bytes (Boundaries &
// Constraints: NEVER re-signs — signAndSend above is the only path that ever produces new
// signed bytes) and, on success OR a send error recognized as "already known"/"nonce too
// low" (isAlreadyKnownError), transitions the withdrawal to WithdrawalStatusBroadcast via
// MarkBroadcast. Any OTHER send error returns without changing status, leaving the
// withdrawal at WithdrawalStatusSigned (with its signed_tx already persisted) for the next
// poll to retry (I/O & Edge-Case Matrix).
func (uc *SignAndBroadcastWithdrawal) sendAndFinalize(ctx context.Context, chain Chain, withdrawal Withdrawal) error {
	sendErr := uc.broadcaster.SendRawTransaction(ctx, chain, withdrawal.SignedTx)
	if sendErr != nil && !isAlreadyKnownError(sendErr) {
		return fmt.Errorf("broadcast withdrawal %s transaction: %w", withdrawal.ID, sendErr)
	}

	txCtx, tx, err := uc.txBeginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin mark-broadcast transaction for withdrawal %s: %w", withdrawal.ID, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(txCtx))
		}
	}()
	if err := uc.repo.MarkBroadcast(txCtx, withdrawal.ID); err != nil {
		return fmt.Errorf("mark withdrawal %s broadcast: %w", withdrawal.ID, err)
	}
	if err := tx.Commit(context.WithoutCancel(txCtx)); err != nil {
		return fmt.Errorf("commit mark-broadcast transaction for withdrawal %s: %w", withdrawal.ID, err)
	}
	committed = true
	return nil
}

// isAlreadyKnownError reports whether err's text (case-insensitively, substring match)
// matches "already known" or "nonce too low" — the two phrasings different go-ethereum-
// family node implementations (op-geth for Base, Arbitrum Nitro) commonly use to mean "a
// transaction at this nonce already exists in the mempool or has already landed on-chain."
// Under AD-11's single-writer guarantee, exactly one process ever sends from the hot wallet
// and nonces are allocated strictly sequentially inside one Postgres transaction per
// withdrawal (Design Notes) — so the only transaction that could ever already occupy this
// exact nonce is THIS withdrawal's own (possibly already-sent) prior attempt. Treating
// either error text as success on resend is therefore safe: either the node genuinely still
// needs it (and accepts it), or the original already landed (and the node is telling us so)
// — both outcomes correctly converge on WithdrawalStatusBroadcast.
//
// Deliberately narrow: this is NOT a general "classify terminal vs. transient send errors"
// facility (explicitly out of scope, Boundaries & Constraints — unreliable across node
// implementations) — it recognizes exactly these two specific, well-known phrasings and
// nothing else. Any other send error falls through to sendAndFinalize's normal "leave at
// signed, retry next poll" path.
func isAlreadyKnownError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already known") || strings.Contains(msg, "nonce too low")
}
