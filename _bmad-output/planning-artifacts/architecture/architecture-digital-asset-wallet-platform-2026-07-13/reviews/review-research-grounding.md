# Review — Research Grounding

**Artifact:** `ARCHITECTURE-SPINE.md` (architecture-digital-asset-wallet-platform-2026-07-13)
**Lens:** Verify every committed decision was web-researched or reality-checked rather than asserted from training data.
**Reviewed:** 2026-07-14
**Verdict:** PASS — every priority claim spot-checked against the live web (and live chain RPCs) holds; findings are hygiene-level, none invalidates a committed decision.

## Method

Priority claims were checked against primary sources: direct JSON-RPC calls to public Base and Arbitrum One endpoints (`eth_getCode`, `eth_chainId`), AWS KMS documentation, the BitGo GitHub repository, official release channels (go.dev, soliditylang.org, foundry-rs, docker/compose, postgresql.org), Chainlist, and the RFC Editor.

## Verified correct (evidence)

| Claim (spine location) | Result | Evidence |
| --- | --- | --- |
| Canonical CREATE2 deterministic deployer `0x4e59b44847B379578588920cA78FbF26c0B4956C` live on Base and Arbitrum One (AD-8) | **Confirmed live on both chains** | Direct `eth_getCode` against `https://mainnet.base.org` and `https://arb1.arbitrum.io/rpc` returns the Arachnid deterministic-deployment-proxy runtime bytecode (`0x7fff…600cf3`) on both. It is also an OP Stack preinstall ([specs.optimism.io/protocol/preinstalls.html](https://specs.optimism.io/protocol/preinstalls.html)) and the default deployer in Foundry's deterministic-deployment guide ([getfoundry.sh](https://www.getfoundry.sh/guides/deterministic-deployments-using-create2), [Arachnid/deterministic-deployment-proxy](https://github.com/Arachnid/deterministic-deployment-proxy)). |
| Chain IDs — Base 8453, Arbitrum One 42161, Base Sepolia 84532, Arbitrum Sepolia 421614 (Stack table) | **All correct** | Live `eth_chainId`: Base returns `0x2105` (8453), Arbitrum One returns `0xa4b1` (42161). Testnets confirmed via [Chainlist 84532](https://chainlist.org/chain/84532) and [Chainlist 421614](https://chainlist.org/chain/421614). |
| AWS KMS `ECC_SECG_P256K1` signing (AD-10, Stack) | **Correct** | AWS KMS key-spec reference lists `ECC_SECG_P256K1` (secp256k1) with `SIGN_VERIFY` usage, explicitly recommended for cryptocurrency signing ([AWS KMS key spec reference](https://docs.aws.amazon.com/kms/latest/developerguide/symm-asymm-choose-key-spec.html)). The DER → r/s, low-s normalization, v-recovery pipeline in AD-10 matches the known KMS-to-Ethereum signing requirements. |
| `matelang/go-ethereum-aws-kms-tx-signer` exists (Stack) | **Correct, with a caveat (finding L-2)** | Repo exists at [github.com/matelang/go-ethereum-aws-kms-tx-signer](https://github.com/matelang/go-ethereum-aws-kms-tx-signer); its README prescribes exactly the `ECC_SECG_P256K1` / `SIGN_VERIFY` setup the spine specifies. Import path is the `/v2` module. |
| BitGo eth-multisig-v4 ForwarderV4 exists as reference pattern (AD-8, Deferred, source tree) | **Exists and active, with a caveat (finding M-1)** | [BitGo/eth-multisig-v4](https://github.com/BitGo/eth-multisig-v4) contains `ForwarderV4` and `ForwarderFactoryV4` (CREATE2 factory, parent + fee address model); repo is active (latest release v4.0-hoodeth, May 2026). |
| Go 1.26.x (Stack) | **Current** | Go 1.26 released February 2026 ([go.dev/doc/go1.26](https://go.dev/doc/go1.26)); 1.26.x is the current stable line in July 2026. |
| PostgreSQL 18 (Stack) | **Current** | 18 GA'd September 2025; current patch line 18.4 as of Feb 2026 releases ([postgresql.org release notes](https://www.postgresql.org/about/news/postgresql-184-1710-1614-1518-and-1423-released-3297/)). |
| go-ethereum v1.17.x (Stack) | **Current** | geth 1.17.x is the current release line (1.17.4 latest on [geth.ethereum.org/downloads](https://geth.ethereum.org/downloads)). |
| Solidity 0.8.36 (Stack) | **Current** | 0.8.36 is the latest release ([soliditylang.org releases](https://www.soliditylang.org/blog/category/releases/); 0.8.35 was 2026-04-29, 0.8.36 followed with two medium-severity security fixes). See finding L-3 on recency. |
| Foundry v1.7.x (Stack) | **Current** | v1.7.0 is a real, current release line ([foundry-rs/foundry releases](https://github.com/foundry-rs/foundry/releases)). |
| Docker Compose v5 (Stack) | **Current** | Looks implausible from a 2025 vantage point but is real: v5.3.0 released 2026-07-02 ([docker/compose releases](https://github.com/docker/compose/releases)). Correctly researched, not stale. |
| jackc/pgx v5, goose v3, oapi-codegen v2 (std-http target) (Stack) | **Correct** | All are the current major lines; oapi-codegen v2 supports the stdlib `net/http` (ServeMux) server target, consistent with AD-14. |
| RFC 9457 `application/problem+json` (Conventions) | **Correct and current** | RFC 9457 (July 2023) obsoletes RFC 7807 and remains the current Standards Track problem-details spec ([rfc-editor.org/info/rfc9457](https://www.rfc-editor.org/info/rfc9457/)). |
| EIP-6780 "no destruct-and-redeploy" premise (AD-8) | **Correct** | EIP-6780 (Dencun) restricts SELFDESTRUCT to same-transaction creation; persistent forwarders are the right conclusion, and ForwarderV4 is BitGo's post-6780 persistent design. |
| `anvil_reorg` for reorg testing (Conventions) | **Exists** | Anvil ships multi-block reorg simulation via the `anvil_reorg` custom RPC method (landed via [foundry-rs/foundry#7368](https://github.com/foundry-rs/foundry/issues/7368)); listed in the [anvil custom-methods reference](https://getfoundry.sh/anvil/reference/). |
| Arbitrum NodeInterface / Base (OP Stack) GasPriceOracle for fees (source tree comment) | **Correct constructs** | `NodeInterface` (0xc8 virtual precompile, gasEstimateL1Component) and the OP Stack `GasPriceOracle` predeploy are the correct per-chain L1-fee surfaces for Arbitrum and Base respectively. |

## Findings

### Critical

None.

### High

None.

### Medium

- **M-1 — ForwarderV4 audit provenance is asserted by reputation, not by located evidence.** AD-8 and the Deferred section lean on "BitGo ForwarderV4" as the trusted reference pattern for the forwarder contracts. The repo exists and is active, but the eth-multisig-v4 repository's landing page references no audit report for ForwarderV4/ForwarderFactoryV4, and this review could not confirm a published third-party audit covering the V4 forwarder specifically. Since the contracts epic will model code that custodies customer deposits on this reference, the epic should locate the actual audit report(s) (or commission a review of the derived contracts) rather than inherit "BitGo = audited" by reputation. The spine's wording ("modeled on") is honest; the risk is downstream over-trust.

### Low

- **L-1 — "Verified current 2026-07-14" is claimed but carries no evidence trail.** The Stack table header asserts verification but cites nothing. Every entry checked out (including the surprising ones — Docker Compose v5, Solidity 0.8.36 — which is itself strong evidence the research actually happened), so this is a documentation gap, not a correctness one. A one-line source note per row (or a dated research appendix) would make the claim auditable instead of trust-me.
- **L-2 — matelang/go-ethereum-aws-kms-tx-signer is a small, recently re-homed community project.** The repo was moved from `welthee/` to `matelang/` when the original sponsor could no longer maintain it. The spine phrases it as "signer per matelang/…" (a pattern reference), which is the right posture — but the implementation epic should treat it as reference code to vendor/own inside `internal/adapter/signer/kms/`, not as a load-bearing dependency, given AD-10 puts it on the custody-critical path. Note the current module path is the `/v2` module.
- **L-3 — Solidity 0.8.36 is weeks old at pin time.** It is genuinely the latest release and ships two medium-severity security fixes, so pinning it is defensible; just be aware the contracts epic may want to confirm no regression advisories have landed against 0.8.36 (and that Foundry's solc integration and verifiers support it) before final pin. Cosmetic, since the spine already says "the code owns this once it exists."
- **L-4 — Frontmatter dates (`created`/`updated`: 2026-07-14) disagree with the artifact directory name (…-2026-07-13).** Not a research-grounding issue per se, but a provenance inconsistency in an artifact whose Stack table stakes its credibility on a verification date.

## Conclusion

The spine's riskiest external-reality claims — deterministic deployer address and its liveness on both target chains, KMS key spec, all four chain IDs, the BitGo reference pattern, the problem-details RFC, and every version in the Stack table including the two that look "impossible" from stale training data (Docker Compose v5, Solidity 0.8.36) — were all confirmed against live sources. This is the signature of a spine that was actually researched, not asserted. Remaining work is evidentiary hygiene (M-1, L-1) rather than correction.
