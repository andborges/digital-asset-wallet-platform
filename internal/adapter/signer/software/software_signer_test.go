package software

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	decredecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// secp256k1NForTest is the secp256k1 curve order, the same hex constant AD-10's recipe (and
// internal/adapter/signer/kms's own copy of it) cites — duplicated here rather than
// exported from the kms package, since importing across these two independent signer
// packages would create a coupling neither needs otherwise.
var secp256k1NForTest, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", 16)

// privateKeyBytes returns key's raw 32-byte big-endian scalar — secp256k1.PrivateKey
// exposes this only via its embedded ModNScalar field (Key.Bytes()), not a top-level
// Serialize method.
func privateKeyBytes(key *secp256k1.PrivateKey) []byte {
	b := key.Key.Bytes()
	return b[:]
}

func TestNewSigner_ValidHexKey(t *testing.T) {
	privateKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	keyHex := hex.EncodeToString(privateKeyBytes(privateKey))

	signer, err := NewSigner(keyHex)
	if err != nil {
		t.Fatalf("NewSigner() error = %v, want nil", err)
	}
	if signer.Address() == "" {
		t.Fatal("Address() = \"\", want a non-empty derived Ethereum address")
	}
}

func TestNewSigner_AcceptsHexPrefix(t *testing.T) {
	privateKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	keyHex := "0x" + hex.EncodeToString(privateKeyBytes(privateKey))

	if _, err := NewSigner(keyHex); err != nil {
		t.Fatalf("NewSigner() error = %v, want nil (a 0x prefix must be tolerated)", err)
	}
}

func TestNewSigner_RejectsMalformedHex(t *testing.T) {
	if _, err := NewSigner("not-hex"); err == nil {
		t.Fatal("NewSigner() error = nil, want an error for malformed hex")
	}
}

func TestNewSigner_RejectsWrongLength(t *testing.T) {
	if _, err := NewSigner("aabbcc"); err == nil {
		t.Fatal("NewSigner() error = nil, want an error for a key shorter than 32 bytes")
	}
}

func TestNewSigner_RejectsZeroKey(t *testing.T) {
	zero := make([]byte, 32)
	if _, err := NewSigner(hex.EncodeToString(zero)); err == nil {
		t.Fatal("NewSigner() error = nil, want an error for an all-zero private key")
	}
}

// TestNewSigner_RejectsScalarAtOrAboveCurveOrder proves the review's core finding
// (2026-07-21, edge-case review): secp256k1.PrivKeyFromBytes silently reduces a scalar >= N
// modulo N via ModNScalar.SetByteSlice rather than erroring, so without an explicit
// pre-check, a malformed/mistyped/corrupted SIGNER_PRIVATE_KEY value that happens to land
// >= N would construct a DIFFERENT, valid-looking key than the bytes actually configured,
// with no error at all. NewSigner must reject this, not silently accept a different key.
func TestNewSigner_RejectsScalarAtOrAboveCurveOrder(t *testing.T) {
	nBytes := make([]byte, 32)
	secp256k1NForTest.FillBytes(nBytes)
	if _, err := NewSigner(hex.EncodeToString(nBytes)); err == nil {
		t.Fatal("NewSigner() error = nil, want an error for a scalar exactly equal to the curve order N")
	}

	aboveN := new(big.Int).Add(secp256k1NForTest, big.NewInt(1))
	aboveNBytes := make([]byte, 32)
	aboveN.FillBytes(aboveNBytes)
	if _, err := NewSigner(hex.EncodeToString(aboveNBytes)); err == nil {
		t.Fatal("NewSigner() error = nil, want an error for a scalar one above the curve order N")
	}
}

// TestNewSigner_AcceptsScalarOneBelowCurveOrder proves the boundary's other side: N-1 is a
// perfectly valid private key and must not be rejected by the same check.
func TestNewSigner_AcceptsScalarOneBelowCurveOrder(t *testing.T) {
	belowN := new(big.Int).Sub(secp256k1NForTest, big.NewInt(1))
	belowNBytes := make([]byte, 32)
	belowN.FillBytes(belowNBytes)
	if _, err := NewSigner(hex.EncodeToString(belowNBytes)); err != nil {
		t.Fatalf("NewSigner() error = %v, want nil for a scalar one below the curve order N", err)
	}
}

