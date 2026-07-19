package core_test

import (
	"context"
	"fmt"
	"maps"
	"math/big"
	"testing"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// fakeTx and fakeTxBeginner mirror the same fakes used to unit-test IdempotencyMiddleware
// (internal/adapter/api/middleware_idempotency_test.go) — the same AD-4 "one transaction"
// contract, exercised here against TrackDeposits.Execute instead of an HTTP middleware.
type fakeTx struct {
	committed  bool
	rolledBack bool
	// onRollback, when set, is invoked by Rollback before returning — wired by
	// fakeTxBeginner (when its repo field is set) to restore fakeDepositRepo to the
	// snapshot taken at Begin, modeling for this in-memory fake the same all-or-nothing
	// guarantee AD-4 gives the real postgres-backed transaction (Story 2.5's mid-batch-
	// failure tests).
	onRollback func()
}

func (t *fakeTx) Commit(ctx context.Context) error { t.committed = true; return nil }
func (t *fakeTx) Rollback(ctx context.Context) error {
	t.rolledBack = true
	if t.onRollback != nil {
		t.onRollback()
	}
	return nil
}

type fakeTxBeginner struct {
	beginCount int
	lastTx     *fakeTx
	// repo, when non-nil, makes Begin snapshot repo's mutable state and wires the
	// returned fakeTx's Rollback to restore it (Story 2.5): unlike a real Postgres
	// transaction, this in-memory fake has no engine-level rollback of its own state, so
	// tests that need to prove a mid-batch failure leaves nothing committed set this
	// field. Every other test leaves it nil, so Rollback stays a pure no-op exactly as
	// before.
	repo *fakeDepositRepo
}

func (b *fakeTxBeginner) Begin(ctx context.Context) (context.Context, core.Tx, error) {
	b.beginCount++
	tx := &fakeTx{}
	if b.repo != nil {
		snap := b.repo.snapshot()
		tx.onRollback = func() { b.repo.restore(snap) }
		// recordObservedCalls counts calls within the CURRENT Execute invocation only —
		// reset here, at the one point (Begin) that runs exactly once per Execute call.
		b.repo.recordObservedCalls = 0
	}
	b.lastTx = tx
	return ctx, tx, nil
}

// fakeScanner is a core.ChainScanner test double. By default it does not filter its
// configured transfers by the requested block range — tests that care about the
// requested range assert on gotFrom/gotTo directly, and tests simulating a repoll (the
// same event observed twice) rely on this to return the same transfer again without
// needing a real chain's block layout. Setting filterByBlockRange to true (Story 2.5)
// makes ScanDeposits actually respect [fromBlock, toBlock] like the real evm.Scanner
// does — required for the multi-poll catch-up test, where only a scanner that filters by
// range makes incremental, non-overlapping catch-up across capped ranges observable at
// all.
type fakeScanner struct {
	latest, safe, finalized uint64
	headErr                 error
	transfers               []core.ObservedTransfer
	unsupported             []core.UnsupportedTokenObservation
	scanErr                 error
	scanCalls               int
	gotFrom, gotTo          uint64
	gotTokenRegistry        map[string]core.Asset
	filterByBlockRange      bool
	// scanRanges records every [fromBlock, toBlock] ScanDeposits was called with, in
	// order — so a test can assert the exact sequence of ranges a multi-call catch-up
	// scanned (contiguous, capped, no gap, no overlap), not just the last one.
	scanRanges [][2]uint64

	// blockHashes is the fake chain's current canonical hash at each height, keyed by
	// block number — standing in for the real chain's history the way fakeDepositRepo's
	// seen-set stands in for a real UNIQUE constraint. A height absent from this map
	// simulates BlockHash's exists=false ("chain got shorter than the deposit's height").
	blockHashes    map[uint64]string
	blockHashErr   error
	blockHashCalls []uint64
}

func (s *fakeScanner) Head(ctx context.Context) (uint64, uint64, uint64, error) {
	return s.latest, s.safe, s.finalized, s.headErr
}

// BlockHash is a core.ChainScanner test double for Story 2.4's reorg-check phase. It
// records every call (blockHashCalls) so a test can assert TrackDeposits' dedupe-by-
// block_number optimization (Design Notes) actually avoids redundant calls.
func (s *fakeScanner) BlockHash(ctx context.Context, blockNumber uint64) (string, bool, error) {
	s.blockHashCalls = append(s.blockHashCalls, blockNumber)
	if s.blockHashErr != nil {
		return "", false, s.blockHashErr
	}
	hash, ok := s.blockHashes[blockNumber]
	return hash, ok, nil
}

func (s *fakeScanner) ScanDeposits(ctx context.Context, knownAddresses []string, tokenRegistry map[string]core.Asset, fromBlock, toBlock uint64) ([]core.ObservedTransfer, []core.UnsupportedTokenObservation, error) {
	s.scanCalls++
	s.gotFrom, s.gotTo = fromBlock, toBlock
	s.scanRanges = append(s.scanRanges, [2]uint64{fromBlock, toBlock})
	s.gotTokenRegistry = tokenRegistry
	if s.scanErr != nil {
		return nil, nil, s.scanErr
	}
	if !s.filterByBlockRange {
		return s.transfers, s.unsupported, nil
	}
	var transfers []core.ObservedTransfer
	for _, t := range s.transfers {
		if t.BlockNumber >= fromBlock && t.BlockNumber <= toBlock {
			transfers = append(transfers, t)
		}
	}
	var unsupported []core.UnsupportedTokenObservation
	for _, u := range s.unsupported {
		if u.BlockNumber >= fromBlock && u.BlockNumber <= toBlock {
			unsupported = append(unsupported, u)
		}
	}
	return transfers, unsupported, nil
}

type fakeAddressLister struct {
	addresses []string
}

func (l *fakeAddressLister) ListDepositAddresses(ctx context.Context) ([]string, error) {
	return l.addresses, nil
}

// fakeTokenRegistryLister is a core.TokenRegistryLister test double.
type fakeTokenRegistryLister struct {
	registry map[string]core.Asset
	err      error
}

func (l *fakeTokenRegistryLister) ListTokenRegistry(ctx context.Context, chain core.Chain) (map[string]core.Asset, error) {
	return l.registry, l.err
}

// fakeUnsupportedTokenRepo is a core.UnsupportedTokenRepository test double. It
// reproduces the real postgres repository's key behavior — RecordObservation is a no-op
// (inserted=false) for an already-seen (chain, tx_hash, log_index), never an error — via
// a plain seen-set, standing in for the real UNIQUE constraint (AD-5), the same technique
// fakeDepositRepo uses for RecordObserved.
type fakeUnsupportedTokenRepo struct {
	seen     map[string]bool
	recorded []core.UnsupportedTokenObservation
}

func newFakeUnsupportedTokenRepo() *fakeUnsupportedTokenRepo {
	return &fakeUnsupportedTokenRepo{seen: map[string]bool{}}
}

func unsupportedKey(o core.UnsupportedTokenObservation) string {
	return fmt.Sprintf("%s|%s|%d", o.Chain, o.TxHash, o.LogIndex)
}

func (r *fakeUnsupportedTokenRepo) RecordObservation(ctx context.Context, o core.UnsupportedTokenObservation) (bool, error) {
	k := unsupportedKey(o)
	if r.seen[k] {
		return false, nil
	}
	r.seen[k] = true
	r.recorded = append(r.recorded, o)
	return true, nil
}

func (r *fakeUnsupportedTokenRepo) ListObservations(ctx context.Context) ([]core.UnsupportedTokenObservation, error) {
	return r.recorded, nil
}

// fakeDepositRepo is a core.DepositRepository test double. It reproduces the real
// postgres repository's key behavior — RecordObserved is a no-op (inserted=false) for an
// already-seen (chain, tx_hash, log_index), never an error — via a plain seen-set,
// standing in for the real UNIQUE constraint (AD-5).
type fakeDepositRepo struct {
	seen                   map[string]bool
	inserted               []core.Deposit
	promoteCalls           int
	promotedChain          core.Chain
	promotedSafe           uint64
	promoteFinalizedCalls  int
	promotedFinalizedChain core.Chain
	promotedFinalizedBlock uint64
	creditCalls            int
	creditedChain          core.Chain
	cursors                map[string]uint64
	orphanedIDs            []string

	// recordObservedFailAt, when > 0, makes RecordObserved return an error on exactly its
	// Nth call within the current Execute invocation (Story 2.5's mid-batch-failure
	// tests) — simulating a poll that fails partway through recording a multi-deposit
	// scan result. 0 (the default) disables failure injection entirely, so every
	// pre-existing test is unaffected.
	recordObservedFailAt int
	// recordObservedCalls counts RecordObserved calls within the current Execute
	// invocation only — reset by fakeTxBeginner.Begin (when wired to this repo) at the
	// start of every call.
	recordObservedCalls int
}

func newFakeDepositRepo() *fakeDepositRepo {
	return &fakeDepositRepo{seen: map[string]bool{}, cursors: map[string]uint64{}}
}

func depositKey(d core.Deposit) string {
	return fmt.Sprintf("%s|%s|%d", d.Chain, d.TxHash, d.LogIndex)
}

func (r *fakeDepositRepo) RecordObserved(ctx context.Context, d core.Deposit) (bool, error) {
	r.recordObservedCalls++
	if r.recordObservedFailAt > 0 && r.recordObservedCalls == r.recordObservedFailAt {
		return false, fmt.Errorf("simulated mid-batch RecordObserved failure on call %d", r.recordObservedCalls)
	}
	k := depositKey(d)
	if r.seen[k] {
		return false, nil
	}
	r.seen[k] = true
	r.inserted = append(r.inserted, d)
	return true, nil
}

// snapshot captures a deep copy of every field a poll's writes can mutate, so a
// simulated rollback (fakeTxBeginner's repo-linked onRollback hook) can restore this fake
// to its exact pre-transaction state (Story 2.5) — mirroring what a real ROLLBACK
// guarantees for the real postgres-backed DepositRepository, which this in-memory fake
// has no automatic equivalent of on its own. recordObservedFailAt/recordObservedCalls are
// deliberately excluded: they are test-harness configuration/bookkeeping, not persisted
// repository state, so a rollback must never touch them.
func (r *fakeDepositRepo) snapshot() *fakeDepositRepo {
	return &fakeDepositRepo{
		seen:                   maps.Clone(r.seen),
		inserted:               append([]core.Deposit(nil), r.inserted...),
		promoteCalls:           r.promoteCalls,
		promotedChain:          r.promotedChain,
		promotedSafe:           r.promotedSafe,
		promoteFinalizedCalls:  r.promoteFinalizedCalls,
		promotedFinalizedChain: r.promotedFinalizedChain,
		promotedFinalizedBlock: r.promotedFinalizedBlock,
		creditCalls:            r.creditCalls,
		creditedChain:          r.creditedChain,
		cursors:                maps.Clone(r.cursors),
		orphanedIDs:            append([]string(nil), r.orphanedIDs...),
	}
}

// restore overwrites r's mutable state with snap's — see snapshot's doc comment.
func (r *fakeDepositRepo) restore(snap *fakeDepositRepo) {
	r.seen = snap.seen
	r.inserted = snap.inserted
	r.promoteCalls = snap.promoteCalls
	r.promotedChain = snap.promotedChain
	r.promotedSafe = snap.promotedSafe
	r.promoteFinalizedCalls = snap.promoteFinalizedCalls
	r.promotedFinalizedChain = snap.promotedFinalizedChain
	r.promotedFinalizedBlock = snap.promotedFinalizedBlock
	r.creditCalls = snap.creditCalls
	r.creditedChain = snap.creditedChain
	r.cursors = snap.cursors
	r.orphanedIDs = snap.orphanedIDs
}

func (r *fakeDepositRepo) PromoteToSafe(ctx context.Context, chain core.Chain, safeBlock uint64) (int, error) {
	r.promoteCalls++
	r.promotedChain = chain
	r.promotedSafe = safeBlock
	n := 0
	for i := range r.inserted {
		if r.inserted[i].Chain == chain && r.inserted[i].State == core.DepositObserved && r.inserted[i].BlockNumber <= safeBlock {
			r.inserted[i].State = core.DepositSafe
			n++
		}
	}
	return n, nil
}

// PromoteToFinalized mirrors PromoteToSafe's fake behavior one tier up: every inserted
// deposit for chain currently in the safe state whose block is at or below
// finalizedBlock moves to finalized.
func (r *fakeDepositRepo) PromoteToFinalized(ctx context.Context, chain core.Chain, finalizedBlock uint64) (int, error) {
	r.promoteFinalizedCalls++
	r.promotedFinalizedChain = chain
	r.promotedFinalizedBlock = finalizedBlock
	n := 0
	for i := range r.inserted {
		if r.inserted[i].Chain == chain && r.inserted[i].State == core.DepositSafe && r.inserted[i].BlockNumber <= finalizedBlock {
			r.inserted[i].State = core.DepositFinalized
			n++
		}
	}
	return n, nil
}

// CreditFinalizedDeposits reproduces the real repository's state guard: only deposits
// currently in the finalized state are ever credited, so an already-credited deposit is
// never re-selected on a later call (the same guarantee the real SQL's WHERE
// state='finalized' gives by construction).
func (r *fakeDepositRepo) CreditFinalizedDeposits(ctx context.Context, chain core.Chain) (int, error) {
	r.creditCalls++
	r.creditedChain = chain
	n := 0
	for i := range r.inserted {
		if r.inserted[i].Chain == chain && r.inserted[i].State == core.DepositFinalized {
			r.inserted[i].State = core.DepositCredited
			n++
		}
	}
	return n, nil
}

// ListPendingDeposits reproduces the real postgres repository's state guard (Story 2.4):
// only observed/safe rows on chain are ever returned, so an orphaned (or
// finalized/credited) row is never a candidate for re-checking by a later poll — the same
// "true by construction" pattern as CreditFinalizedDeposits' state='finalized' guard.
func (r *fakeDepositRepo) ListPendingDeposits(ctx context.Context, chain core.Chain) ([]core.Deposit, error) {
	var pending []core.Deposit
	for _, d := range r.inserted {
		if d.Chain == chain && (d.State == core.DepositObserved || d.State == core.DepositSafe) {
			pending = append(pending, d)
		}
	}
	return pending, nil
}

// OrphanDeposit transitions depositID to orphaned in place, mirroring the real
// repository's paired state-transition write (minus the outbox event, which this fake
// doesn't model — the same choice fakeDepositRepo.RecordObserved already makes for
// deposit.pending). Every call is recorded in orphanedIDs so a test can assert exactly
// which deposits were orphaned, and how many times.
func (r *fakeDepositRepo) OrphanDeposit(ctx context.Context, depositID string) error {
	r.orphanedIDs = append(r.orphanedIDs, depositID)
	for i := range r.inserted {
		if r.inserted[i].ID == depositID {
			r.inserted[i].State = core.DepositOrphaned
			return nil
		}
	}
	return fmt.Errorf("orphan deposit %s: no such deposit", depositID)
}

func (r *fakeDepositRepo) Cursor(ctx context.Context, chain core.Chain, tier string) (uint64, error) {
	return r.cursors[string(chain)+"/"+tier], nil
}

func (r *fakeDepositRepo) SetCursor(ctx context.Context, chain core.Chain, tier string, block uint64) error {
	r.cursors[string(chain)+"/"+tier] = block
	return nil
}

func TestTrackDeposits_Execute_NewObservedDeposit(t *testing.T) {
	t.Parallel()

	scanner := &fakeScanner{latest: 100, safe: 0, transfers: []core.ObservedTransfer{
		{Chain: core.ChainBase, Asset: core.AssetETH, Address: "0xAbC", TxHash: "0xdeadbeef", LogIndex: -1, Amount: big.NewInt(1000), BlockNumber: 50},
	}}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	repo := newFakeDepositRepo()
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}

	if len(repo.inserted) != 1 {
		t.Fatalf("inserted deposits = %d, want 1", len(repo.inserted))
	}
	got := repo.inserted[0]
	if got.State != core.DepositObserved {
		t.Fatalf("state = %q, want %q", got.State, core.DepositObserved)
	}
	if got.Chain != core.ChainBase || got.Asset != core.AssetETH || got.TxHash != "0xdeadbeef" || got.LogIndex != -1 {
		t.Fatalf("deposit = %+v, want the scanner's observed transfer verbatim", got)
	}
	if scanner.gotFrom != 1 || scanner.gotTo != 100 {
		t.Fatalf("scanned range = [%d, %d], want [1, 100] (cursor starts at 0, so fromBlock = 0+1)", scanner.gotFrom, scanner.gotTo)
	}
	if repo.cursors["base/"+core.CursorTierObserved] != 100 {
		t.Fatalf("observed cursor = %d, want 100 (advanced to latest)", repo.cursors["base/"+core.CursorTierObserved])
	}
	if !txb.lastTx.committed {
		t.Fatal("expected the transaction to be committed")
	}
	if txb.lastTx.rolledBack {
		t.Fatal("a successful poll must not roll back")
	}
}

