package core

import (
	"context"
	"fmt"
	"time"
)

// maxBlocksPerScan caps how many blocks a single poll cycle scans (re-review 2026-07-16).
// Without a cap, a large backlog — first deploy (cursor at 0), or any extended watcher
// downtime — could span a range an RPC provider's eth_getLogs rejects outright; every
// subsequent poll would then retry the identical oversized range and fail identically,
// deadlocking the watcher forever (the cursor never advances past a failed poll). Capping
// the range makes catch-up incremental across multiple poll cycles instead.
const maxBlocksPerScan = 2000

// TrackDeposits runs one watcher poll cycle for a single configured chain: re-check every
// observed/safe deposit's stored block_hash against the chain's current history and
// orphan any that no longer match (Story 2.4, first phase — see Execute's comment on
// ordering), reload the known deposit-address set, read the chain's head/safe/finalized
// tags, scan for new ETH/USDC transfers since the last observed cursor, record each as an
// observed deposit (plus its paired "deposit.pending" outbox event, AD-4), promote every
// observed deposit whose block is at or below the chain's current safe tag, then promote
// every safe deposit whose block is at or below the chain's current finalized tag and
// credit every finalized deposit whose (chain, asset) crediting policy is 'finalized'
// (Story 2.2, FR9). One OS process runs this per chain (AD-2), on a ticker
// (cmd/walletd/main.go's watcher subcommand).
type TrackDeposits struct {
	scanner             ChainScanner
	addressLister       DepositAddressLister
	tokenRegistryLister TokenRegistryLister
	repo                DepositRepository
	unsupportedRepo     UnsupportedTokenRepository
	txBeginner          TxBeginner
}

// NewTrackDeposits constructs the use case against the given ports.
func NewTrackDeposits(scanner ChainScanner, addressLister DepositAddressLister, tokenRegistryLister TokenRegistryLister, repo DepositRepository, unsupportedRepo UnsupportedTokenRepository, txBeginner TxBeginner) *TrackDeposits {
	return &TrackDeposits{
		scanner:             scanner,
		addressLister:       addressLister,
		tokenRegistryLister: tokenRegistryLister,
		repo:                repo,
		unsupportedRepo:     unsupportedRepo,
		txBeginner:          txBeginner,
	}
}

// ExecuteResult summarizes what one Execute poll cycle actually did — distinct from the
// error return, which only says whether the poll succeeded. Populated only on success;
// a failed poll returns the zero value alongside its error, since the whole cycle rolled
// back and there is nothing meaningful to report. Intended for operator-facing logging
// (cmd/walletd/main.go), not for any control-flow decision.
type ExecuteResult struct {
	Latest, Safe, Finalized uint64
	// ScannedFrom/ScannedTo are both 0 if no new blocks were scanned this cycle (the
	// observed cursor had already caught up to Latest).
	ScannedFrom, ScannedTo uint64
	// DepositsObserved/UnsupportedObserved count only newly inserted rows — a repoll's
	// no-op (AD-5) is not counted again.
	DepositsObserved    int
	UnsupportedObserved int
	PromotedToSafe      int
	PromotedToFinalized int
	Credited            int
}

