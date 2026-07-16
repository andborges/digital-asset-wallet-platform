// Command walletd is the platform's single binary. It dispatches to a role
// subcommand — only "api" exists in this story; watcher/broadcaster/recon/dispatcher
// are added by later epics. No CLI framework for one subcommand (see Dev Notes).
package main

import (
	"context"
	"errors"
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
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: walletd <api>")
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	switch os.Args[1] {
	case "api":
		if err := runAPI(logger); err != nil {
			logger.Error("walletd api exited with error", "error", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q; usage: walletd <api>\n", os.Args[1])
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

	serverImpl := adapterapi.NewServerInterface(createCustomer, getCustomer, getBalances, createTransfer, listTransactions)
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