func TestTrackDeposits_Execute_RepollSameEventIsNoOp(t *testing.T) {
	t.Parallel()

	transfer := core.ObservedTransfer{Chain: core.ChainBase, Asset: core.AssetETH, Address: "0xAbC", TxHash: "0xsame", LogIndex: -1, Amount: big.NewInt(1000), BlockNumber: 50, BlockHash: "0xblock50"}
	// blockHashes must agree with transfer.BlockHash at height 50 — otherwise the second
	// poll's reorg-check phase (which now runs on every Execute call, Story 2.4) would
	// see a "mismatch" against an unconfigured fake and orphan the deposit, which is not
	// what this test is about (that behavior is covered by the dedicated reorg-check
	// tests below).
	scanner := &fakeScanner{latest: 100, safe: 0, transfers: []core.ObservedTransfer{transfer}, blockHashes: map[uint64]string{50: "0xblock50"}}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	repo := newFakeDepositRepo()
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("first Execute() error = %v, want nil", err)
	}
	if len(repo.inserted) != 1 {
		t.Fatalf("after first poll, inserted deposits = %d, want 1", len(repo.inserted))
	}

	// Simulate more blocks arriving (so the second poll's range actually scans again)
	// while the scanner keeps returning the SAME already-observed transfer — the
	// scenario the (chain, tx_hash, log_index) unique constraint exists to make a
	// harmless no-op (AD-5), never an application-level existence check.
	scanner.latest = 200
	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("second Execute() error = %v, want nil", err)
	}

	if len(repo.inserted) != 1 {
		t.Fatalf("after repoll, inserted deposits = %d, want still 1 (no duplicate row)", len(repo.inserted))
	}
	if scanner.scanCalls != 2 {
		t.Fatalf("scan calls = %d, want 2 (both polls scanned)", scanner.scanCalls)
	}
	if repo.inserted[0].State != core.DepositObserved {
		t.Fatalf("state = %q, want unchanged %q (matching block hash — no reorg)", repo.inserted[0].State, core.DepositObserved)
	}
	if len(repo.orphanedIDs) != 0 {
		t.Fatalf("orphanedIDs = %v, want none", repo.orphanedIDs)
	}
}

