package evm

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// testPlatformFactory mirrors contracts/test/CreateAddressVectors.t.sol's
// TEST_PLATFORM_FACTORY constant exactly — an arbitrary fixed address standing in for
// wherever the platform's real Factory ends up deployed. Both suites compute against
// this SAME fixed address so their outputs are directly comparable (AC5).
var testPlatformFactory = common.HexToAddress("0x0000000000000000000000000000000000133700")

// TestPlatformFactoryAddress_MatchesForgeVector pins platformFactoryAddress — computed
// from the canonical deployer + fixed factory salt + Factory's init-code hash — against
// the same value contracts/test/CreateAddressVectors.t.sol's
// testPlatformFactoryAddressMatchesExpected asserts. This is half of AC5's cross-language
// pinning (the Solidity test is the other half).
func TestPlatformFactoryAddress_MatchesForgeVector(t *testing.T) {
	want := common.HexToAddress("0xCc0939512Fdb0811bD89aB1E13D6bB131AC3e7A7")
	if platformFactoryAddress != want {
		t.Fatalf("platformFactoryAddress = %s, want %s", platformFactoryAddress.Hex(), want.Hex())
	}
}

// TestCreate2Address_MatchesForgeVectors pins a handful of customer forwarder addresses
// against testPlatformFactory, mirroring
// CreateAddressVectorsTest.testForwarderAddressesMatchExpected byte-for-byte — the other
// half of AC5's cross-language pinning.
func TestCreate2Address_MatchesForgeVectors(t *testing.T) {
	tests := []struct {
		name string
		// salt matches the raw 16 bytes used to build each customer UUID in the Solidity
		// vector (see that file's customerUUIDs), left-padded to 32 bytes exactly as
		// customerSalt (internal/core) does.
		salt [32]byte
		want common.Address
	}{
		{
			name: "customer uuid low128 = 0x00000000000000000000000000000001",
			salt: [32]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
			want: common.HexToAddress("0x3fBd234cBCe33F5E0E6DE317a18f83391043243f"),
		},
		{
			name: "customer uuid low128 = 0x11111111111111111111111111111111",
			salt: [32]byte{
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
			},
			want: common.HexToAddress("0x87C0A5ed755da1D0Ab7Bcdb4C5b659abb6f32dA1"),
		},
		{
			name: "customer uuid low128 = 0x018f66c96433_72ea_a81e_e41bba23bc27",
			salt: [32]byte{
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0x01, 0x8f, 0x66, 0xc9, 0x64, 0x33, 0x72, 0xea, 0xa8, 0x1e, 0xe4, 0x1b, 0xba, 0x23, 0xbc, 0x27,
			},
			want: common.HexToAddress("0xda1b72527005D50a97320CDB8D276eDEDAaF4da7"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := create2Address(testPlatformFactory, tt.salt, forwarderInitCodeHash)
			if got != tt.want {
				t.Fatalf("create2Address(...) = %s, want %s", got.Hex(), tt.want.Hex())
			}
		})
	}
}

// TestDeriveAddress_MatchesForgeProductionVectors pins the EXACT production derivation
// tuple — DeriveAddress's own CREATE2(platformFactoryAddress, salt, forwarderInitCodeHash)
// — against Solidity-derived expected values, mirroring
// CreateAddressVectorsTest.testProductionForwarderAddressesMatchExpected byte-for-byte.
// The other vectors pin each ingredient separately (formula, init-code hash, factory
// address); this one pins their composition, closing AC5's last compositional gap
// (re-review 2026-07-16).
func TestDeriveAddress_MatchesForgeProductionVectors(t *testing.T) {
	tests := []struct {
		name string
		salt [32]byte
		want string
	}{
		{
			name: "customer uuid low128 = 0x00000000000000000000000000000001",
			salt: [32]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
			want: "0x23392da92cB99a92a67866933F56A1FF1a0c2B8F",
		},
		{
			name: "customer uuid low128 = 0x11111111111111111111111111111111",
			salt: [32]byte{
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
			},
			want: "0x69F1091ac791Be6cbF11fD57Ed88A69e0cD16aC6",
		},
		{
			name: "customer uuid low128 = 0x018f66c96433_72ea_a81e_e41bba23bc27",
			salt: [32]byte{
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0x01, 0x8f, 0x66, 0xc9, 0x64, 0x33, 0x72, 0xea, 0xa8, 0x1e, 0xe4, 0x1b, 0xba, 0x23, 0xbc, 0x27,
			},
			want: "0xdcB2d52518a576007e1Ccf439e2b8020Ab13F6a3",
		},
	}

	d := NewDepositAddressDeriver()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.DeriveAddress(tt.salt)
			if err != nil {
				t.Fatalf("DeriveAddress() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Fatalf("DeriveAddress(...) = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestDepositAddressDeriver_DeriveAddress_ReturnsChecksummedHex(t *testing.T) {
	d := NewDepositAddressDeriver()

	var salt [32]byte
	salt[31] = 1

	got, err := d.DeriveAddress(salt)
	if err != nil {
		t.Fatalf("DeriveAddress() error = %v, want nil", err)
	}
	if len(got) != 42 || got[:2] != "0x" {
		t.Fatalf("DeriveAddress() = %q, want a 0x-prefixed 20-byte hex address", got)
	}
	// Deterministic: the same salt must always derive the same address.
	got2, err := d.DeriveAddress(salt)
	if err != nil {
		t.Fatalf("DeriveAddress() error = %v, want nil", err)
	}
	if got != got2 {
		t.Fatalf("DeriveAddress() is not deterministic: %q != %q", got, got2)
	}
}