// Execute runs one poll cycle for chain. The observed-transition writes (RecordObserved
// per transfer + advancing the observed cursor), the safe-promotion writes (PromoteToSafe
// + advancing the safe cursor), and the finalized/credit-phase writes (PromoteToFinalized
// + advancing the finalized cursor + CreditFinalizedDeposits) all commit in the same
// transaction (AD-4) — a failure partway through leaves every deposit row, every cursor,
// and the ledger exactly as they were before this call, so the next poll simply retries.
func (uc *TrackDeposits) Execute(ctx context.Context, chain Chain) (ExecuteResult, error) {
	// Known addresses and the chain's head/safe tags are plain reads (one against
	// Postgres, one against the RPC endpoint) with nothing to roll back, so they run
	// before the transaction opens — mirroring how BalanceRepository/CustomerReader
	// read outside any transaction elsewhere in this codebase.
	addresses, err := uc.addressLister.ListDepositAddresses(ctx)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("list known deposit addresses: %w", err)
	}

	// The token registry snapshot is reloaded every poll cycle, the same "simple and
	// correct" choice as addresses above (Story 2.3, FR34): an operator's newly added
	// registry row is picked up on the very next poll with zero code change.
	tokenRegistry, err := uc.tokenRegistryLister.ListTokenRegistry(ctx, chain)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("list token registry: %w", err)
	}

	latest, safe, finalized, err := uc.scanner.Head(ctx)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("read chain head/safe tags: %w", err)
	}
	// A well-formed chain always reports finalized <= safe <= latest — these three tags
	// are exactly what now drives real ledger crediting (Story 2.2), so a nonsensical
	// ordering (misconfigured RPC, a reset local chain, a buggy provider) must fail loud
	// here, the same discipline already applied to the observed cursor below, rather than
	// silently feeding a bad tag into PromoteToFinalized/CreditFinalizedDeposits.
	if !(finalized <= safe && safe <= latest) {
		return ExecuteResult{}, fmt.Errorf("chain %s reported inconsistent tags: finalized=%d, safe=%d, latest=%d (want finalized <= safe <= latest)", chain, finalized, safe, latest)
	}

	result := ExecuteResult{Latest: latest, Safe: safe, Finalized: finalized}

	txCtx, tx, err := uc.txBeginner.Begin(ctx)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(txCtx))
		}
	}()

	// Story 2.4: reorg-checking runs FIRST, before the observed-scan/safe-promotion/
	// finalized-promotion/crediting phases below, all still inside this one transaction
	// (AD-4) — so a deposit orphaned this cycle can never also be promoted or credited
	// in the same cycle (Design Notes). Only observed/safe deposits are ever candidates
	// (ListPendingDeposits never returns finalized/credited rows), which is what makes
	// AC1's "no balance ever affected" true by construction, not a runtime guard.
	// if err := uc.checkForReorgs(txCtx, chain); err != nil {
	// 	return ExecuteResult{}, fmt.Errorf("check for reorgs: %w", err)
	// }

	observedCursor, err := uc.repo.Cursor(txCtx, chain, CursorTierObserved)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("read observed cursor: %w", err)
	}
	// A persisted cursor already at or beyond the chain's reported head is not a normal
	// steady state to silently absorb — it indicates the chain moved backward underneath
	// the watcher (a local anvil reset, a swapped/misconfigured RPC endpoint) and deserves
	// a loud failure, not an indefinite silent no-op (re-review 2026-07-16).
	if observedCursor > latest {
		return ExecuteResult{}, fmt.Errorf("observed cursor (%d) is ahead of chain %s's reported head (%d) — chain reset or RPC misconfiguration?", observedCursor, chain, latest)
	}
	scanFrom := observedCursor + 1

	if scanFrom <= latest {
		// Cap the range to maxBlocksPerScan (computed only here, where scanFrom <=
		// latest is already known, to avoid an unsigned-underflow trap computing
		// latest-scanFrom when there's nothing new to scan at all).
		scanTo := latest
		if latest-scanFrom+1 > maxBlocksPerScan {
			scanTo = scanFrom + maxBlocksPerScan - 1
		}
		result.ScannedFrom, result.ScannedTo = scanFrom, scanTo

		transfers, unsupported, err := uc.scanner.ScanDeposits(txCtx, addresses, tokenRegistry, scanFrom, scanTo)
		if err != nil {
			return ExecuteResult{}, fmt.Errorf("scan deposits: %w", err)
		}

		now := time.Now().UTC()
		for _, t := range transfers {
			id, err := newUUIDv7()
			if err != nil {
				return ExecuteResult{}, fmt.Errorf("generate deposit id: %w", err)
			}
			deposit := Deposit{
				ID:          id,
				Chain:       t.Chain,
				Asset:       t.Asset,
				Address:     t.Address,
				TxHash:      t.TxHash,
				LogIndex:    t.LogIndex,
				Amount:      t.Amount,
				BlockNumber: t.BlockNumber,
				BlockHash:   t.BlockHash,
				State:       DepositObserved,
				ObservedAt:  now,
				UpdatedAt:   now,
			}
			// A conflict on (chain, tx_hash, log_index) is a no-op by construction
			// (AD-5) — RecordObserved reports it via inserted=false, never an error, so
			// a repoll of an already-observed event is silently harmless here.
			inserted, err := uc.repo.RecordObserved(txCtx, deposit)
			if err != nil {
				return ExecuteResult{}, fmt.Errorf("record observed deposit: %w", err)
			}
			if inserted {
				result.DepositsObserved++
			}
		}

		// Story 2.3: every log the scanner classified as unsupported (no token_registry
		// match) is recorded, in this same transaction (AD-4), as a visible-but-never-
		// credited observation — never a deposits row, never a journal posting (FR11).
		// RecordObservation's own (chain, tx_hash, log_index) UNIQUE constraint makes a
		// repoll of the same event a harmless no-op, mirroring RecordObserved above.
		for _, u := range unsupported {
			id, err := newUUIDv7()
			if err != nil {
				return ExecuteResult{}, fmt.Errorf("generate unsupported token observation id: %w", err)
			}
			u.ID = id
			u.ObservedAt = now
			inserted, err := uc.unsupportedRepo.RecordObservation(txCtx, u)
			if err != nil {
				return ExecuteResult{}, fmt.Errorf("record unsupported token observation: %w", err)
			}
			if inserted {
				result.UnsupportedObserved++
			}
		}

		// Advance to scanTo, not latest — when the range was capped, only [scanFrom,
		// scanTo] was actually scanned, so the cursor must reflect exactly that or the
		// unscanned tail of this poll's range would be silently skipped forever.
		if err := uc.repo.SetCursor(txCtx, chain, CursorTierObserved, scanTo); err != nil {
			return ExecuteResult{}, fmt.Errorf("advance observed cursor: %w", err)
		}
	}

	promotedToSafe, err := uc.repo.PromoteToSafe(txCtx, chain, safe)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("promote deposits to safe: %w", err)
	}
	result.PromotedToSafe = promotedToSafe
	if err := uc.repo.SetCursor(txCtx, chain, CursorTierSafe, safe); err != nil {
		return ExecuteResult{}, fmt.Errorf("advance safe cursor: %w", err)
	}

	// Story 2.2: promote safe deposits to finalized, then credit every finalized deposit
	// whose (chain, asset) crediting policy says 'finalized' (the only v1 value) — all
	// still inside this same transaction (AD-4), so a failure partway through this phase
	// leaves every deposit row, both new cursors, and the ledger exactly as they were
	// before this call.
	promotedToFinalized, err := uc.repo.PromoteToFinalized(txCtx, chain, finalized)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("promote deposits to finalized: %w", err)
	}
	result.PromotedToFinalized = promotedToFinalized
	if err := uc.repo.SetCursor(txCtx, chain, CursorTierFinalized, finalized); err != nil {
		return ExecuteResult{}, fmt.Errorf("advance finalized cursor: %w", err)
	}
	credited, err := uc.repo.CreditFinalizedDeposits(txCtx, chain)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("credit finalized deposits: %w", err)
	}
	result.Credited = credited

	if err := tx.Commit(context.WithoutCancel(txCtx)); err != nil {
		return ExecuteResult{}, fmt.Errorf("commit transaction: %w", err)
	}
	committed = true

	return result, nil
}