func TestTrackDeposits_Execute_PromotesObservedToSafe(t *testing.T) {
	t.Parallel()

	scanner := &fakeScanner{latest: 100, safe: 50, transfers: []core.ObservedTransfer{
		{Chain: core.ChainBase, Asset: core.AssetETH, Address: "0xAbC", TxHash: "0xa", LogIndex: -1, Amount: big.NewInt(1), BlockNumber: 40},
		{Chain: core.ChainBase, Asset: core.AssetETH, Address: "0xAbC", TxHash: "0xb", LogIndex: -1, Amount: big.NewInt(1), BlockNumber: 60},
	}}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	repo := newFakeDepositRepo()
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}

	if repo.promoteCalls != 1 {
		t.Fatalf("PromoteToSafe calls = %d, want 1", repo.promoteCalls)
	}
	if repo.promotedSafe != 50 {
		t.Fatalf("promoted safe block = %d, want 50", repo.promotedSafe)
	}

	var gotA, gotB core.DepositState
	for _, d := range repo.inserted {
		switch d.TxHash {
		case "0xa":
			gotA = d.State
		case "0xb":
			gotB = d.State
		}
	}
	if gotA != core.DepositSafe {
		t.Fatalf("deposit at block 40 (<= safe 50) state = %q, want %q", gotA, core.DepositSafe)
	}
	if gotB != core.DepositObserved {
		t.Fatalf("deposit at block 60 (> safe 50) state = %q, want %q (not yet promoted)", gotB, core.DepositObserved)
	}
	if repo.cursors["base/"+core.CursorTierSafe] != 50 {
		t.Fatalf("safe cursor = %d, want 50", repo.cursors["base/"+core.CursorTierSafe])
	}
}

