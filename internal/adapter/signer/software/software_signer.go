// Package software implements core.Signer with an in-memory ECDSA private key — dev/test
// only (AD-10: KMS is the prod backend). It deliberately does NOT import go-ethereum
// (AD-1's import boundary confines all go-ethereum/raw-transaction/RLP code to
// internal/adapter/evm, and check-import-boundary enforces this for every package outside
// it, including this one and internal/adapter/signer/kms). Instead it replicates, byte for
// byte, exactly what go-ethereum's own crypto.Sign does on a non-cgo build
// (the go-ethereum module's crypto package, signature_nocgo.go's Sign function, verified
// against v1.17.4's source): sign via
// github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa.SignCompact (RFC6979-deterministic,
// already low-s/canonical per BIP0062 — decred's own signRFC6979 negates s and flips its
// recovery bit whenever s would otherwise be over the curve's half-order, so no separate
// normalization step is needed here), then reformat decred's compact
// "<27+recoveryCode><R><S>" output into Ethereum's "<R><S><recoveryCode>" order. This is
// the exact same underlying secp256k1 implementation go-ethereum's own crypto package
// delegates to on any platform without cgo — not a reimplementation of different math,
// just the same math via the same library, without the go-ethereum import wrapper around
// it.
package software

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	decredecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// Signer implements core.Signer with an in-memory secp256k1 private key. NFR13 holds by
// construction: Sign's return type is a signature only — the key itself, and Address's own
// implementation, never expose privateKey's bytes.
type Signer struct {
	privateKey *secp256k1.PrivateKey
	address    string
}

// NewSigner parses privateKeyHex (a hex-encoded secp256k1 private key, e.g. the
// SIGNER_PRIVATE_KEY environment variable — dev/test only) into an in-memory key. An
// optional "0x" prefix is tolerated. Returns an error, never a zero-value key, if the hex
// string doesn't decode to exactly 32 bytes.
func NewSigner(privateKeyHex string) (*Signer, error) {
	trimmed := strings.TrimPrefix(privateKeyHex, "0x")
	keyBytes, err := hex.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("decode signer private key hex: %w", err)
	}
	if len(keyBytes) != secp256k1.PrivKeyBytesLen {
		return nil, fmt.Errorf("signer private key must be exactly %d bytes, got %d", secp256k1.PrivKeyBytesLen, len(keyBytes))
	}

	// Reject a scalar >= the curve order N before ever constructing the key (re-review
	// 2026-07-21): secp256k1.PrivKeyFromBytes silently reduces mod N via
	// ModNScalar.SetByteSlice rather than erroring, so a malformed/mistyped/corrupted 32-byte
	// hex value that happens to be >= N would otherwise construct a DIFFERENT, valid-looking
	// key than the bytes an operator actually configured, with no error at all — dangerous
	// for a value this security-sensitive. ModNScalar.SetByteSlice's own return value is
	// exactly this overflow signal; used here purely as a check, discarded otherwise.
	var scalar secp256k1.ModNScalar
	if overflow := scalar.SetByteSlice(keyBytes); overflow {
		return nil, fmt.Errorf("signer private key must be less than the secp256k1 curve order")
	}

	privateKey := secp256k1.PrivKeyFromBytes(keyBytes)
	if privateKey.Key.IsZero() {
		return nil, fmt.Errorf("signer private key must not be zero")
	}

	return &Signer{
		privateKey: privateKey,
		address:    deriveAddress(privateKey.PubKey()),
	}, nil
}

// Sign implements core.Signer: signs digest with the in-memory key via
// github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa.SignCompact, then reformats decred's
// compact signature ("<27+recoveryCode><R 32 bytes><S 32 bytes>") into the standard
// Ethereum signature shape ("<R 32 bytes><S 32 bytes><recoveryCode>") — see this package's
// own doc comment for why this is exactly what go-ethereum's own crypto.Sign does
// internally. chain is accepted for core.Signer's interface symmetry (AD-10: one
// hot-wallet address system-wide) but is otherwise unused — a single in-memory key signs
// for every configured chain.
func (s *Signer) Sign(_ context.Context, _ core.Chain, digest [32]byte) ([65]byte, error) {
	compact := decredecdsa.SignCompact(s.privateKey, digest[:], false)
	if len(compact) != 65 {
		// Defensive, not expected validation: decred's SignCompact always returns exactly
		// 65 bytes for a valid, non-nil *secp256k1.PrivateKey — a future upstream change
		// breaking that invariant must fail loud here, never silently truncate/pad.
		return [65]byte{}, fmt.Errorf("unexpected compact signature length %d, want 65", len(compact))
	}

	var out [65]byte
	copy(out[0:64], compact[1:65])
	out[64] = compact[0] - 27
	return out, nil
}

// Address returns the signer's Ethereum address, derived once at construction — never the
// key itself (NFR13). Useful for operator-facing startup logging (confirming
// SIGNER_PRIVATE_KEY corresponds to the expected hot-wallet address without ever logging
// the key).
func (s *Signer) Address() string {
	return s.address
}

// deriveAddress computes pub's Ethereum address: the low 20 bytes of Keccak-256 over the
// uncompressed public key's X||Y coordinates (the 0x04 format-byte prefix is stripped
// first, per the standard Ethereum address derivation formula) — golang.org/x/crypto/sha3's
// NewLegacyKeccak256 is Ethereum's actual Keccak-256 (pre-final-NIST-padding), not the
// standardized SHA3-256 golang.org/x/crypto/sha3.New256 would produce.
func deriveAddress(pub *secp256k1.PublicKey) string {
	uncompressed := pub.SerializeUncompressed()
	h := sha3.NewLegacyKeccak256()
	h.Write(uncompressed[1:])
	sum := h.Sum(nil)
	return "0x" + hex.EncodeToString(sum[12:])
}

var _ core.Signer = (*Signer)(nil)
