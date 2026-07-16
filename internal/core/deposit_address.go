package core

import (
	"fmt"

	"github.com/google/uuid"
)

// customerSalt implements AD-8's salt scheme: the customer UUID's 16 bytes, left-padded
// with zeros to a 32-byte CREATE2 salt (first 16 bytes zero, last 16 bytes the UUID's own
// bytes verbatim). Pure byte layout — no cryptography, no I/O — which is why it stays
// directly in core rather than behind a port (see DepositAddressDeriver's doc comment in
// ports.go for the correctness-critical CREATE2 hash this salt feeds into).
func customerSalt(customerID string) ([32]byte, error) {
	id, err := uuid.Parse(customerID)
	if err != nil {
		return [32]byte{}, fmt.Errorf("parse customer id as uuid: %w", err)
	}

	var salt [32]byte
	copy(salt[16:], id[:])
	return salt, nil
}