func TestTrackDeposits_Execute_UnsupportedTokenTransferIsIgnored(t *testing.T) {
	t.Parallel()

	// An empty scan result (e.g. every log the scanner saw was classified as
	// unsupported and returned via the second return value, not this one) proves
	// TrackDeposits handles it cleanly: no deposit row, no crash, and the transaction
	// still commits. TestTrackDeposits_Execute_UnsupportedTokenObservationIsRecorded
	// below covers the actual unsupported-observation recording path (Story 2.3).
	scanner := &fakeScanner{latest: 100, safe: 0, transfers: nil}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	repo := newFakeDepositRepo()
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}
	if len(repo.inserted) != 0 {
		t.Fatalf("inserted deposits = %d, want 0", len(repo.inserted))
	}
	if !txb.lastTx.committed {
		t.Fatal("expected the transaction to still commit on an empty scan result")
	}
}

// TestTrackDeposits_Execute_UnsupportedTokenObservationIsRecorded proves Story 2.3's
// core behavior: an unsupported-token observation returned by the scanner is recorded via
// UnsupportedTokenRepository (never as a deposit, never touching repo.inserted), while a
// supported transfer returned in the same poll is still recorded as a deposit exactly as
// before — the two return values are handled independently, in the same transaction.
func TestTrackDeposits_Execute_UnsupportedTokenObservationIsRecorded(t *testing.T) {
	t.Parallel()

	supported := core.ObservedTransfer{Chain: core.ChainBase, Asset: core.AssetUSDC, Address: "0xAbC", TxHash: "0xsupported", LogIndex: 1, Amount: big.NewInt(500), BlockNumber: 40}
	unsupported := core.UnsupportedTokenObservation{Chain: core.ChainBase, Address: "0xAbC", ContractAddress: "0xDeAdBeEf00000000000000000000000000000000", TxHash: "0xunsupported", LogIndex: 2, Amount: big.NewInt(999), BlockNumber: 41}
	scanner := &fakeScanner{
		latest:      100,
		safe:        0,
		transfers:   []core.ObservedTransfer{supported},
		unsupported: []core.UnsupportedTokenObservation{unsupported},
	}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	repo := newFakeDepositRepo()
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}

	if len(repo.inserted) != 1 || repo.inserted[0].TxHash != "0xsupported" {
		t.Fatalf("inserted deposits = %+v, want exactly the supported transfer", repo.inserted)
	}
	if len(unsupportedRepo.recorded) != 1 {
		t.Fatalf("recorded unsupported observations = %d, want 1", len(unsupportedRepo.recorded))
	}
	got := unsupportedRepo.recorded[0]
	if got.TxHash != "0xunsupported" || got.ContractAddress != unsupported.ContractAddress || got.Address != "0xAbC" || got.Amount.Cmp(big.NewInt(999)) != 0 {
		t.Fatalf("recorded unsupported observation = %+v, want the scanner's unsupported observation verbatim", got)
	}
	if got.ID == "" {
		t.Fatal("expected TrackDeposits to assign a generated id to the unsupported observation")
	}
	if got.ObservedAt.IsZero() {
		t.Fatal("expected TrackDeposits to assign an observedAt timestamp to the unsupported observation")
	}
	if !txb.lastTx.committed {
		t.Fatal("expected the transaction to be committed")
	}
}

