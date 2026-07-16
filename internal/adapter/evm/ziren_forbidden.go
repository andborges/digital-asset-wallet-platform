//go:build ziren

package evm

// Compile-time canary (Story 1.5 re-review, 2026-07-16): building anything in this module
// with `-tags ziren` would swap go-ethereum's money-critical Keccak-256 — the hash behind
// every CREATE2 deposit address — for the ProjectZKM/Ziren zkVM backend
// (go-ethereum/crypto/keccak_ziren.go). That must never happen (see address.go's import
// guardrail comment), so this file turns any `-tags ziren` build or test into an immediate
// compile error instead of a silently different binary. The undefined identifier below is
// deliberate.
var _ = walletd_must_never_be_built_with_tags_ziren
