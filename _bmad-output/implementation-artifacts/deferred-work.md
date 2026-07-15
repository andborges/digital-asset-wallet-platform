# Deferred Work

Items deferred during reviews — real but not actionable at the time they were found.

## Deferred from: code review of 1-2-query-customer-balances (2026-07-14)

- **Raw internal error text leaked in problem+json `detail` on 500** [internal/adapter/api/customers.go:66] — `WriteProblem(..., err.Error(), ...)` forwards wrapped DB error text (query structure, schema names, SQL state) to clients. Pre-existing project-wide pattern (`CreateCustomer`'s 500 branch does the same); `problem.go` already warns against sensitive `Detail`. Fix globally: log the detail server-side and return a generic client-facing message.
- **No object-level authorization on per-customer reads** [internal/adapter/api/customers.go:59] — the auth model is a static shared-token allowlist with no notion of caller identity or customer ownership, so any valid token can read any customer's balances by id. This is the first read endpoint to expose per-customer financial state through that model. Likely by-design for the B2B "application team" caller, but the trust model should be explicitly confirmed before any end-user-facing exposure.
- **Existence check + balance SUM run as two separate pool queries outside a transaction (TOCTOU)** [internal/adapter/postgres/balance_repo.go:36-47] — benign today (read-only, no delete path, no posting writer), but once Story 1.3 adds concurrent writes, consider folding existence into a single round-trip (CTE / RIGHT JOIN) for a consistent snapshot.
- **Negative derived balance surfaced with no guard** [internal/adapter/postgres/balance_repo.go:48] — `postings.amount` is signed `NUMERIC(78,0)`, so `SUM` can go negative and the read path emits it verbatim. No writer exists until Story 1.3; decide on a non-negative floor (read-path guard and/or DB CHECK) before rows are written.