func TestTrackDeposits_Execute_NoNewBlocksSkipsScan(t *testing.T) {
	t.Parallel()

	// cursor(observed) == latest means there is nothing new to scan; ScanDeposits must
	// not be called with an inverted (fromBlock > toBlock) range.
	scanner := &fakeScanner{latest: 10, safe: 10}
	repo := newFakeDepositRepo()
	repo.cursors["base/"+core.CursorTierObserved] = 10
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}
	if scanner.scanCalls != 0 {
		t.Fatalf("scan calls = %d, want 0 (no new blocks to scan)", scanner.scanCalls)
	}
}

func TestTrackDeposits_Execute_PromotesFinalizedAndCreditsInSamePoll(t *testing.T) {
	t.Parallel()

	// Both transfers land far enough below latest/safe that a single poll carries the
	// first all the way from observed -> safe -> finalized -> credited; the second stops
	// at safe because its block is above the finalized tag (50) but at/below the safe tag
	// (90).
	scanner := &fakeScanner{latest: 100, safe: 90, finalized: 50, transfers: []core.ObservedTransfer{
		{Chain: core.ChainBase, Asset: core.AssetETH, Address: "0xAbC", TxHash: "0xfinal", LogIndex: -1, Amount: big.NewInt(1), BlockNumber: 40},
		{Chain: core.ChainBase, Asset: core.AssetETH, Address: "0xAbC", TxHash: "0xnotfinal", LogIndex: -1, Amount: big.NewInt(1), BlockNumber: 60},
	}}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	repo := newFakeDepositRepo()
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}

	if repo.promoteFinalizedCalls != 1 {
		t.Fatalf("PromoteToFinalized calls = %d, want 1", repo.promoteFinalizedCalls)
	}
	if repo.promotedFinalizedBlock != 50 {
		t.Fatalf("promoted finalized block = %d, want 50", repo.promotedFinalizedBlock)
	}
	if repo.creditCalls != 1 {
		t.Fatalf("CreditFinalizedDeposits calls = %d, want 1", repo.creditCalls)
	}

	var gotFinal, gotNotFinal core.DepositState
	for _, d := range repo.inserted {
		switch d.TxHash {
		case "0xfinal":
			gotFinal = d.State
		case "0xnotfinal":
			gotNotFinal = d.State
		}
	}
	if gotFinal != core.DepositCredited {
		t.Fatalf("deposit at block 40 (<= finalized 50) state = %q, want %q (promoted through finalized and credited in the same poll)", gotFinal, core.DepositCredited)
	}
	if gotNotFinal != core.DepositSafe {
		t.Fatalf("deposit at block 60 (> finalized 50, <= safe 90) state = %q, want %q (safe but not yet finalized/credited)", gotNotFinal, core.DepositSafe)
	}
	if repo.cursors["base/"+core.CursorTierFinalized] != 50 {
		t.Fatalf("finalized cursor = %d, want 50", repo.cursors["base/"+core.CursorTierFinalized])
	}
}

func TestTrackDeposits_Execute_CreditedDepositNeverReCredited(t *testing.T) {
	t.Parallel()

	// Simulate a deposit that was already credited by an earlier poll: seed it directly
	// into repo.inserted at the credited state (bypassing the scan) so this poll's
	// PromoteToFinalized/CreditFinalizedDeposits calls are the only thing that could
	// touch it.
	repo := newFakeDepositRepo()
	repo.inserted = append(repo.inserted, core.Deposit{
		Chain: core.ChainBase, Asset: core.AssetETH, TxHash: "0xalreadycredited", LogIndex: -1,
		Amount: big.NewInt(1), BlockNumber: 10, State: core.DepositCredited,
	})
	scanner := &fakeScanner{latest: 100, safe: 90, finalized: 50}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}

	if repo.inserted[0].State != core.DepositCredited {
		t.Fatalf("already-credited deposit state = %q, want unchanged %q (never re-selected, never transitioned backward)", repo.inserted[0].State, core.DepositCredited)
	}
}

// TestTrackDeposits_Execute_ReorgCheck_MatchingHashNoChange proves the "no reorg" branch
// of Story 2.4's I/O matrix: an observed deposit whose stored block_hash still matches the
// chain's current hash at that height is left completely untouched.
func TestTrackDeposits_Execute_ReorgCheck_MatchingHashNoChange(t *testing.T) {
	t.Parallel()

	repo := newFakeDepositRepo()
	repo.inserted = append(repo.inserted, core.Deposit{
		ID: "dep-1", Chain: core.ChainBase, Asset: core.AssetETH, TxHash: "0xstillvalid", LogIndex: -1,
		Amount: big.NewInt(1), BlockNumber: 40, BlockHash: "0xaaa", State: core.DepositObserved,
	})
	scanner := &fakeScanner{latest: 100, safe: 0, finalized: 0, blockHashes: map[uint64]string{40: "0xaaa"}}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}

	if repo.inserted[0].State != core.DepositObserved {
		t.Fatalf("state = %q, want unchanged %q (stored hash still matches the chain's current hash)", repo.inserted[0].State, core.DepositObserved)
	}
	if len(repo.orphanedIDs) != 0 {
		t.Fatalf("orphanedIDs = %v, want none", repo.orphanedIDs)
	}
	if len(scanner.blockHashCalls) != 1 || scanner.blockHashCalls[0] != 40 {
		t.Fatalf("BlockHash calls = %v, want exactly one call for height 40", scanner.blockHashCalls)
	}
}

