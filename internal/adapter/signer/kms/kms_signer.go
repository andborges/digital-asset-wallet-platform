// Package kms implements core.Signer against AWS KMS's asymmetric Sign API
// (ECC_SECG_P256K1 keys, ECDSA_SHA_256 signing algorithm) — the prod backend (AD-10), also
// usable against LocalStack KMS via the same code path (an endpoint override baked into the
// AWS config this package loads).
//
// AWS KMS's Sign API returns a DER-encoded ECDSA signature (ASN.1 SEQUENCE { r INTEGER, s
// INTEGER }) — not the 65-byte r||s||v shape core.Signer's port promises. Turning one into
// the other is real, correctness-critical elliptic-curve cryptography, vendored here
// exactly per AD-10's own instruction ("vendored, not a live dependency" of
// matelang/go-ethereum-aws-kms-tx-signer/v2's approach) — with one deliberate twist: rather
// than depending on go-ethereum's crypto package for the low-s/recovery-id math (which
// would violate AD-1's import boundary — check-import-boundary rejects ANY go-ethereum
// import outside internal/adapter/evm, and this package is explicitly named as one that
// must hold that line, per the story's own Verification section), this package calls
// github.com/decred/dcrd/dcrec/secp256k1/v4 (and its /ecdsa subpackage) directly — the
// EXACT SAME underlying secp256k1 implementation go-ethereum's own crypto package delegates
// to on a non-cgo build (the go-ethereum module's crypto package, signature_nocgo.go). This
// is the identical cryptographic recipe, not a reimplementation of different math:
//
//  1. Parse the DER signature into r, s (encoding/asn1 into a struct{ R, S *big.Int } —
//     Go's stdlib handles ASN.1 SEQUENCE/INTEGER decoding natively, no vendored ASN.1
//     parser needed).
//  2. Normalize to low-s: secp256k1's order N is
//     0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141. If s > N/2,
//     replace s with N-s — canonical/low-s form, required for the signature to be a valid
//     Ethereum transaction encoding.
//  3. Recover the recovery id v (0 or 1): for each candidate v, reconstruct decred's compact
//     signature format ("<27+v><r><s>") and call
//     github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa.RecoverCompact (the same function
//     go-ethereum's own crypto.Ecrecover delegates to internally) to recover a candidate
//     public key, then compare it against the KMS key's actual public key (fetched once via
//     KMS GetPublicKey at construction time and cached for this Signer's whole lifetime —
//     one GetPublicKey call per process lifetime, not per signature). The v that produces a
//     match is the correct recovery id.
//  4. Final signature = r[32] || s[32] || v[1] (65 bytes) — exactly core.Signer's contract.
//
// GetPublicKey returns a DER/SPKI-encoded public key (a SubjectPublicKeyInfo structure).
// Go's stdlib crypto/x509.ParsePKIXPublicKey does NOT recognize secp256k1's OID (Go's
// x509/ecdsa support is limited to the NIST P-224/256/384/521 curves) — verified empirically
// against this module's own go version before writing this comment, never guessed — so this
// package parses the SPKI wrapper itself (encoding/asn1 + crypto/x509/pkix's
// AlgorithmIdentifier struct, both stdlib) to extract the raw 65-byte uncompressed EC point,
// then decodes that point with secp256k1.ParsePubKey (which handles the standard
// 0x04||X||Y uncompressed format directly).
package kms

