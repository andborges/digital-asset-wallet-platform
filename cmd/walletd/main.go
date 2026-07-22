// Command walletd is the platform's single binary. It dispatches to a role
// subcommand — "api" and "watcher" exist as of Story 2.1; "broadcaster" is added by Story
// 3.4; recon/dispatcher are added by later epics. No CLI framework for this handful of
// subcommands (see Dev Notes).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	adapterapi "github.com/andborges/digital-asset-wallet-platform/internal/adapter/api"
	"github.com/andborges/digital-asset-wallet-platform/internal/adapter/evm"
	"github.com/andborges/digital-asset-wallet-platform/internal/adapter/postgres"
	"github.com/andborges/digital-asset-wallet-platform/internal/adapter/signer/kms"
	"github.com/andborges/digital-asset-wallet-platform/internal/adapter/signer/software"
	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// Server timeouts bound how long a single connection may tie up resources, closing off
// slow-client (Slowloris) exposure. shutdownGracePeriod bounds how long in-flight
// requests get to drain on SIGINT/SIGTERM before the process exits.
const (
	readHeaderTimeout   = 5 * time.Second
	readTimeout         = 15 * time.Second
	writeTimeout        = 30 * time.Second
	idleTimeout         = 60 * time.Second
	shutdownGracePeriod = 20 * time.Second
	// deployerCheckTimeout bounds the startup canonical-deployer RPC probe per chain.
	// ethclient dials lazily over HTTP, so without a deadline the eth_getCode call inherits
	// the deadline-less root context and a black-holed/stalled RPC endpoint would hang
	// startup forever — the opposite of AC3's "fail loudly". This turns a stall into a
	// timeout error that trips the os.Exit(1) path.
	deployerCheckTimeout = 10 * time.Second
	// defaultWatcherPollInterval is how often the watcher subcommand runs one
	// TrackDeposits.Execute poll cycle when WATCHER_POLL_INTERVAL is unset.
	defaultWatcherPollInterval = 5 * time.Second
	// defaultBroadcasterPollInterval is how often the broadcaster subcommand runs one
	// claim-and-advance-then-poll-receipts cycle when BROADCASTER_POLL_INTERVAL is unset.
	defaultBroadcasterPollInterval = 5 * time.Second
	// defaultWithdrawalStuckThreshold is how long a withdrawal may sit at
	// WithdrawalStatusBroadcast without confirming before DetectStuckWithdrawals writes its
	// one-time "withdrawal.stuck" outbox event, when WITHDRAWAL_STUCK_THRESHOLD is unset
	// (Story 3.5, Boundaries & Constraints).
	defaultWithdrawalStuckThreshold = 30 * time.Minute
	// defaultLivenessStalenessMultiplier is how liveness staleness threshold's own default is
	// derived from the watcher's own poll interval when LIVENESS_STALENESS_THRESHOLD is unset
	// (Story 3.5, Boundaries & Constraints: "default 3x WATCHER_POLL_INTERVAL").
	defaultLivenessStalenessMultiplier = 3
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: walletd <api|watcher|broadcaster>")
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	switch os.Args[1] {
	case "api":
		if err := runAPI(logger); err != nil {
			logger.Error("walletd api exited with error", "error", err)
			os.Exit(1)
		}
	case "watcher":
		if err := runWatcher(logger, os.Args[2:]); err != nil {
			logger.Error("walletd watcher exited with error", "error", err)
			os.Exit(1)
		}
	case "broadcaster":
		if err := runBroadcaster(logger, os.Args[2:]); err != nil {
			logger.Error("walletd broadcaster exited with error", "error", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q; usage: walletd <api|watcher|broadcaster>\n", os.Args[1])
		os.Exit(1)
	}
}

func runAPI(logger *slog.Logger) error {
	// The root context is cancelled on SIGINT/SIGTERM, which triggers graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	listenAddr := os.Getenv("API_LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":8080"
	}
	validTokens := splitNonEmpty(os.Getenv("API_BEARER_TOKENS"), ",")
	cursorSigningKey := []byte(os.Getenv("API_CURSOR_SIGNING_KEY"))
	if len(cursorSigningKey) == 0 {
		return fmt.Errorf("API_CURSOR_SIGNING_KEY is required")
	}
	baseRPCURL := os.Getenv("BASE_RPC_URL")
	if baseRPCURL == "" {
		return fmt.Errorf("BASE_RPC_URL is required")
	}
	arbitrumRPCURL := os.Getenv("ARBITRUM_RPC_URL")
	if arbitrumRPCURL == "" {
		return fmt.Errorf("ARBITRUM_RPC_URL is required")
	}
	// Expected chain IDs are operator-configured, not hardcoded: the same binary runs
	// against Sepolia testnets, mainnets, and local anvil (31337). The startup probe
	// verifies each RPC endpoint actually reports its expected id — without this, a
	// swapped or mis-pasted RPC URL passes the deployer-presence check silently, since
	// the canonical deployer is live on most EVM chains (re-review 2026-07-16).
	baseChainID, err := requiredChainIDEnv("BASE_CHAIN_ID")
	if err != nil {
		return err
	}
	arbitrumChainID, err := requiredChainIDEnv("ARBITRUM_CHAIN_ID")
	if err != nil {
		return err
	}
	chains := []evm.Chain{
		{Name: "base", RPCURL: baseRPCURL, ChainID: baseChainID},
		{Name: "arbitrum", RPCURL: arbitrumRPCURL, ChainID: arbitrumChainID},
	}

	// AC3: verify the canonical CREATE2 deployer is present on every configured chain
	// before serving anything — fail startup loudly rather than risk deriving or
	// verifying deposit addresses against a chain where CREATE2 addresses could diverge.
	// Each probe is bounded by deployerCheckTimeout so an unreachable/stalled RPC endpoint
	// surfaces as a timeout error instead of hanging startup indefinitely.
	for _, chain := range chains {
		if err := func() error {
			checkCtx, cancel := context.WithTimeout(ctx, deployerCheckTimeout)
			defer cancel()
			return evm.VerifyDeployerPresence(checkCtx, chain)
		}(); err != nil {
			return fmt.Errorf("verify canonical CREATE2 deployer presence: %w", err)
		}
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	if err := postgres.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	// Composition root: the only place that imports both internal/adapter/api and
	// internal/adapter/postgres. Neither adapter package imports the other (AD-1, AD-2).
	txBeginner := postgres.NewTxBeginner(pool)
	idempotencyStore := postgres.NewIdempotencyStore(pool)
	customerRepo := postgres.NewCustomerRepository()
	addressDeriver := evm.NewDepositAddressDeriver()
	createCustomer := core.NewCreateCustomer(customerRepo, addressDeriver)
	customerReader := postgres.NewCustomerReader(pool)
	getCustomer := core.NewGetCustomer(customerReader)
	balanceRepo := postgres.NewBalanceRepository(pool)
	getBalances := core.NewGetCustomerBalances(balanceRepo)
	transferRepo := postgres.NewTransferRepository()
	createTransfer := core.NewCreateTransfer(transferRepo)
	transactionRepo := postgres.NewTransactionRepository(pool, cursorSigningKey)
	listTransactions := core.NewListCustomerTransactions(transactionRepo)
	depositReader := postgres.NewDepositReader(pool)
	getDeposits := core.NewGetCustomerDeposits(depositReader)
	unsupportedTokenRepo := postgres.NewUnsupportedTokenRepository(pool)
	listUnsupportedTokenObservations := core.NewListUnsupportedTokenObservations(unsupportedTokenRepo)
	tokenRegistry := postgres.NewTokenRegistry(pool)
	feeEstimator, err := evm.NewFeeEstimator(ctx, chains, tokenRegistry)
	if err != nil {
		return fmt.Errorf("connect fee estimator RPC clients: %w", err)
	}
	defer feeEstimator.Close()
	estimateFee := core.NewEstimateFee(feeEstimator)
	withdrawalRepo := postgres.NewWithdrawalRepository()
	withdrawalThresholdLister := postgres.NewWithdrawalThresholdLister(pool)
	createWithdrawal := core.NewCreateWithdrawal(withdrawalRepo, feeEstimator, withdrawalThresholdLister)
	approveWithdrawal := core.NewApproveWithdrawal(withdrawalRepo)

	serverImpl := adapterapi.NewServerInterface(createCustomer, getCustomer, getBalances, createTransfer, listTransactions, getDeposits, listUnsupportedTokenObservations, estimateFee, createWithdrawal, approveWithdrawal, logger)
	mux := http.NewServeMux()
	handler := adapterapi.HandlerWithOptions(serverImpl, adapterapi.StdHTTPServerOptions{
		BaseRouter: mux,
		BaseURL:    "/v1",
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			adapterapi.WriteProblem(w, http.StatusBadRequest, "invalid-request", err.Error(), r.URL.Path)
		},
	})
	handler = adapterapi.IdempotencyMiddleware(txBeginner, idempotencyStore)(handler)
	handler = adapterapi.AuthMiddleware(validTokens)(handler)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("walletd api listening", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining in-flight requests", "grace", shutdownGracePeriod.String())
		stop() // stop intercepting signals; a second signal now terminates immediately
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown failed: %w", err)
		}
		logger.Info("walletd api stopped cleanly")
		return nil
	}
}