// TestTrackDeposits_Execute_ReorgCheck_MismatchedHashOrphans proves the core reorg-
// detection rule (Design Notes: "a stored-hash comparison, not a depth heuristic"): when
// the chain's current hash at a deposit's stored height no longer matches, the deposit is
// orphaned. Two deposits share the same block_number (40), proving the dedupe-by-
// block_number optimization (Design Notes) — BlockHash must be called exactly once for
// that height, not once per deposit.
func TestTrackDeposits_Execute_ReorgCheck_MismatchedHashOrphans(t *testing.T) {
	t.Parallel()

	repo := newFakeDepositRepo()
	repo.inserted = append(repo.inserted,
		core.Deposit{ID: "dep-1", Chain: core.ChainBase, Asset: core.AssetETH, TxHash: "0xreorged1", LogIndex: -1, Amount: big.NewInt(1), BlockNumber: 40, BlockHash: "0xaaa", State: core.DepositObserved},
		core.Deposit{ID: "dep-2", Chain: core.ChainBase, Asset: core.AssetUSDC, TxHash: "0xreorged2", LogIndex: 3, Amount: big.NewInt(2), BlockNumber: 40, BlockHash: "0xaaa", State: core.DepositSafe},
	)
	// The chain's current hash at height 40 is now "0xbbb" — a competing history replaced
	// the block both deposits were observed in.
	scanner := &fakeScanner{latest: 100, safe: 0, finalized: 0, blockHashes: map[uint64]string{40: "0xbbb"}}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}

	for _, d := range repo.inserted {
		if d.State != core.DepositOrphaned {
			t.Fatalf("deposit %s state = %q, want %q (block replaced by a competing history)", d.ID, d.State, core.DepositOrphaned)
		}
	}
	if len(repo.orphanedIDs) != 2 {
		t.Fatalf("orphanedIDs = %v, want exactly 2 (both deposits sharing the replaced block)", repo.orphanedIDs)
	}
	if len(scanner.blockHashCalls) != 1 {
		t.Fatalf("BlockHash calls = %v, want exactly 1 (deduped by block_number, Design Notes)", scanner.blockHashCalls)
	}
}

// TestTrackDeposits_Execute_ReorgCheck_HeightBeyondChainHeadOrphans proves the second
// unambiguous reorg signal (Design Notes' I/O matrix): a deposit's block_number no longer
// exists on the chain at all (BlockHash reports exists=false) — "the chain got shorter
// than the deposit's height" — is orphaned exactly like a hash mismatch, with no separate
// code path.
func TestTrackDeposits_Execute_ReorgCheck_HeightBeyondChainHeadOrphans(t *testing.T) {
	t.Parallel()

	repo := newFakeDepositRepo()
	repo.inserted = append(repo.inserted, core.Deposit{
		ID: "dep-1", Chain: core.ChainBase, Asset: core.AssetETH, TxHash: "0xvanished", LogIndex: -1,
		Amount: big.NewInt(1), BlockNumber: 40, BlockHash: "0xaaa", State: core.DepositObserved,
	})
	// blockHashes has no entry for height 40 — simulates the chain no longer having a
	// block there at all.
	scanner := &fakeScanner{latest: 100, safe: 0, finalized: 0, blockHashes: map[uint64]string{}}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}

	if repo.inserted[0].State != core.DepositOrphaned {
		t.Fatalf("state = %q, want %q (height no longer exists on the chain)", repo.inserted[0].State, core.DepositOrphaned)
	}
}

// TestTrackDeposits_Execute_ReorgCheck_OrphanedDepositNeverReselected proves AC1/AC2's
// "never conflated" guarantee holds across polls: once a deposit is orphaned, a later
// poll's reorg-check phase never selects it again (ListPendingDeposits is scoped to
// observed/safe only) — no repeat BlockHash call, no repeat OrphanDeposit call, no
// backward transition.
func TestTrackDeposits_Execute_ReorgCheck_OrphanedDepositNeverReselected(t *testing.T) {
	t.Parallel()

	repo := newFakeDepositRepo()
	repo.inserted = append(repo.inserted, core.Deposit{
		ID: "dep-1", Chain: core.ChainBase, Asset: core.AssetETH, TxHash: "0xreorged", LogIndex: -1,
		Amount: big.NewInt(1), BlockNumber: 40, BlockHash: "0xaaa", State: core.DepositObserved,
	})
	scanner := &fakeScanner{latest: 100, safe: 0, finalized: 0, blockHashes: map[uint64]string{40: "0xbbb"}}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	// First poll: mismatch detected, the deposit is orphaned.
	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("first Execute() error = %v, want nil", err)
	}
	if repo.inserted[0].State != core.DepositOrphaned {
		t.Fatalf("state after first poll = %q, want %q", repo.inserted[0].State, core.DepositOrphaned)
	}
	if len(repo.orphanedIDs) != 1 {
		t.Fatalf("orphanedIDs after first poll = %v, want exactly 1", repo.orphanedIDs)
	}

	// Second poll: the same (now-orphaned) deposit must never be re-selected, even
	// though its stored block_hash still "mismatches" scanner.blockHashes[40].
	scanner.blockHashCalls = nil
	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("second Execute() error = %v, want nil", err)
	}
	if len(scanner.blockHashCalls) != 0 {
		t.Fatalf("BlockHash calls on second poll = %v, want none (an orphaned deposit is never a reorg-check candidate)", scanner.blockHashCalls)
	}
	if len(repo.orphanedIDs) != 1 {
		t.Fatalf("orphanedIDs after second poll = %v, want still exactly 1 (never re-orphaned)", repo.orphanedIDs)
	}
	if repo.inserted[0].State != core.DepositOrphaned {
		t.Fatalf("state after second poll = %q, want unchanged %q", repo.inserted[0].State, core.DepositOrphaned)
	}
}

