package codec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// Encode converts a string ID into a slice of bits (0 or 1).
//
// Format:
//
//	[ 4 bytes length ][ N bytes payload ][ 4 bytes CRC32 ]
//	all converted to bits, MSB first
//
// The length prefix tells the decoder how many payload bytes to read.
// The CRC32 is computed over the payload and lets the decoder verify
// that the extracted ID was not corrupted during encoding/decoding.
const (
	MinStreamBits = 72
	MaxStreamBits = 4096
)

func Encode(id string) ([]byte, error) {
	if len(id) == 0 {
		return nil, errors.New("id cannot be empty")
	}
	if len(id) > 1024 {
		return nil, errors.New("id too long (max 1024 bytes)")
	}

	// Build the framed payload: [length (4B)] [id bytes] [crc32 (4B)]
	payload := []byte(id)
	framed := make([]byte, 4+len(payload)+4)

	binary.BigEndian.PutUint32(framed[0:4], uint32(len(payload)))
	copy(framed[4:4+len(payload)], payload)
	crc := crc32.ChecksumIEEE(payload)
	binary.BigEndian.PutUint32(framed[4+len(payload):], crc)

	// Convert the framed bytes to bits (MSB first)
	bits := make([]byte, len(framed)*8)
	for i, b := range framed {
		for bit := 0; bit < 8; bit++ {
			bits[i*8+bit] = (b >> (7 - bit)) & 1
		}
	}

	return bits, nil
}

// Decode reconstructs the original ID string from a slice of bits.
//
// Returns an error if:
//   - the bits slice is too short to contain even the length prefix
//   - the declared length is invalid
//   - the CRC32 does not match (payload was corrupted)
func Decode(bits []byte) (string, error) {
	if len(bits) < 8*4 {
		return "", errors.New("bit stream too short for length prefix")
	}

	// Read the 4-byte length prefix
	lengthBytes := bitsToBytes(bits[0:32])
	payloadLen := binary.BigEndian.Uint32(lengthBytes)

	if payloadLen == 0 || payloadLen > 1024 {
		return "", errors.New("invalid payload length")
	}

	totalBytes := 4 + int(payloadLen) + 4
	if len(bits) < totalBytes*8 {
		return "", errors.New("bit stream too short for declared payload")
	}

	// Read payload
	payloadBytes := bitsToBytes(bits[32 : 32+int(payloadLen)*8])

	// Read CRC32
	crcBytes := bitsToBytes(bits[32+int(payloadLen)*8 : 32+int(payloadLen)*8+32])
	expectedCRC := binary.BigEndian.Uint32(crcBytes)

	// Verify
	actualCRC := crc32.ChecksumIEEE(payloadBytes)
	if actualCRC != expectedCRC {
		return "", errors.New("CRC32 mismatch: payload corrupted")
	}

	return string(payloadBytes), nil
}

// bitsToBytes packs a slice of bits (each 0 or 1) into bytes, MSB first.
func bitsToBytes(bits []byte) []byte {
	if len(bits)%8 != 0 {
		panic("bitsToBytes: length must be a multiple of 8")
	}
	out := make([]byte, len(bits)/8)
	for i := 0; i < len(out); i++ {
		var b byte
		for bit := 0; bit < 8; bit++ {
			b = (b << 1) | (bits[i*8+bit] & 1)
		}
		out[i] = b
	}
	return out
}
