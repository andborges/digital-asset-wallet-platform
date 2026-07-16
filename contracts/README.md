# contracts/

Foundry project holding the platform's on-chain layer: the CREATE2 `Factory` and the
per-customer deposit `Forwarder` (Story 1.5, AD-8). **Nothing here is deployed yet** —
deposit addresses are counterfactual: computed off-chain (`internal/adapter/evm`) as the
address each contract *would* have. Story 3.6 performs the first real deployments.

## Why bytecode stability is sacred

Every customer deposit address is `CREATE2(platformFactory, customerSalt,
keccak256(Forwarder creationCode))`, and the platform Factory's own address is
`CREATE2(canonicalDeployer, bytes32(0), keccak256(Factory creationCode))`. The init-code
hashes are pinned as constants in `internal/adapter/evm/address.go`. **Any change to the
compiled creation bytecode of `Forwarder.sol` or `Factory.sol` — source edits, compiler
settings, toolchain drift — re-derives every deposit address ever issued** (AD-8).

`foundry.toml` therefore pins everything that affects emitted bytecode: `solc_version`,
`optimizer` / `optimizer_runs`, `evm_version`, and `bytecode_hash = "none"` (strips the
CBOR metadata trailer so comment/path edits can't change the hash). Don't loosen any of
these. The cross-language vector tests (below) fail loudly if bytecode drifts anyway.

## ⚠️ TREASURY is a placeholder

`Forwarder.sol`'s `TREASURY` constant is `0x…dEaD` — **not** the real treasury.
`flush()`/`flushToken()` are permissionless and can only send there. The real hot-wallet
address is provisioned at the Story 6.2 key ceremony and MUST replace the placeholder
**before the first production deposit address is issued**: the swap changes the Forwarder
bytecode and re-derives every address (acceptable only pre-production), and any funds
swept before the swap are burned. Tracked in
`_bmad-output/implementation-artifacts/deferred-work.md`.

## Cross-language CREATE2 vectors (AC5)

`test/CreateAddressVectors.t.sol` and `internal/adapter/evm/address_test.go` pin the same
fixed vectors byte-for-byte: the two init-code hashes, the platform factory address, and
sample forwarder addresses against both a fixed test factory and the real platform
factory (the exact production derivation tuple). Both suites must pass together —
`forge test` here, `go test ./internal/adapter/evm/` in the repo root.

### Regenerating vectors after a bytecode change

1. Run the committed print test:
   `forge test --match-test testPrintVectors -vv`
2. Copy the printed values **programmatically or by careful full-line copy — never
   re-typed by eye from wrapped terminal output** (a dropped trailing hex digit has
   already happened once; Go's `mustHexToHash32` panics on it at init).
3. Update, together, in one pass:
   - `internal/adapter/evm/address.go`: `forwarderInitCodeHash`, `factoryInitCodeHash`
   - `test/CreateAddressVectors.t.sol`: expected addresses in all vector tests,
     `PLATFORM_FACTORY`
   - `internal/adapter/evm/address_test.go`: the mirrored fixture tables and
     `TestPlatformFactoryAddress_MatchesForgeVector`
4. Re-run both suites; both must pass.

Remember: once addresses have been issued to real customers, this procedure must never
run again — that's the point of AD-8.

## Dependencies

`lib/forge-std` (v1.16.2) is deliberately vendored in-tree by `forge init` — no git
submodule, no `.gitmodules`, no network fetch in CI. Upgrading it never affects contract
bytecode (test-only dependency), but keep it pinned to whatever a plain checkout builds
with.

## Toolchain

Foundry v1.7.1 (`foundryup --install 1.7.1`), solc 0.8.36 (auto-installed via the
`solc_version` pin). CI installs the same via `foundry-rs/foundry-toolchain@v1`.

## Commands

```shell
forge build          # or: make contracts-build (repo root)
forge test           # or: make contracts-test  (repo root)
```