// TestTrackDeposits_Execute_MultiPollCatchUpAfterDowntime proves AC1 (the story's core
// claim): a backlog spanning more than one maxBlocksPerScan-sized range — simulating an
// extended watcher downtime, not just first-deploy's cursor-at-0 case — is fully
// recovered across multiple Execute calls, with no gap and no duplicate, and the cursor
// advances by exactly maxBlocksPerScan each call except the final (partial) one.
func TestTrackDeposits_Execute_MultiPollCatchUpAfterDowntime(t *testing.T) {
	t.Parallel()

	// Mirrors track_deposits.go's unexported maxBlocksPerScan constant (re-review
	// 2026-07-16) — this test package can't reference it directly (core_test is an
	// external test package), so the literal is duplicated here deliberately.
	const maxBlocksPerScan = 2000
	// latest is more than 2x maxBlocksPerScan beyond the persisted cursor (starts at 0):
	// catching up requires three Execute calls — [1,2000], [2001,4000], [4001,5000].
	const latest = 5000

	transfers := []core.ObservedTransfer{
		{Chain: core.ChainBase, Asset: core.AssetETH, Address: "0xAbC", TxHash: "0x1", LogIndex: -1, Amount: big.NewInt(1), BlockNumber: 500},
		{Chain: core.ChainBase, Asset: core.AssetETH, Address: "0xAbC", TxHash: "0x2", LogIndex: -1, Amount: big.NewInt(1), BlockNumber: 2500},
		{Chain: core.ChainBase, Asset: core.AssetETH, Address: "0xAbC", TxHash: "0x3", LogIndex: -1, Amount: big.NewInt(1), BlockNumber: 4500},
	}
	scanner := &fakeScanner{latest: latest, safe: 0, finalized: 0, transfers: transfers, filterByBlockRange: true}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	repo := newFakeDepositRepo()
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	var cursorsAfterEachCall []uint64
	for i := 0; i < 10; i++ {
		if repo.cursors["base/"+core.CursorTierObserved] >= latest {
			break
		}
		if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
			t.Fatalf("Execute() call %d error = %v, want nil", i+1, err)
		}
		cursorsAfterEachCall = append(cursorsAfterEachCall, repo.cursors["base/"+core.CursorTierObserved])
	}

	if got := repo.cursors["base/"+core.CursorTierObserved]; got != latest {
		t.Fatalf("final observed cursor = %d, want %d (full catch-up)", got, latest)
	}
	if len(scanner.scanRanges) != 3 {
		t.Fatalf("scan calls = %d, want exactly 3 to cover a %d-block backlog capped at %d blocks/poll", len(scanner.scanRanges), latest, maxBlocksPerScan)
	}

	// No gap, no overlap: each range starts exactly where the previous one ended, the
	// first starts at block 1, no range exceeds the cap, and the last ends at latest.
	wantFrom := uint64(1)
	for i, r := range scanner.scanRanges {
		from, to := r[0], r[1]
		if from != wantFrom {
			t.Fatalf("scan call %d fromBlock = %d, want %d (contiguous with the previous call's toBlock — no gap, no overlap)", i+1, from, wantFrom)
		}
		if to-from+1 > maxBlocksPerScan {
			t.Fatalf("scan call %d spans %d blocks, want <= %d (maxBlocksPerScan cap)", i+1, to-from+1, maxBlocksPerScan)
		}
		wantFrom = to + 1
	}
	if last := scanner.scanRanges[len(scanner.scanRanges)-1]; last[1] != latest {
		t.Fatalf("final scan call's toBlock = %d, want %d", last[1], latest)
	}

	// The cursor advanced by exactly maxBlocksPerScan on every call except the final
	// (partial) one.
	prevCursor := uint64(0)
	for i, c := range cursorsAfterEachCall {
		advanced := c - prevCursor
		isLast := i == len(cursorsAfterEachCall)-1
		if !isLast && advanced != maxBlocksPerScan {
			t.Fatalf("call %d advanced the cursor by %d, want exactly %d", i+1, advanced, maxBlocksPerScan)
		}
		if isLast && advanced > maxBlocksPerScan {
			t.Fatalf("final call advanced the cursor by %d, want <= %d", advanced, maxBlocksPerScan)
		}
		prevCursor = c
	}

	// Every configured transfer, spread across the whole backlog, was recorded exactly
	// once — no gaps, no duplicates, across the whole multi-call catch-up.
	if len(repo.inserted) != len(transfers) {
		t.Fatalf("inserted deposits = %d, want %d (every transfer across the whole backlog, exactly once)", len(repo.inserted), len(transfers))
	}
	counts := map[string]int{}
	for _, d := range repo.inserted {
		counts[d.TxHash]++
	}
	for _, tr := range transfers {
		if counts[tr.TxHash] != 1 {
			t.Fatalf("tx hash %q recorded %d times, want exactly 1", tr.TxHash, counts[tr.TxHash])
		}
	}
}

// midBatchTestTransfers backs both mid-batch-failure tests below with an identical scan
// result: three transfers in one poll's range, so a failure "on the Nth of several
// transfers" (the spec's own phrasing) has both a predecessor and a successor within the
// same batch.
func midBatchTestTransfers() []core.ObservedTransfer {
	return []core.ObservedTransfer{
		{Chain: core.ChainBase, Asset: core.AssetETH, Address: "0xAbC", TxHash: "0xa", LogIndex: -1, Amount: big.NewInt(1), BlockNumber: 10},
		{Chain: core.ChainBase, Asset: core.AssetETH, Address: "0xAbC", TxHash: "0xb", LogIndex: -1, Amount: big.NewInt(1), BlockNumber: 20},
		{Chain: core.ChainBase, Asset: core.AssetETH, Address: "0xAbC", TxHash: "0xc", LogIndex: -1, Amount: big.NewInt(1), BlockNumber: 30},
	}
}

