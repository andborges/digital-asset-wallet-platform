// Package evm is the chain adapter (AD-1): all go-ethereum imports, chain-ID references,
// and RPC access are confined to this package. Nothing in internal/core or any other
// adapter imports go-ethereum or this package.
package evm

import (
	"encoding/hex"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	// crypto.CreateAddress2 is the CREATE2 primitive every deposit address depends on;
	// it computes its Keccak-256 through go-ethereum's crypto package.
	//
	// GUARDRAIL (Story 1.5 code review): go-ethereum v1.17.x ships an ALTERNATE Keccak
	// backend in crypto/keccak_ziren.go, gated by the `ziren` build tag, that routes
	// hashing through the ProjectZKM/Ziren zkVM runtime (that is why the module appears in
	// go.mod as an indirect dependency — Go records source-level imports from build-tagged
	// files regardless of active tags). walletd MUST NEVER be built or tested with
	// `-tags ziren`: doing so would swap the money-critical Keccak used for address
	// derivation to third-party zkVM code. The default backend (crypto/keccak.go, built
	// under `!ziren`) uses the standard golang.org/x/crypto/sha3 implementation, and no
	// Makefile/CI/build path in this repo sets the tag. The cross-language pinning tests
	// (address_test.go vs contracts/test/CreateAddressVectors.t.sol) would catch any
	// divergence, but the tag must simply never be set in the first place.
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// canonicalDeployerAddress is the well-known deterministic CREATE2 deployer (Nick's
// method / the deterministic deployment proxy), pinned by AD-8 — Foundry's own default
// CREATE2 deployer, already preinstalled on Base, Arbitrum, and their testnets.
//
// The architecture spine's prose cites this address as
// "0x4e59b44847B379578588920cA78FbF26c0B4956C" — solc rejects that exact casing as an
// invalid EIP-55 checksum (a compile error, confirmed while building contracts/). The
// checksum below is solc's own correction for the same 20 bytes.
var canonicalDeployerAddress = common.HexToAddress("0x4e59b44847b379578588920cA78FbF26c0B4956C")

// factorySalt is the fixed, arbitrary salt used to deploy the platform's own Factory
// contract through canonicalDeployerAddress — bytes32(0), matching
// contracts/test/CreateAddressVectors.t.sol's FACTORY_SALT. Fixed once live (AD-8):
// changing it changes the Factory's address, which changes every customer address.
var factorySalt [32]byte

// forwarderInitCodeHash and factoryInitCodeHash are keccak256(creationCode) for
// contracts/src/Forwarder.sol and contracts/src/Factory.sol respectively — extracted by
// running the committed regeneration procedure,
// `forge test --match-test testPrintVectors -vv` (contracts/test/CreateAddressVectors.t.sol;
// see contracts/README.md for the full procedure). Never hand-typed independently of that
// run. Both are fixed once live (AD-8): changing either contract's bytecode changes every
// customer address ever issued.
var (
	forwarderInitCodeHash = mustHexToHash32("8105f1890b2e4462d702e4b7983b3e7b50b71b24318eb817cd5e6dc52cfd455c")
	factoryInitCodeHash   = mustHexToHash32("9f6b39d1bf0f757756561f234c073c8814754cc663f94ea4492dc77cc9af5996")
)

// platformFactoryAddress is the platform's own Factory contract address — computed, not
// hand-copied, via the same CREATE2 helper every customer address uses: deployed through
// canonicalDeployerAddress with factorySalt and Factory's own init-code hash. Deploying
// Factory through the same deterministic deployer everywhere is what makes this address
// identical on every supported chain (AD-8). No on-chain deployment happens in this
// story (see the story's Architectural Decisions) — this is a pure off-chain computation
// of the address Factory *would* have.
var platformFactoryAddress = create2Address(canonicalDeployerAddress, factorySalt, factoryInitCodeHash)

// create2Address computes the standard CREATE2 address formula via go-ethereum's own
// crypto.CreateAddress2 — never reimplemented by hand (see
// core.DepositAddressDeriver's doc comment for why).
func create2Address(deployer common.Address, salt [32]byte, initCodeHash [32]byte) common.Address {
	return crypto.CreateAddress2(deployer, salt, initCodeHash[:])
}

// mustHexToHash32 decodes a 64-character hex string (no 0x prefix) into a [32]byte,
// panicking on malformed input — these are compile-time-fixed constants, so a decode
// failure here means a transcription bug in this file, not bad runtime input.
func mustHexToHash32(s string) [32]byte {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		panic(fmt.Sprintf("evm: invalid init-code hash constant %q: %v", s, err))
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

// DepositAddressDeriver implements core.DepositAddressDeriver using go-ethereum's own
// crypto.CreateAddress2.
type DepositAddressDeriver struct{}

// NewDepositAddressDeriver constructs a core.DepositAddressDeriver.
func NewDepositAddressDeriver() *DepositAddressDeriver {
	return &DepositAddressDeriver{}
}

// DeriveAddress computes CREATE2(platformFactoryAddress, salt, forwarderInitCodeHash)
// and renders it as an EIP-55 checksummed hex string — common.Address.Hex() already
// produces that form, no separate checksum step needed.
func (d *DepositAddressDeriver) DeriveAddress(salt [32]byte) (string, error) {
	addr := create2Address(platformFactoryAddress, salt, forwarderInitCodeHash)
	return addr.Hex(), nil
}

var _ core.DepositAddressDeriver = (*DepositAddressDeriver)(nil)

// IsChecksummedAddress reports whether s is a well-formed, EIP-55-checksummed 20-byte hex
// address. It is exported so composition-root callers (e.g. the wired integration test)
// can validate addresses WITHOUT importing go-ethereum directly — keeping the go-ethereum
// dependency confined to this adapter (AD-1). Round-tripping through common.Address.Hex()
// is stricter than a regex: it rejects wrong length, non-hex characters, and a wrong
// EIP-55 checksum (any input that is not already in canonical checksummed form).
func IsChecksummedAddress(s string) bool {
	return common.HexToAddress(s).Hex() == s
}
