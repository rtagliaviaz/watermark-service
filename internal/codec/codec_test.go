package codec

import (
	"strings"
	"testing"
)

// TestRoundTrip verifies that Encode → Decode returns the original ID.
func TestRoundTrip(t *testing.T) {
	cases := []string{
		"a",
		"user_abc",
		"video_1790947028433_user_abc12345",
		strings.Repeat("x", 100),
		strings.Repeat("x", 1024),
	}

	for _, id := range cases {
		bits, err := Encode(id)
		if err != nil {
			t.Fatalf("Encode(%q) failed: %v", id, err)
		}
		decoded, err := Decode(bits)
		if err != nil {
			t.Fatalf("Decode failed for %q: %v", id, err)
		}
		if decoded != id {
			t.Errorf("round-trip mismatch:\n  original: %q\n  decoded:  %q", id, decoded)
		}
	}
}

// TestEmptyID verifies Encode rejects empty IDs.
func TestEmptyID(t *testing.T) {
	_, err := Encode("")
	if err == nil {
		t.Error("expected error for empty ID, got nil")
	}
}

// TestTooLongID verifies Encode rejects overly long IDs.
func TestTooLongID(t *testing.T) {
	_, err := Encode(strings.Repeat("x", 2000))
	if err == nil {
		t.Error("expected error for overly long ID, got nil")
	}
}

// TestCorruptedBits verifies Decode detects corruption via CRC32.
func TestCorruptedBits(t *testing.T) {
	id := "user_abc"
	bits, err := Encode(id)
	if err != nil {
		t.Fatal(err)
	}

	// Flip a bit in the middle of the payload (after the 4-byte length prefix)
	payloadStart := 32
	if bits[payloadStart+3] == 0 {
		bits[payloadStart+3] = 1
	} else {
		bits[payloadStart+3] = 0
	}

	_, err = Decode(bits)
	if err == nil {
		t.Error("expected CRC32 mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "CRC32") {
		t.Errorf("expected CRC32 error, got: %v", err)
	}
}
