package core

import (
	"errors"
	"math/big"
	"time"
)

// WithdrawalStatusCreated is a withdrawal's initial status, immediately superseded in the
// SAME request by the policy-check-and-route step (Story 3.3): no withdrawal is ever
// observable at rest in this status — CreateWithdrawal advances past it before returning,
// in the same transaction as the hold placement (Design Notes, AD-6 "api-through-core,
// single writer"). Kept only because postgres.WithdrawalRepository.CreateWithdrawal still
// writes it transiently as part of that single insert-then-advance statement shape.
const WithdrawalStatusCreated = "created"

// WithdrawalStatusAwaitingApproval is a withdrawal whose amount exceeded its (chain,
// asset)'s configured approval threshold (Story 3.3, FR17): it waits here until an
// operator explicitly approves it via ApproveWithdrawal. Never entered or left by any
// path other than CreateWithdrawal (entry) and ApproveWithdrawal (exit) — there is no
// poller (Boundaries & Constraints).
const WithdrawalStatusAwaitingApproval = "awaiting-approval"

// WithdrawalStatusApproved is a withdrawal cleared to proceed toward Story 3.4's
// signing/broadcast path — reached either automatically (amount at or below threshold) or
// via an operator's explicit approval of a WithdrawalStatusAwaitingApproval withdrawal.
const WithdrawalStatusApproved = "approved"

// WithdrawalStatusSigned is a withdrawal whose nonce has been durably allocated from
// chain_nonce_state and whose broadcast_attempts row has been committed (Story 3.4, AD-11's
// exact wording: the nonce allocation and that row's insert commit BEFORE any sign/
// broadcast call happens) — but which has not yet been successfully signed AND broadcast.
// A withdrawal can sit here indefinitely if the signer or the broadcast call failed (I/O &
// Edge-Case Matrix: "...leaves that withdrawal at signed with no tx_hash"); resuming it is
// explicitly Story 3.5's job, never this one's (Boundaries & Constraints).
const WithdrawalStatusSigned = "signed"

// WithdrawalStatusBroadcast is a withdrawal whose signed transaction has been successfully
// sent to the chain (eth_sendRawTransaction accepted it) — tx_hash is now known, and the
// broadcaster's poll-receipts phase checks it against the chain's finalized tag every poll
// cycle until it settles to confirmed or failed.
const WithdrawalStatusBroadcast = "broadcast"

// WithdrawalStatusConfirmed is a withdrawal whose broadcast transaction has a successful
// receipt at the chain's finalized tag (Story 3.4, mirroring AD-7's identical tag choice
// for deposit crediting) — terminal: its hold has been settled (debit hold, credit
// treasury).
const WithdrawalStatusConfirmed = "confirmed"

// WithdrawalStatusFailed is a withdrawal whose broadcast transaction reverted on-chain
// (confirmed at the finalized tag, but with a failed receipt status) — terminal: its hold
// has been released back to the customer's available balance (debit hold, credit
// available).
const WithdrawalStatusFailed = "failed"

// ErrMalformedDestinationAddress is returned when a withdrawal request's destination
// address is not a structurally well-formed 20-byte hex address
// (^0x[0-9a-fA-F]{40}$, matching unsupported_token_observations.address's existing CHECK
// convention). This is a pure SHAPE check, distinct from ErrInvalidDestinationAddress's
// denylist check below (Story 3.2 vs. 3.3's own boundary split).
var ErrMalformedDestinationAddress = errors.New("destination address must be a well-formed 0x-prefixed 40-hex-character address")

// ErrInvalidDestinationAddress is returned when a withdrawal request's destination
// address is structurally well-formed but matches a known-invalid target — v1's only
// denylist entry is the zero address (Story 3.3, FR18: "e.g. the zero address," not an
// exhaustive list). Checked after ErrMalformedDestinationAddress's shape check, before any
// hold is placed.
var ErrInvalidDestinationAddress = errors.New("destination address is a known-invalid target (the zero address)")

// ErrInsufficientBalanceForFee is returned when a customer's available balance, after
// Story 3.2's hold has already reclassified the requested amount out of it, does not
// cover the withdrawal's estimated fee (Story 3.3, FR18). Arithmetically identical to
// requiring pre-hold available >= amount + fee (Design Notes) — distinct from
// ErrInsufficientBalance, which is the pre-hold "can't even cover amount" case.
var ErrInsufficientBalanceForFee = errors.New("available balance does not cover the estimated withdrawal fee")

// ErrWithdrawalNotAwaitingApproval is returned by WithdrawalRepository.ApproveWithdrawal
// when the withdrawal being approved is not currently in the awaiting-approval status —
// covers both "already approved" (including the losing side of a concurrent double-
// approve race) and "still created/never routed to awaiting-approval in the first place."
var ErrWithdrawalNotAwaitingApproval = errors.New("withdrawal is not awaiting approval")

// ErrWithdrawalNotFound is returned by WithdrawalRepository.ApproveWithdrawal when no
// withdrawal with the given id exists.
var ErrWithdrawalNotFound = errors.New("withdrawal not found")

