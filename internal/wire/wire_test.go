// internal/wire/wire_test.go
package wire

import (
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	orig := Frame{
		ID:      42,
		Type:    FrameTypeData,
		Payload: []byte("hello, unreliable world"),
	}

	encoded, err := EncodeFrame(orig)
	if err != nil {
		t.Fatalf("EncodeFrame error: %v", err)
	}

	if len(encoded) < 2 {
		t.Fatalf("encoded too short")
	}

	// strip flags, unstuff, parse
	raw, err := slipUnstuff(encoded[1 : len(encoded)-1])
	if err != nil {
		t.Fatalf("slipUnstuff error: %v", err)
	}

	decoded, crcOK, err := ParseFrame(raw)
	if err != nil {
		t.Fatalf("ParseFrame error: %v", err)
	}
	if !crcOK {
		t.Fatalf("CRC not OK on valid frame")
	}

	if decoded.ID != orig.ID || decoded.Type != orig.Type {
		t.Fatalf("header mismatch: got %#v, want %#v", decoded, orig)
	}
	if string(decoded.Payload) != string(orig.Payload) {
		t.Fatalf("payload mismatch: got %q, want %q", decoded.Payload, orig.Payload)
	}
}

// Hammer all bits in the encoded frame (excluding the leading/trailing flag)
// and verify that no single-bit flip escapes CRC detection.
//
// Success is either:
//   - slipUnstuff/ParseFrame error, OR
//   - crcOK == false.
//
// We ONLY fail if crcOK == true after a bit flip.
func TestCRCDetectsSingleBitErrors(t *testing.T) {
	orig := Frame{
		ID:      1,
		Type:    FrameTypeData,
		Payload: []byte("test-payload-for-crc"),
	}

	encoded, err := EncodeFrame(orig)
	if err != nil {
		t.Fatalf("EncodeFrame error: %v", err)
	}

	if len(encoded) < 4 {
		t.Fatalf("encoded too short for bit hammering")
	}

	// mutate each bit in the inner bytes (including stuffed data & CRC).
	for i := 1; i < len(encoded)-1; i++ { // skip flags
		for bit := 0; bit < 8; bit++ {
			mut := make([]byte, len(encoded))
			copy(mut, encoded)
			mut[i] ^= 1 << bit

			// try decoding
			raw, err := slipUnstuff(mut[1 : len(mut)-1])
			if err != nil {
				// framing busted => detected, good
				continue
			}
			_, crcOK, err := ParseFrame(raw)
			if err != nil {
				// also fine
				continue
			}
			if crcOK {
				t.Fatalf("single-bit flip at byte %d bit %d escaped CRC", i, bit)
			}
		}
	}
}