// runWatcher runs the watcher role for exactly one configured chain (AD-2: one OS
// process per chain, never one process looping both) — chain is selected by the
// required --chain=base|arbitrum flag, which also selects which {CHAIN}_* environment
// variables this process reads.
func runWatcher(logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("watcher", flag.ContinueOnError)
	chainName := fs.String("chain", "", "chain to watch: base|arbitrum")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse watcher flags: %w", err)
	}

	var envPrefix string
	switch *chainName {
	case "base":
		envPrefix = "BASE"
	case "arbitrum":
		envPrefix = "ARBITRUM"
	default:
		return fmt.Errorf("--chain must be %q or %q, got %q", "base", "arbitrum", *chainName)
	}

	// The root context is cancelled on SIGINT/SIGTERM, which stops the poll loop below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	rpcURL, err := requiredStringEnv(envPrefix + "_RPC_URL")
	if err != nil {
		return err
	}
	chainID, err := requiredChainIDEnv(envPrefix + "_CHAIN_ID")
	if err != nil {
		return err
	}
	usdcAddress, err := requiredStringEnv(envPrefix + "_USDC_ADDRESS")
	if err != nil {
		return err
	}
	// Validated beyond mere non-emptiness (re-review 2026-07-16): .env.example ships a
	// zero-address placeholder that MUST be replaced per-environment. Without this check,
	// an operator who forgets to fill it in gets a watcher that starts cleanly, logs
	// success every poll, and silently never detects a single USDC deposit — the same
	// fail-loud discipline VerifyDeployerPresence already applies to a bad RPC endpoint.
	if !evm.IsChecksummedAddress(usdcAddress) {
		return fmt.Errorf("%s_USDC_ADDRESS must be a well-formed, EIP-55-checksummed address, got %q", envPrefix, usdcAddress)
	}
	if usdcAddress == zeroAddress {
		return fmt.Errorf("%s_USDC_ADDRESS is still the .env.example placeholder zero address — set it to the real USDC contract address for this chain/environment", envPrefix)
	}

	pollInterval := defaultWatcherPollInterval
	if v := os.Getenv("WATCHER_POLL_INTERVAL"); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("WATCHER_POLL_INTERVAL must be a valid duration (e.g. \"5s\"), got %q: %w", v, err)
		}
		if parsed <= 0 {
			return fmt.Errorf("WATCHER_POLL_INTERVAL must be a positive duration, got %q", v)
		}
		pollInterval = parsed
	}

	chain := evm.Chain{Name: *chainName, RPCURL: rpcURL, ChainID: chainID}

	// Reuse Story 1.5's startup check: refuse to poll a chain whose RPC endpoint isn't
	// verifiably the chain it claims to be, or where the canonical CREATE2 deployer
	// (and therefore every customer's counterfactual deposit address) can't be trusted.
	if err := func() error {
		checkCtx, cancel := context.WithTimeout(ctx, deployerCheckTimeout)
		defer cancel()
		return evm.VerifyDeployerPresence(checkCtx, chain)
	}(); err != nil {
		return fmt.Errorf("verify canonical CREATE2 deployer presence: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	// AD-2 says exactly one watcher OS process runs per chain, but nothing enforced that
	// before this lock (re-review 2026-07-16): an accidental double-start (overlapping
	// rolling deploy, operator error) would have both instances racing SetCursor, able to
	// regress the persisted cursor. A session-held Postgres advisory lock, scoped to this
	// chain, makes a second instance for the same chain fail to start instead of racing.
	// The dedicated connection is held for this process's entire lifetime and released
	// only at shutdown, right before pool.Close() above runs.
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire postgres connection for watcher lock: %w", err)
	}
	defer lockConn.Release()

	var lockAcquired bool
	if err := lockConn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", watcherLockID(*chainName)).Scan(&lockAcquired); err != nil {
		return fmt.Errorf("acquire watcher advisory lock for chain %q: %w", *chainName, err)
	}
	if !lockAcquired {
		return fmt.Errorf("another watcher process already holds the advisory lock for chain %q (AD-2: exactly one watcher process per chain)", *chainName)
	}

	if err := postgres.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	// Story 2.3: the registry is populated by this watcher process itself at startup, not
	// by a migration — contract addresses are environment-specific (mainnet vs testnet vs
	// local anvil), which a migration applied once at deploy time can't read the way this
	// already-validated *_USDC_ADDRESS env var can. This upsert runs once, before the poll
	// loop, never as part of any per-poll transaction; a restart re-upserts the same row,
	// keeping the registry in sync with configuration, while an operator's own separately
	// inserted row for a genuinely new ERC-20 is left untouched (FR34).
	tokenRegistry := postgres.NewTokenRegistry(pool)
	if err := tokenRegistry.UpsertToken(ctx, *chainName, usdcAddress, string(core.AssetUSDC)); err != nil {
		return fmt.Errorf("upsert configured USDC address into token registry: %w", err)
	}

	scanner, err := evm.NewScanner(ctx, chain, logger)
	if err != nil {
		return fmt.Errorf("connect chain scanner: %w", err)
	}
	defer scanner.Close()

	// Composition root, same as runAPI: the only place that imports both
	// internal/adapter/evm and internal/adapter/postgres for the watcher role.
	txBeginner := postgres.NewTxBeginner(pool)
	addressLister := postgres.NewDepositAddressLister(pool)
	depositRepo := postgres.NewDepositRepository()
	unsupportedTokenRepo := postgres.NewUnsupportedTokenRepository(pool)
	trackDeposits := core.NewTrackDeposits(scanner, addressLister, tokenRegistry, depositRepo, unsupportedTokenRepo, txBeginner)

	coreChain := core.Chain(*chainName)

	logger.Info("walletd watcher starting", "chain", *chainName, "pollInterval", pollInterval.String())

	// Story 2.5: report each tier's resumed cursor position before the poll loop starts,
	// so a restart's recovery point is operator-visible in the logs rather than silent —
	// the same DepositRepository instance constructed above, no new port or query.
	// DepositRepository carries no pool of its own (AD-4): every method, including this
	// read-only Cursor call, requires a transaction on ctx (txFromContext panics
	// otherwise, confirmed by actually running this code path — cmd/walletd has no
	// automated test coverage, so nothing else would have caught it). A short-lived
	// transaction, rolled back immediately since nothing is written, is the correct way
	// to satisfy that for a pure read here.
	//
	// This whole block is best-effort (re-review 2026-07-17): it exists purely for
	// operator visibility, so it must never be able to prevent the watcher from starting.
	// Any error — including a shutdown signal landing in this narrow window, which would
	// otherwise surface as a spurious fatal exit instead of the graceful-shutdown path the
	// poll loop uses for the identical signal moments later — is logged as a warning, not
	// returned. A recover() guards against a future regression reintroducing the original
	// panic (calling Cursor outside a transaction) turning back into a fatal crash instead
	// of a logged warning.
	func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("recovered panic while logging resumed cursors at startup (continuing regardless)", "chain", *chainName, "panic", r)
			}
		}()

		startupCtx, startupTx, err := txBeginner.Begin(ctx)
		if err != nil {
			logger.Warn("failed to begin startup cursor-read transaction (continuing regardless)", "chain", *chainName, "error", err)
			return
		}
		defer func() {
			if err := startupTx.Rollback(context.WithoutCancel(ctx)); err != nil {
				logger.Warn("failed to roll back startup cursor-read transaction", "chain", *chainName, "error", err)
			}
		}()

		observedCursor, err := depositRepo.Cursor(startupCtx, coreChain, core.CursorTierObserved)
		if err != nil {
			logger.Warn("failed to read observed cursor at startup (continuing regardless)", "chain", *chainName, "error", err)
			return
		}
		safeCursor, err := depositRepo.Cursor(startupCtx, coreChain, core.CursorTierSafe)
		if err != nil {
			logger.Warn("failed to read safe cursor at startup (continuing regardless)", "chain", *chainName, "error", err)
			return
		}
		finalizedCursor, err := depositRepo.Cursor(startupCtx, coreChain, core.CursorTierFinalized)
		if err != nil {
			logger.Warn("failed to read finalized cursor at startup (continuing regardless)", "chain", *chainName, "error", err)
			return
		}
		logger.Info("walletd watcher resuming from persisted cursors",
			"chain", *chainName,
			"observedCursor", observedCursor,
			"safeCursor", safeCursor,
			"finalizedCursor", finalizedCursor,
		)
	}()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Poll once immediately, then on every tick, until SIGINT/SIGTERM. A single poll's
	// error is logged, not fatal — a transient RPC or DB hiccup should not kill the
	// whole watcher process; the next tick simply retries the same block range (the
	// cursor never advanced past the failed poll).
	for {
		logger.Info("watcher poll started", "chain", *chainName)
		result, err := trackDeposits.Execute(ctx, coreChain)
		if err != nil {
			logger.Error("watcher poll failed", "chain", *chainName, "error", err)
		} else {
			logger.Info("watcher poll completed",
				"chain", *chainName,
				"latest", result.Latest,
				"safe", result.Safe,
				"finalized", result.Finalized,
				"scannedFrom", result.ScannedFrom,
				"scannedTo", result.ScannedTo,
				"depositsObserved", result.DepositsObserved,
				"unsupportedObserved", result.UnsupportedObserved,
				"promotedToSafe", result.PromotedToSafe,
				"promotedToFinalized", result.PromotedToFinalized,
				"credited", result.Credited,
			)
		}

		select {
		case <-ctx.Done():
			logger.Info("shutdown signal received, walletd watcher stopping")
			return nil
		case <-ticker.C:
		}
	}
}