// ErrDuplicateWithdrawalCause is returned by WithdrawalRepository.CreateWithdrawal when a
// journal entry already exists for the given idempotency key — a narrow race between two
// concurrent requests carrying the same Idempotency-Key (AD-3, AD-5), mirroring
// ErrDuplicateTransferCause exactly. The caller should retry; a retry lands after the
// winning request's commit and is served by IdempotencyMiddleware's own dedup.
var ErrDuplicateWithdrawalCause = errors.New("a journal entry already exists for this idempotency key")

// ErrWithdrawalNotSigned is returned by WithdrawalRepository.RecordSignedTx/MarkBroadcast
// (Story 3.5, replacing Story 3.4's RecordBroadcastTxHash) when the withdrawal being
// recorded/transitioned is not currently at WithdrawalStatusSigned — defensive, should be
// unreachable: SignAndBroadcastWithdrawal only ever calls these against a row its own
// ClaimApprovedWithdrawal or ListSignedWithdrawals call already found at WithdrawalStatusSigned,
// in the same broadcaster process, with no other writer of withdrawals.status ever running
// concurrently against the same row (AD-11: exactly one broadcaster process per chain).
var ErrWithdrawalNotSigned = errors.New("withdrawal is not signed")

// ErrWithdrawalNotBroadcast is returned by WithdrawalRepository.SettleConfirmedWithdrawal
// and SettleFailedWithdrawal when the withdrawal being settled is not currently at
// WithdrawalStatusBroadcast — defensive, should be unreachable: PollWithdrawalReceipts only
// ever calls these for withdrawals its own ListBroadcastWithdrawals call just returned.
var ErrWithdrawalNotBroadcast = errors.New("withdrawal is not broadcast")

// WithdrawalRequest is the input to CreateWithdrawal (Story 3.2).
type WithdrawalRequest struct {
	CustomerID         string
	Chain              Chain
	Asset              Asset
	Amount             *big.Int
	DestinationAddress string
	// IdempotencyKey becomes the withdrawal hold's journal entry cause_id, exactly
	// mirroring TransferRequest.IdempotencyKey.
	IdempotencyKey string
}

// Withdrawal is a requested withdrawal with its hold already placed (Story 3.2) — no
// money has left the customer and no chain interaction has happened yet: this is a plain
// ledger reclassification of req.Amount from the customer's own available account to its
// own hold account, for the same (chain, asset) pair, in one balanced journal entry.
// ApprovedAt/ApprovedBy/ApprovalReason are populated once ApproveWithdrawal has
// transitioned this row from awaiting-approval to approved (Story 3.3, NFR11) — all three
// remain zero-valued for a withdrawal that was auto-approved by the threshold check
// (never touched by an operator) or is still awaiting approval.
type Withdrawal struct {
	ID                 string
	CustomerID         string
	Chain              Chain
	Asset              Asset
	Amount             *big.Int
	DestinationAddress string
	Status             string
	CreatedAt          time.Time
	ApprovedAt         *time.Time
	ApprovedBy         string
	ApprovalReason     string
	// TxHash is this withdrawal's broadcast transaction hash (Story 3.4) — "" until
	// WithdrawalRepository.RecordBroadcastTxHash has run. A denormalized read-convenience
	// copy of broadcast_attempts.tx_hash (Design Notes); broadcast_attempts remains the
	// source of truth.
	TxHash string
	// Nonce is the per-chain nonce WithdrawalRepository.ClaimApprovedWithdrawal allocated
	// for this withdrawal from chain_nonce_state (Story 3.4, AD-10: nonce state is per-chain
	// only, never per-address) — nil until claimed (still WithdrawalStatusApproved or
	// earlier), populated from WithdrawalStatusSigned onward.
	Nonce *int64
	// SignedTx is this withdrawal's exact signed transaction bytes, as already returned by
	// TransactionBroadcaster.AssembleSignedTx and persisted by WithdrawalRepository.
	// RecordSignedTx BEFORE any send is ever attempted (Story 3.5's core restructuring,
	// Design Notes) — nil/empty for a withdrawal claimed but not yet signed this attempt,
	// populated (via ListSignedWithdrawals) for one already signed in a prior attempt, crash,
	// or interrupted send: SignAndBroadcastWithdrawal.Execute branches on exactly this field
	// to decide "build/sign fresh" vs. "resend these exact bytes, never re-sign." A plain
	// []byte, not a go-ethereum type (AD-1) — the same opaque shape
	// TransactionBroadcaster.SendRawTransaction already takes.
	SignedTx []byte
	// StuckAlertedAt records when DetectStuckWithdrawals wrote this withdrawal's one-time
	// "withdrawal.stuck" outbox event (Story 3.5) — nil until that happens, and never cleared
	// afterward even once the withdrawal later confirms or fails (I/O & Edge-Case Matrix's own
	// last row: a historical fact, not a live status).
	StuckAlertedAt *time.Time
}
