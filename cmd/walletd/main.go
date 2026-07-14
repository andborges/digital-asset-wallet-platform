// Command walletd is the platform's single binary. It dispatches to a role
// subcommand — only "api" exists in this story; watcher/broadcaster/recon/dispatcher
// are added by later epics. No CLI framework for one subcommand (see Dev Notes).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	adapterapi "github.com/andborges/digital-asset-wallet-platform/internal/adapter/api"
	"github.com/andborges/digital-asset-wallet-platform/internal/adapter/postgres"
	"github.com/andborges/digital-asset-wallet-platform/internal/core"
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
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	listenAddr := os.Getenv("API_LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":8080"
	}
	validTokens := splitNonEmpty(os.Getenv("API_BEARER_TOKENS"), ",")

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
	createCustomer := core.NewCreateCustomer(customerRepo)

	serverImpl := adapterapi.NewServerInterface(createCustomer)
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

	logger.Info("walletd api listening", "addr", listenAddr)
	return http.ListenAndServe(listenAddr, handler)
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