// runBroadcaster runs the broadcaster role for exactly one configured chain (Story 3.4,
// AD-11: one broadcaster process per chain, mirroring runWatcher's AD-2 discipline exactly)
// — chain is selected by the required --chain=base|arbitrum flag, which also selects which
// {CHAIN}_* environment variables this process reads. Mirrors runWatcher's own flag/lock/
// migrate/poll-loop shape exactly, per the story's own explicit instruction.
func runBroadcaster(logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("broadcaster", flag.ContinueOnError)
	chainName := fs.String("chain", "", "chain to broadcast withdrawals for: base|arbitrum")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse broadcaster flags: %w", err)
	}

	var envPrefix string
	switch *chainName {
	case "base":
		envPrefix = "BASE"
	case "arbitrum":
		envPrefix = "ARBITRUM"
	default:
		return fmt.Errorf("--chain must be %q or %q, got %q", "base", "arbitrum", *chainName)
	}

	// The root context is cancelled on SIGINT/SIGTERM, which stops the poll loop below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	rpcURL, err := requiredStringEnv(envPrefix + "_RPC_URL")
	if err != nil {
		return err
	}
	chainID, err := requiredChainIDEnv(envPrefix + "_CHAIN_ID")
	if err != nil {
		return err
	}

	pollInterval := defaultBroadcasterPollInterval
	if v := os.Getenv("BROADCASTER_POLL_INTERVAL"); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("BROADCASTER_POLL_INTERVAL must be a valid duration (e.g. \"5s\"), got %q: %w", v, err)
		}
		if parsed <= 0 {
			return fmt.Errorf("BROADCASTER_POLL_INTERVAL must be a positive duration, got %q", v)
		}
		pollInterval = parsed
	}

	// Story 3.5: WITHDRAWAL_STUCK_THRESHOLD, mirroring BROADCASTER_POLL_INTERVAL's exact
	// parsing/defaulting pattern above.
	stuckThreshold := defaultWithdrawalStuckThreshold
	if v := os.Getenv("WITHDRAWAL_STUCK_THRESHOLD"); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("WITHDRAWAL_STUCK_THRESHOLD must be a valid duration (e.g. \"30m\"), got %q: %w", v, err)
		}
		if parsed <= 0 {
			return fmt.Errorf("WITHDRAWAL_STUCK_THRESHOLD must be a positive duration, got %q", v)
		}
		stuckThreshold = parsed
	}

	// Story 3.5: LIVENESS_STALENESS_THRESHOLD, same parsing/defaulting pattern — its own
	// default (Boundaries & Constraints: "default 3x WATCHER_POLL_INTERVAL") is derived from
	// WATCHER_POLL_INTERVAL if this process's own environment happens to have it set (shared
	// .env files commonly do, even though this is a broadcaster, not a watcher, process),
	// falling back to defaultWatcherPollInterval otherwise — mirroring runWatcher's own
	// WATCHER_POLL_INTERVAL parsing exactly, just read here instead of defaulted-and-applied.
	watcherPollIntervalForDefault := defaultWatcherPollInterval
	if v := os.Getenv("WATCHER_POLL_INTERVAL"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			watcherPollIntervalForDefault = parsed
		}
	}
	livenessStalenessThreshold := defaultLivenessStalenessMultiplier * watcherPollIntervalForDefault
	if v := os.Getenv("LIVENESS_STALENESS_THRESHOLD"); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("LIVENESS_STALENESS_THRESHOLD must be a valid duration (e.g. \"15s\"), got %q: %w", v, err)
		}
		if parsed <= 0 {
			return fmt.Errorf("LIVENESS_STALENESS_THRESHOLD must be a positive duration, got %q", v)
		}
		livenessStalenessThreshold = parsed
	}

	chain := evm.Chain{Name: *chainName, RPCURL: rpcURL, ChainID: chainID}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	// AD-11 says exactly one broadcaster OS process runs per chain — enforced the same way
	// runWatcher enforces AD-2's identical rule (watcherLockID's own doc comment): a
	// session-held Postgres advisory lock, scoped to this chain, keyed in a NUMERIC
	// NAMESPACE DISTINCT from watcherLockID's (890_200_00x here vs. 890_100_00x there,
	// broadcasterLockID's own doc comment) so the two locks for the same chain never
	// collide — the watcher and the broadcaster are independent roles that must be able to
	// run concurrently against the same chain. The dedicated connection is held for this
	// process's entire lifetime and released only at shutdown, right before pool.Close()
	// above runs.
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire postgres connection for broadcaster lock: %w", err)
	}
	defer lockConn.Release()

	var lockAcquired bool
	if err := lockConn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", broadcasterLockID(*chainName)).Scan(&lockAcquired); err != nil {
		return fmt.Errorf("acquire broadcaster advisory lock for chain %q: %w", *chainName, err)
	}
	if !lockAcquired {
		return fmt.Errorf("another broadcaster process already holds the advisory lock for chain %q (AD-11: exactly one broadcaster process per chain)", *chainName)
	}

	if err := postgres.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	tokenRegistry := postgres.NewTokenRegistry(pool)

	transactionBroadcaster, err := evm.NewBroadcaster(ctx, chain, tokenRegistry)
	if err != nil {
		return fmt.Errorf("connect transaction broadcaster: %w", err)
	}
	defer transactionBroadcaster.Close()

	// Composition root: wires SIGNER_BACKEND=kms|software to pick the Signer
	// implementation (Code Map) — the only place that decides which one this process runs
	// with. NFR13: neither branch below ever logs anything derived from key material,
	// only (at most, via each Signer's own Address() method) a public Ethereum address.
	// SIGNER_BACKEND has no default (re-review 2026-07-21): the software signer's own
	// package doc says plainly "dev/test only," so silently defaulting to it here — as an
	// earlier version of this function did — means an operator who simply forgets to set
	// SIGNER_BACKEND=kms in production gets no error at all, only a real hot-wallet key
	// signed by a plaintext in-memory key instead of AWS KMS. Every other required
	// configuration value in this function (DATABASE_URL, RPC URLs, SIGNER_PRIVATE_KEY,
	// SIGNER_KMS_KEY_ID) already fails loud via requiredStringEnv when missing; this is the
	// one that most needs to, since it decides the custody backend.
	signerBackend, err := requiredStringEnv("SIGNER_BACKEND")
	if err != nil {
		return err
	}
	var withdrawalSigner core.Signer
	var signerAddress string
	switch signerBackend {
	case "software":
		privateKeyHex, err := requiredStringEnv("SIGNER_PRIVATE_KEY")
		if err != nil {
			return err
		}
		softwareSigner, err := software.NewSigner(privateKeyHex)
		if err != nil {
			return fmt.Errorf("construct software signer: %w", err)
		}
		withdrawalSigner, signerAddress = softwareSigner, softwareSigner.Address()
	case "kms":
		kmsKeyID, err := requiredStringEnv("SIGNER_KMS_KEY_ID")
		if err != nil {
			return err
		}
		kmsSigner, err := kms.NewSigner(ctx, kmsKeyID)
		if err != nil {
			return fmt.Errorf("construct KMS signer: %w", err)
		}
		withdrawalSigner, signerAddress = kmsSigner, kmsSigner.Address()
	default:
		return fmt.Errorf("SIGNER_BACKEND must be %q or %q, got %q", "software", "kms", signerBackend)
	}

	// Composition root, same as runAPI/runWatcher: the only place that imports
	// internal/adapter/evm, internal/adapter/signer/{software,kms}, and
	// internal/adapter/postgres together for the broadcaster role.
	txBeginner := postgres.NewTxBeginner(pool)
	withdrawalRepo := postgres.NewWithdrawalRepository()
	signAndBroadcast := core.NewSignAndBroadcastWithdrawal(withdrawalRepo, withdrawalSigner, transactionBroadcaster, txBeginner)
	pollReceipts := core.NewPollWithdrawalReceipts(withdrawalRepo, transactionBroadcaster, txBeginner)
	detectStuck := core.NewDetectStuckWithdrawals(withdrawalRepo, txBeginner)
	watcherLiveness := postgres.NewWatcherLiveness(pool)

	coreChain := core.Chain(*chainName)

	logger.Info("walletd broadcaster starting",
		"chain", *chainName,
		"pollInterval", pollInterval.String(),
		"signerBackend", signerBackend,
		"signerAddress", signerAddress,
		"withdrawalStuckThreshold", stuckThreshold.String(),
		"livenessStalenessThreshold", livenessStalenessThreshold.String(),
	)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// lastLive tracks the previous tick's liveness determination so transitions are logged
	// at most once, not every tick (Boundaries & Constraints) — nil means "not yet
	// determined," which forces the very first tick's outcome to be logged too, then only
	// actual flips from there on.
	var lastLive *bool

	// Poll once immediately, then on every tick, until SIGINT/SIGTERM — mirrors
	// runWatcher's identical loop shape. Each tick: (1) checks the watcher's own cursor
	// liveness (Story 3.5, AD-15) — a stale cursor skips claiming/resuming new work THIS
	// tick entirely (no new nonce allocated, no new withdrawal stranded) but never blocks
	// the two steps below, which always run regardless; (2) checks every broadcast
	// withdrawal's receipt once; (3) runs one stuck-detection pass. A single claim/build/
	// sign/broadcast failure is logged and stops the inner drain loop for this tick (the
	// failed withdrawal is left at WithdrawalStatusSigned, resumed automatically next tick)
	// — never a fatal process crash.
	for {
		live, err := watcherLiveness.IsLive(ctx, coreChain, livenessStalenessThreshold)
		if err != nil {
			// A liveness-check failure (e.g. a transient DB hiccup) is treated as "not live"
			// for this tick — fail safe: better to pause new claims for one tick on a false
			// alarm than to keep claiming through an outage we simply couldn't observe.
			logger.Error("broadcaster liveness check failed; treating watcher as not live this tick", "chain", *chainName, "error", err)
			live = false
		}
		if lastLive == nil || *lastLive != live {
			if live {
				logger.Info("watcher cursor liveness recovered — resuming withdrawal claims", "chain", *chainName)
			} else {
				logger.Warn("watcher cursor is stale — pausing new withdrawal claims/resends this tick (receipt polling and stuck detection still run)",
					"chain", *chainName, "stalenessThreshold", livenessStalenessThreshold.String())
			}
			liveCopy := live
			lastLive = &liveCopy
		}

		// live gates only the fallback to ClaimApprovedWithdrawal inside Execute (allowClaim),
		// never resuming an already-signed withdrawal (re-review 2026-07-22: the original
		// version wrapped this entire loop in `if live`, so a stale watcher cursor also
		// paused resending an already-signed withdrawal's persisted bytes — work that
		// allocates no new nonce and strands nothing new, the exact risk the liveness gate
		// exists to prevent). This loop therefore always runs, at minimum checking whether
		// anything is left to resume, even while live is false.
		claimedCount := 0
		for {
			claimed, err := signAndBroadcast.Execute(ctx, coreChain, live)
			if err != nil {
				logger.Error("broadcaster sign-and-broadcast failed", "chain", *chainName, "error", err)
				break
			}
			if !claimed {
				break
			}
			claimedCount++
		}

		result, err := pollReceipts.Execute(ctx, coreChain)
		if err != nil {
			logger.Error("broadcaster poll-receipts failed", "chain", *chainName, "error", err)
		}

		stuckResult, err := detectStuck.Execute(ctx, coreChain, stuckThreshold)
		if err != nil {
			logger.Error("broadcaster stuck-detection failed", "chain", *chainName, "error", err)
		}

		logger.Info("broadcaster poll completed",
			"chain", *chainName,
			"live", live,
			"claimed", claimedCount,
			"receiptsChecked", result.Checked,
			"confirmed", result.Confirmed,
			"failed", result.Failed,
			"stuckCandidatesChecked", stuckResult.Checked,
			"stuckAlerted", stuckResult.Alerted,
		)

		select {
		case <-ctx.Done():
			logger.Info("shutdown signal received, walletd broadcaster stopping")
			return nil
		case <-ticker.C:
		}
	}
}

