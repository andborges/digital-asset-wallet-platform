package core

import (
	"testing"
)

func TestCustomerSalt_LeftPadsUUIDBytesWithZeros(t *testing.T) {
	// A UUID whose 16 bytes are easy to eyeball: 00010203-0405-0607-0809-0a0b0c0d0e0f.
	const id = "00010203-0405-0607-0809-0a0b0c0d0e0f"

	got, err := customerSalt(id)
	if err != nil {
		t.Fatalf("customerSalt(%q) error = %v, want nil", id, err)
	}

	for i := 0; i < 16; i++ {
		if got[i] != 0 {
			t.Fatalf("salt[%d] = %#x, want 0 (first 16 bytes must be zero padding)", i, got[i])
		}
	}
	want := [16]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f}
	for i := 0; i < 16; i++ {
		if got[16+i] != want[i] {
			t.Fatalf("salt[%d] = %#x, want %#x (last 16 bytes must be the UUID's own bytes)", 16+i, got[16+i], want[i])
		}
	}
}

func TestCustomerSalt_RejectsMalformedUUID(t *testing.T) {
	_, err := customerSalt("not-a-uuid")
	if err == nil {
		t.Fatal("customerSalt(\"not-a-uuid\") error = nil, want a non-nil error")
	}
}

func TestCustomerSalt_DistinctUUIDsProduceDistinctSalts(t *testing.T) {
	a, err := customerSalt("00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := customerSalt("00000000-0000-0000-0000-000000000002")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == b {
		t.Fatal("two distinct customer UUIDs produced the same salt")
	}
}