import (
	"context"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	decredecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// secp256k1N is the secp256k1 curve's order — the exact hex constant AD-10's own recipe
// cites, parsed once at package init.
var secp256k1N = mustParseHexBigInt("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141")

// secp256k1HalfN is secp256k1N / 2 — the low-s normalization threshold (step 2 of the
// recipe above): a signature's s is canonical/low-s iff s <= secp256k1HalfN.
var secp256k1HalfN = new(big.Int).Rsh(secp256k1N, 1)

func mustParseHexBigInt(hexDigits string) *big.Int {
	n, ok := new(big.Int).SetString(hexDigits, 16)
	if !ok {
		// Unreachable outside a transcription bug in this file's own constant — a
		// programming error, not a runtime condition (mirrors evm/fee_estimator.go's
		// mustParseFeeEstimatorABI panic-on-programmer-error pattern).
		panic("kms: invalid secp256k1 order hex constant")
	}
	return n
}

// kmsClient is the minimal AWS KMS API surface Signer needs — small enough to fake in unit
// tests without a real (or LocalStack) KMS endpoint. *kms.Client (the real AWS SDK v2
// client) satisfies this directly.
type kmsClient interface {
	Sign(ctx context.Context, params *kms.SignInput, optFns ...func(*kms.Options)) (*kms.SignOutput, error)
	GetPublicKey(ctx context.Context, params *kms.GetPublicKeyInput, optFns ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error)
}

// Signer implements core.Signer against AWS KMS (or LocalStack KMS). NFR13 holds by
// construction: Sign's return type is a signature only — no KMS key handle, ARN, or key
// material ever appears in an error or log line this package produces.
type Signer struct {
	client  kmsClient
	keyID   string
	pubKey  *secp256k1.PublicKey
	address string
}

// NewSigner loads the default AWS SDK v2 config (respecting AWS_ENDPOINT_URL/
// AWS_ENDPOINT_URL_KMS for LocalStack — AD-10: "the same adapter, same code path" against
// both real KMS and LocalStack KMS) and constructs a Signer against keyID, fetching and
// caching its public key once via GetPublicKey (one call per process lifetime, per this
// package's own doc comment).
func NewSigner(ctx context.Context, keyID string) (*Signer, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return newSigner(ctx, kms.NewFromConfig(cfg), keyID)
}

// newSigner is NewSigner's client-injectable core, factored out so unit tests can supply a
// fake kmsClient without a real AWS config or network access.
func newSigner(ctx context.Context, client kmsClient, keyID string) (*Signer, error) {
	out, err := client.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: aws.String(keyID)})
	if err != nil {
		return nil, fmt.Errorf("kms get public key: %w", err)
	}
	pub, err := parseSPKIPublicKey(out.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("parse kms public key: %w", err)
	}
	return &Signer{
		client:  client,
		keyID:   keyID,
		pubKey:  pub,
		address: deriveAddress(pub),
	}, nil
}

// Address returns the signer's Ethereum address, derived once at construction from the
// KMS key's own public key — never the key itself (NFR13). Useful for operator-facing
// startup logging (confirming the configured KMS key ARN corresponds to the expected
// hot-wallet address without ever logging key material).
func (s *Signer) Address() string {
	return s.address
}