// broadcasterLockID returns the fixed Postgres advisory-lock key for chainName's
// broadcaster — a distinct numeric namespace (890_200_00x) from watcherLockID's
// (890_100_00x) so the two locks for the same chain never collide: the watcher and the
// broadcaster are independent roles (AD-2 vs. AD-11) that must both be able to hold their
// own lock for the same chain at the same time.
func broadcasterLockID(chainName string) int64 {
	switch chainName {
	case "base":
		return 890_200_001
	case "arbitrum":
		return 890_200_002
	default:
		// Unreachable: runBroadcaster already validated chainName is "base" or "arbitrum"
		// before this is ever called.
		panic(fmt.Sprintf("broadcasterLockID: unknown chain %q", chainName))
	}
}

// requiredStringEnv reads a required, non-empty environment variable.
func requiredStringEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}

// zeroAddress is the placeholder shipped in .env.example for *_USDC_ADDRESS — a real
// USDC contract address must never actually be the zero address.
const zeroAddress = "0x0000000000000000000000000000000000000000"

// watcherLockID returns the fixed Postgres advisory-lock key for chainName's watcher.
// Exactly two chains exist in v1, so a direct mapping is clearer than hashing the name.
func watcherLockID(chainName string) int64 {
	switch chainName {
	case "base":
		return 890_100_001
	case "arbitrum":
		return 890_100_002
	default:
		// Unreachable: runWatcher already validated chainName is "base" or "arbitrum"
		// before this is ever called.
		panic(fmt.Sprintf("watcherLockID: unknown chain %q", chainName))
	}
}

// requiredChainIDEnv reads a required EIP-155 chain id from the environment as a
// positive decimal integer.
func requiredChainIDEnv(name string) (uint64, error) {
	v := os.Getenv(name)
	if v == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	id, err := strconv.ParseUint(v, 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("%s must be a positive decimal EIP-155 chain id, got %q", name, v)
	}
	return id, nil
}

func splitNonEmpty(s, sep string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