// TestTrackDeposits_Execute_MidBatchScanFailureRollsBackCleanly proves AC3's rollback
// half: a poll that fails on the 2nd of 3 transfers in its scan result (a genuine
// mid-batch failure — it has both a predecessor already "recorded" this attempt and a
// successor never reached) must leave nothing committed at all: no deposit row, no
// cursor advance, and the transaction rolled back rather than committed.
func TestTrackDeposits_Execute_MidBatchScanFailureRollsBackCleanly(t *testing.T) {
	t.Parallel()

	transfers := midBatchTestTransfers()
	scanner := &fakeScanner{latest: 100, safe: 0, finalized: 0, transfers: transfers}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	repo := newFakeDepositRepo()
	repo.recordObservedFailAt = 2 // fail on the 2nd of 3 transfers: genuinely mid-batch
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{repo: repo}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	_, err := uc.Execute(context.Background(), core.ChainBase)
	if err == nil {
		t.Fatal("Execute() error = nil, want an error from the simulated mid-batch RecordObserved failure")
	}

	if !txb.lastTx.rolledBack {
		t.Fatal("expected the transaction to be rolled back")
	}
	if txb.lastTx.committed {
		t.Fatal("a failed poll must never commit")
	}

	// The real proof that restore() undid the one successful RecordObserved call before
	// the injected failure (re-review 2026-07-17, correcting this comment's earlier
	// claim): inserted/seen are the only fields here actually written before the failure
	// and therefore the only ones that depend on rollback/restore actually working.
	if len(repo.inserted) != 0 {
		t.Fatalf("inserted deposits after rollback = %d, want 0 (the deposit recorded before the failing call must not survive the rollback)", len(repo.inserted))
	}
	if len(repo.seen) != 0 {
		t.Fatalf("seen keys after rollback = %d, want 0", len(repo.seen))
	}
	// The cursor/promote/credit assertions below are true by construction of Execute's own
	// control flow, not because of restore(): SetCursor/PromoteToSafe/PromoteToFinalized/
	// CreditFinalizedDeposits are only ever reached after the RecordObserved loop
	// completes, and the injected failure happens inside that loop — so these fields are
	// never written in the first place this attempt, independent of whether rollback
	// works. Kept as a belt-and-suspenders check on Execute's phase ordering, not as
	// evidence of rollback.
	if got := repo.cursors["base/"+core.CursorTierObserved]; got != 0 {
		t.Fatalf("observed cursor after rollback = %d, want unchanged 0", got)
	}
	if got := repo.cursors["base/"+core.CursorTierSafe]; got != 0 {
		t.Fatalf("safe cursor after rollback = %d, want unchanged 0", got)
	}
	if got := repo.cursors["base/"+core.CursorTierFinalized]; got != 0 {
		t.Fatalf("finalized cursor after rollback = %d, want unchanged 0", got)
	}
	if repo.promoteCalls != 0 || repo.promoteFinalizedCalls != 0 || repo.creditCalls != 0 {
		t.Fatalf("promote/credit calls after rollback = (%d,%d,%d), want all 0 (the failure happens before Execute ever reaches those phases)", repo.promoteCalls, repo.promoteFinalizedCalls, repo.creditCalls)
	}
}

// TestTrackDeposits_Execute_RetryAfterMidBatchFailureRecoversFully proves AC3's recovery
// half concretely: following the exact mid-batch failure above, a retry against the same
// (unadvanced) cursor rescans the identical range and records every deposit from it
// exactly once — not just "returned no error."
func TestTrackDeposits_Execute_RetryAfterMidBatchFailureRecoversFully(t *testing.T) {
	t.Parallel()

	transfers := midBatchTestTransfers()
	scanner := &fakeScanner{latest: 100, safe: 0, finalized: 0, transfers: transfers}
	lister := &fakeAddressLister{addresses: []string{"0xAbC"}}
	tokenRegistryLister := &fakeTokenRegistryLister{registry: map[string]core.Asset{}}
	repo := newFakeDepositRepo()
	repo.recordObservedFailAt = 2
	unsupportedRepo := newFakeUnsupportedTokenRepo()
	txb := &fakeTxBeginner{repo: repo}
	uc := core.NewTrackDeposits(scanner, lister, tokenRegistryLister, repo, unsupportedRepo, txb)

	if _, err := uc.Execute(context.Background(), core.ChainBase); err == nil {
		t.Fatal("first Execute() error = nil, want the simulated mid-batch failure")
	}
	if len(repo.inserted) != 0 {
		t.Fatalf("after the failed attempt, inserted = %d, want 0", len(repo.inserted))
	}

	// Clear the failure condition — AC3's "the next tick simply retries" — and let the
	// retry rescan the exact same range: the observed cursor never advanced past the
	// failed attempt, so the next Execute call computes the identical [fromBlock,
	// toBlock].
	repo.recordObservedFailAt = 0
	if _, err := uc.Execute(context.Background(), core.ChainBase); err != nil {
		t.Fatalf("retry Execute() error = %v, want nil", err)
	}

	if scanner.gotFrom != 1 || scanner.gotTo != 100 {
		t.Fatalf("retried scan range = [%d,%d], want [1,100] (same range as the failed attempt, since the cursor never advanced)", scanner.gotFrom, scanner.gotTo)
	}
	if !txb.lastTx.committed {
		t.Fatal("expected the retry to commit")
	}
	if len(repo.inserted) != len(transfers) {
		t.Fatalf("after retry, inserted deposits = %d, want %d (every deposit from the retried range, exactly once)", len(repo.inserted), len(transfers))
	}
	counts := map[string]int{}
	for _, d := range repo.inserted {
		counts[d.TxHash]++
	}
	for _, tr := range transfers {
		if counts[tr.TxHash] != 1 {
			t.Fatalf("tx hash %q recorded %d times after retry, want exactly 1 (never skipped, never double-processed)", tr.TxHash, counts[tr.TxHash])
		}
	}
	if got := repo.cursors["base/"+core.CursorTierObserved]; got != 100 {
		t.Fatalf("observed cursor after retry = %d, want 100 (advanced now that the poll succeeded)", got)
	}
}
