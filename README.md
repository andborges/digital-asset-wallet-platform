# Digital Assets Wallet Platform

A backend platform that credits customers when crypto arrives and pays out on request — the **ledger side** of wallet infrastructure: deposit monitoring across L2 confirmation tiers, reorg handling, idempotent transaction state machines, and independent reconciliation. No vendor or mature open-source project covers this layer; it's the part every company in this position ends up building by hand.

This is a portfolio project run like a product: real requirements, a real architecture, tests, and (eventually) a reviewed threat model — not a toy. The stated bet is that there's no technical moat here — rigor is the product.

**Status: early, active development.** V1 scope and full requirements are documented (see [Documentation](#documentation) below); only the foundation (customer accounts + provisioning) is implemented so far. Don't expect deposits or withdrawals to work yet — see the [sprint status](_bmad-output/implementation-artifacts/sprint-status.yaml) for what's actually done.

## What it does (v1 scope)

- Deposits and withdrawals of native ETH and USDC on **Base** and **Arbitrum**
- One deposit address per customer, reused across every supported EVM chain
- Deposit crediting gated on L1 finality, safe under reorgs and sequencer reordering
- Idempotent APIs — retries and crashes never double-apply a money movement
- Withdrawal approval workflow above a configurable threshold
- Independent, continuous reconciliation between the ledger and on-chain reality
- Webhook notifications for deposit/withdrawal/approval/reconciliation events
- Platform-held keys (AWS KMS in production; LocalStack locally) — no third-party signer

Explicitly **out of scope for v1**: non-EVM chains, consumer-facing UI, staking/swaps/DeFi, compliance tooling, multi-tenancy.

## Architecture

Hexagonal (ports & adapters): a domain core that depends on nothing, with adapters for the REST API, PostgreSQL, EVM chains, signing, and webhook delivery. One Go binary, `walletd`, runs as separate OS processes per role (`api`, `watcher`, `broadcaster`, `recon`, `dispatcher`) that coordinate only through PostgreSQL — no message broker, no shared memory.

Balances are never stored directly; they're derived from a double-entry journal of balanced postings, so the ledger is always explainable against the chain. Every mutating API call requires bearer authentication and an idempotency key, enforced at the database level (not application-level checks) so retries and concurrent duplicates are structurally impossible to double-apply.

The full rationale — every load-bearing decision, the alternatives that were rejected, and why — is in the [Solution Design](_bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/SOLUTION-DESIGN.md); the terse, enforceable rule set is the [Architecture Spine](_bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/ARCHITECTURE-SPINE.md).

## Tech stack

| Layer | Choice |
| --- | --- |
| Language | Go 1.26 |
| Database | PostgreSQL 18 (`jackc/pgx/v5`, `pressly/goose/v3` migrations) |
| API | REST, spec-first OpenAPI (`oapi-codegen` v2, stdlib `net/http` `ServeMux`) |
| Chains | Base, Arbitrum (`go-ethereum`) |
| Contracts | Solidity + Foundry (CREATE2 deposit-address forwarders) |
| Custody | AWS KMS in production; LocalStack locally |
| Deployment | Docker Compose — same stack in test, local dev, and production |

## Running it locally

You need [Docker](https://docs.docker.com/get-docker/) and [Go 1.26+](https://go.dev/dl/).

```bash
make up    # start Postgres in Docker — the only thing containerized for local dev
make run   # creates .env from .env.example on first run, then runs the API on your machine
```

Migrations run automatically on startup. Then try it:

```bash
curl -X POST http://localhost:8080/v1/customers \
  -H "Authorization: Bearer dev-token" \
  -H "Idempotency-Key: my-first-request" \
  -d '{}'
```

You should get back a `201` with a customer ID and four provisioned accounts (ETH and USDC, on Base and Arbitrum). Stop Postgres when you're done: `make down`.

Run `make help` for every available target. If you'd rather not use `make`, everything it wraps is a plain `go`/`docker compose` command — see the [Makefile](Makefile).

## Running the tests

```bash
make test        # full suite, including the real-Postgres integration test
make test-unit   # fast tests only, no Docker required
make lint        # go vet + gofmt check
```

The integration tests spin up a real PostgreSQL container via [testcontainers-go](https://golang.testcontainers.org/) — no mocked repositories for correctness-critical paths. Docker must be running for `make test`.

## Project structure

```text
cmd/walletd/            # single binary, role subcommands (only "api" exists so far)
internal/core/          # domain: ledger, state machines, ports — depends on nothing
internal/adapter/
  api/                  # REST handlers generated from api/openapi.yaml
  postgres/             # Postgres repositories + goose migrations
api/openapi.yaml        # the API contract; source of truth for generated code
deploy/compose/         # Docker Compose stack + Dockerfile
```

## Documentation

This project treats its planning artifacts as part of the deliverable, not throwaway scaffolding:

- [Product Requirements](_bmad-output/planning-artifacts/prds/prd-digital-asset-wallet-platform-2026-07-13/prd.md) — the four hard problems this platform exists to solve, full functional/non-functional requirements, and success metrics
- [Architecture Spine](_bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/ARCHITECTURE-SPINE.md) — the enforceable rules every piece of code must follow
- [Solution Design](_bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/SOLUTION-DESIGN.md) — the reasoning and rejected alternatives behind those rules
- [Epics & Stories](_bmad-output/planning-artifacts/epics.md) — the full implementation breakdown
- [Sprint Status](_bmad-output/implementation-artifacts/sprint-status.yaml) — what's actually done right now