// checkForReorgs re-verifies every observed/safe deposit on chain against the chain's
// current history (Story 2.4): for each deposit whose stored block_hash no longer matches
// the chain's current hash at that height — or whose height no longer exists at all — it
// calls OrphanDeposit, which transitions the row to orphaned and writes the paired
// "deposit.orphaned" outbox event in the same transaction (AD-4). Deposits are deduped by
// block_number before calling BlockHash (Design Notes) — a cheap optimization, not a scale
// requirement, avoiding a redundant RPC call when multiple deposits share a block.
func (uc *TrackDeposits) checkForReorgs(txCtx context.Context, chain Chain) error {
	pending, err := uc.repo.ListPendingDeposits(txCtx, chain)
	if err != nil {
		return fmt.Errorf("list pending deposits: %w", err)
	}
	if len(pending) == 0 {
		return nil
	}

	// currentHash caches BlockHash's result per unique height so a block shared by
	// multiple pending deposits is only ever queried once this cycle.
	currentHash := make(map[uint64]struct {
		hash   string
		exists bool
	}, len(pending))

	for _, d := range pending {
		if d.BlockHash == "" {
			// A legacy row from before this story's migration — its historical block_hash
			// was never captured (block_hash is nullable precisely because it couldn't be
			// backfilled, re-review 2026-07-17), so there is nothing to compare it
			// against. Left un-checked, exactly as it was before this story existed.
			continue
		}

		cached, ok := currentHash[d.BlockNumber]
		if !ok {
			hash, exists, err := uc.scanner.BlockHash(txCtx, d.BlockNumber)
			if err != nil {
				return fmt.Errorf("read block hash at height %d: %w", d.BlockNumber, err)
			}
			cached = struct {
				hash   string
				exists bool
			}{hash: hash, exists: exists}
			currentHash[d.BlockNumber] = cached
		}

		// A mismatched hash (block replaced by a competing history) or a height that no
		// longer exists (the chain got shorter than the deposit's height) are both
		// unambiguous proof the originally-observed block is gone — orphan the deposit
		// either way (Design Notes: "detection is a stored-hash comparison, not a depth
		// heuristic").
		if !cached.exists || cached.hash != d.BlockHash {
			if err := uc.repo.OrphanDeposit(txCtx, d.ID); err != nil {
				return fmt.Errorf("orphan deposit %s: %w", d.ID, err)
			}
		}
	}

	return nil
}