// TestSigner_Sign_ProducesRecoverableSignature proves the core cryptographic claim this
// package's own doc comment makes: signing a real digest with a real key produces a
// standard 65-byte Ethereum signature whose recovery id, when used to recover a public key
// via the same decred RecoverCompact machinery AD-10's recipe relies on, matches the
// signing key's own public key — the same property go-ethereum's crypto.Sign guarantees,
// reproduced here without importing go-ethereum at all.
func TestSigner_Sign_ProducesRecoverableSignature(t *testing.T) {
	privateKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	keyHex := hex.EncodeToString(privateKeyBytes(privateKey))
	signer, err := NewSigner(keyHex)
	if err != nil {
		t.Fatalf("NewSigner() error = %v, want nil", err)
	}

	var digest [32]byte
	if _, err := rand.Read(digest[:]); err != nil {
		t.Fatalf("generate random digest: %v", err)
	}

	sig, err := signer.Sign(context.Background(), core.ChainBase, digest)
	if err != nil {
		t.Fatalf("Sign() error = %v, want nil", err)
	}

	r := sig[0:32]
	s := sig[32:64]
	v := sig[64]
	if v > 1 {
		t.Fatalf("recovery id v = %d, want 0 or 1", v)
	}

	compact := make([]byte, 65)
	compact[0] = 27 + v
	copy(compact[1:33], r)
	copy(compact[33:65], s)

	recovered, _, err := decredecdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		t.Fatalf("RecoverCompact() error = %v, want nil", err)
	}
	if !recovered.IsEqual(privateKey.PubKey()) {
		t.Fatal("the recovered public key does not match the signing key's own public key")
	}
}

// TestSigner_Sign_LowS proves every signature this package produces is canonical/low-s
// (AD-10's recipe requirement — a valid Ethereum transaction encoding requires it), for
// several distinct digests.
func TestSigner_Sign_LowS(t *testing.T) {
	privateKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	signer, err := NewSigner(hex.EncodeToString(privateKeyBytes(privateKey)))
	if err != nil {
		t.Fatalf("NewSigner() error = %v, want nil", err)
	}

	halfN := new(big.Int).Rsh(secp256k1NForTest, 1)

	for i := 0; i < 5; i++ {
		var digest [32]byte
		if _, err := rand.Read(digest[:]); err != nil {
			t.Fatalf("generate random digest: %v", err)
		}
		sig, err := signer.Sign(context.Background(), core.ChainBase, digest)
		if err != nil {
			t.Fatalf("Sign() error = %v, want nil", err)
		}
		s := new(big.Int).SetBytes(sig[32:64])
		if s.Cmp(halfN) > 0 {
			t.Fatalf("s = %s exceeds N/2 = %s — signature is not canonical/low-s", s, halfN)
		}
	}
}

// TestSigner_Sign_DeterministicPerDigest proves Sign is deterministic (RFC6979) — the same
// key and digest always produce the same signature, never a randomized one — which is what
// decred's underlying signRFC6979 (used by SignCompact) guarantees.
func TestSigner_Sign_DeterministicPerDigest(t *testing.T) {
	privateKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	signer, err := NewSigner(hex.EncodeToString(privateKeyBytes(privateKey)))
	if err != nil {
		t.Fatalf("NewSigner() error = %v, want nil", err)
	}

	var digest [32]byte
	if _, err := rand.Read(digest[:]); err != nil {
		t.Fatalf("generate random digest: %v", err)
	}

	sig1, err := signer.Sign(context.Background(), core.ChainBase, digest)
	if err != nil {
		t.Fatalf("Sign() error = %v, want nil", err)
	}
	sig2, err := signer.Sign(context.Background(), core.ChainBase, digest)
	if err != nil {
		t.Fatalf("Sign() error = %v, want nil", err)
	}
	if !bytes.Equal(sig1[:], sig2[:]) {
		t.Fatal("Sign produced two different signatures for the same key and digest")
	}
}

var _ core.Signer = (*Signer)(nil)