// Sign implements core.Signer: calls KMS's Sign API (ECDSA_SHA_256 against digest, passed
// as MessageType DIGEST so KMS does not hash it again), then applies this package's own
// doc-comment recipe (DER decode -> low-s normalize -> recovery-id-by-trial) to produce the
// standard 65-byte Ethereum signature. chain is accepted for core.Signer's interface
// symmetry (AD-10: one hot-wallet address system-wide) but is otherwise unused — this KMS
// key signs for every configured chain.
func (s *Signer) Sign(ctx context.Context, _ core.Chain, digest [32]byte) ([65]byte, error) {
	out, err := s.client.Sign(ctx, &kms.SignInput{
		KeyId:            aws.String(s.keyID),
		Message:          digest[:],
		MessageType:      types.MessageTypeDigest,
		SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		return [65]byte{}, fmt.Errorf("kms sign: %w", err)
	}

	r, sig, err := parseDERSignature(out.Signature)
	if err != nil {
		return [65]byte{}, fmt.Errorf("parse kms signature: %w", err)
	}

	sig = normalizeLowS(sig)

	v, err := recoverRecoveryID(digest, r, sig, s.pubKey)
	if err != nil {
		return [65]byte{}, fmt.Errorf("recover kms signature recovery id: %w", err)
	}

	var out65 [65]byte
	r.FillBytes(out65[0:32])
	sig.FillBytes(out65[32:64])
	out65[64] = v
	return out65, nil
}

// normalizeLowS implements recipe step 2: s is canonical/low-s iff s <= N/2 (strict
// greater-than is what triggers the flip, so s == N/2 exactly is already canonical and
// passes through unchanged) — extracted as its own function (re-review 2026-07-21) so the
// exact boundary is unit-testable in isolation from signature recovery, which a genuine
// signature landing precisely on it would have vanishing (~2^-256) probability of doing.
func normalizeLowS(s *big.Int) *big.Int {
	if s.Cmp(secp256k1HalfN) > 0 {
		return new(big.Int).Sub(secp256k1N, s)
	}
	return s
}

// parseDERSignature decodes AWS KMS's ECDSA_SHA_256 Sign response (ASN.1 DER, SEQUENCE {
// r INTEGER, s INTEGER }) into its r and s components (recipe step 1).
func parseDERSignature(der []byte) (r, s *big.Int, err error) {
	var sig struct {
		R, S *big.Int
	}
	if _, err := asn1.Unmarshal(der, &sig); err != nil {
		return nil, nil, fmt.Errorf("unmarshal DER ECDSA signature: %w", err)
	}
	if sig.R == nil || sig.S == nil {
		return nil, nil, fmt.Errorf("DER ECDSA signature missing r or s")
	}
	return sig.R, sig.S, nil
}

// recoverRecoveryID implements recipe step 3: for each candidate recovery id v in {0, 1},
// reconstruct decred's compact signature format and attempt RecoverCompact against digest;
// the v whose recovered public key matches want (the KMS key's own cached public key) is
// the correct recovery id. The secp256k1 "x >= N" overflow case (recovery codes 2-3) is not
// tried — the same simplification Ethereum's own signing convention makes system-wide
// (recipe step 3's doc comment): it occurs with probability roughly 2^-127, and every real
// Ethereum signer (go-ethereum's crypto.Sign included) only ever produces v in {0, 1}.
func recoverRecoveryID(digest [32]byte, r, s *big.Int, want *secp256k1.PublicKey) (byte, error) {
	rBytes, sBytes := make([]byte, 32), make([]byte, 32)
	r.FillBytes(rBytes)
	s.FillBytes(sBytes)

	for v := byte(0); v < 2; v++ {
		compact := make([]byte, 65)
		compact[0] = 27 + v
		copy(compact[1:33], rBytes)
		copy(compact[33:65], sBytes)

		candidate, _, err := decredecdsa.RecoverCompact(compact, digest[:])
		if err != nil {
			continue
		}
		if candidate.IsEqual(want) {
			return v, nil
		}
	}
	return 0, fmt.Errorf("no recovery id in {0, 1} recovered the expected public key for this signature")
}

// subjectPublicKeyInfo mirrors crypto/x509's own (unexported) SPKI ASN.1 shape — just
// enough of it (the algorithm identifier plus the raw public key bits) to extract
// secp256k1's raw EC point, which crypto/x509.ParsePKIXPublicKey itself cannot decode (see
// this package's own doc comment).
type subjectPublicKeyInfo struct {
	Algorithm pkix.AlgorithmIdentifier
	PublicKey asn1.BitString
}

// parseSPKIPublicKey decodes a DER/SPKI-encoded public key (AWS KMS's GetPublicKey
// response shape) into a secp256k1 public key, by extracting the raw EC point bytes from
// the SPKI wrapper and parsing them directly (secp256k1.ParsePubKey handles the standard
// 0x04||X||Y 65-byte uncompressed format).
func parseSPKIPublicKey(der []byte) (*secp256k1.PublicKey, error) {
	var spki subjectPublicKeyInfo
	if _, err := asn1.Unmarshal(der, &spki); err != nil {
		return nil, fmt.Errorf("unmarshal SPKI DER: %w", err)
	}
	pub, err := secp256k1.ParsePubKey(spki.PublicKey.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse secp256k1 point from SPKI public key bytes: %w", err)
	}
	return pub, nil
}

// deriveAddress computes pub's Ethereum address: the low 20 bytes of Keccak-256 over the
// uncompressed public key's X||Y coordinates (the 0x04 format-byte prefix is stripped
// first) — see internal/adapter/signer/software's identical helper for why
// golang.org/x/crypto/sha3's NewLegacyKeccak256, not the standardized SHA3-256, is the
// correct hash here. Intentionally duplicated (not shared) between the two signer
// packages: ~10 lines, self-contained, not worth a new shared package for.
func deriveAddress(pub *secp256k1.PublicKey) string {
	uncompressed := pub.SerializeUncompressed()
	h := sha3.NewLegacyKeccak256()
	h.Write(uncompressed[1:])
	sum := h.Sum(nil)
	return "0x" + hex.EncodeToString(sum[12:])
}

var _ core.Signer = (*Signer)(nil)
