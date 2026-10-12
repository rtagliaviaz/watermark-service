package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/klauspost/reedsolomon"
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

// Encode converts a string ID into a slice of bits (0 or 1).
//
// Format: [4B length][N bytes payload][4B CRC32], converted to bits MSB-first.
func Encode(id string) ([]byte, error) {
	framed, err := frameBytes(id)
	if err != nil {
		return nil, err
	}
	return bytesToBits(framed), nil
}

// EncodeWithParity is like Encode but appends parityBytes bytes of
// Reed-Solomon parity after the framed payload. The result tolerates up to
// parityBytes/2 byte errors, which allows the decoder to correct corruption
// instead of merely detecting it.
//
// parityBytes = 0 is equivalent to Encode.
func EncodeWithParity(id string, parityBytes int) ([]byte, error) {
	framed, err := frameBytes(id)
	if err != nil {
		return nil, err
	}
	if parityBytes > 0 {
		framed, err = applyFEC(framed, parityBytes)
		if err != nil {
			return nil, err
		}
	}
	return bytesToBits(framed), nil
}

// Decode reconstructs the original ID from a slice of bits.
func Decode(bits []byte) (string, error) {
	if len(bits) < 8*4 {
		return "", errors.New("bit stream too short for length prefix")
	}

	lengthBytes := bitsToBytes(bits[0:32])
	payloadLen := binary.BigEndian.Uint32(lengthBytes)
	if payloadLen == 0 || payloadLen > 1024 {
		return "", errors.New("invalid payload length")
	}

	totalBytes := 4 + int(payloadLen) + 4
	if len(bits) < totalBytes*8 {
		return "", errors.New("bit stream too short for declared payload")
	}

	payloadBytes := bitsToBytes(bits[32 : 32+int(payloadLen)*8])
	crcBytes := bitsToBytes(bits[32+int(payloadLen)*8 : 32+int(payloadLen)*8+32])
	expectedCRC := binary.BigEndian.Uint32(crcBytes)

	actualCRC := crc32.ChecksumIEEE(payloadBytes)
	if actualCRC != expectedCRC {
		return "", errors.New("CRC32 mismatch: payload corrupted")
	}

	return string(payloadBytes), nil
}

// DecodeWithParity is like Decode but first applies Reed-Solomon error
// correction using the given number of parity bytes. The decoder corrects
// up to parityBytes/2 byte errors before validating the CRC32.
func DecodeWithParity(bits []byte, parityBytes int) (string, error) {
	if parityBytes <= 0 {
		return Decode(bits)
	}
	if len(bits)%8 != 0 {
		return "", fmt.Errorf("with FEC enabled, bit stream length must be a multiple of 8, got %d", len(bits))
	}
	data := bitsToBytes(bits)
	corrected, err := removeFEC(data, parityBytes)
	if err != nil {
		return "", err
	}
	return Decode(bytesToBits(corrected))
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// frameBytes produces [4B length][payload][4B CRC32] as raw bytes.
func frameBytes(id string) ([]byte, error) {
	if len(id) == 0 {
		return nil, errors.New("id cannot be empty")
	}
	if len(id) > 1024 {
		return nil, errors.New("id too long (max 1024 bytes)")
	}

	payload := []byte(id)
	framed := make([]byte, 4+len(payload)+4)
	binary.BigEndian.PutUint32(framed[0:4], uint32(len(payload)))
	copy(framed[4:4+len(payload)], payload)
	crc := crc32.ChecksumIEEE(payload)
	binary.BigEndian.PutUint32(framed[4+len(payload):], crc)
	return framed, nil
}

// applyFEC appends parityBytes of Reed-Solomon parity to data.
//
// The implementation uses one shard per byte, which keeps the layout simple
// and is efficient enough for our small payloads. The total number of shards
// is bounded by 255 (the GF(2^8) limit), which accommodates payloads up to
// 240 bytes with 15 bytes of parity — far beyond anything the A/B mode needs.
func applyFEC(data []byte, parityBytes int) ([]byte, error) {
	if parityBytes <= 0 {
		return data, nil
	}
	dataLen := len(data)
	if dataLen == 0 {
		return nil, errors.New("cannot apply FEC to empty payload")
	}
	if dataLen+parityBytes > 255 {
		return nil, fmt.Errorf("payload (%d) + parity (%d) exceeds RS limit (255)", dataLen, parityBytes)
	}

	enc, err := reedsolomon.New(dataLen, parityBytes)
	if err != nil {
		return nil, fmt.Errorf("creating RS encoder: %w", err)
	}

	shards := make([][]byte, dataLen+parityBytes)
	for i := 0; i < dataLen; i++ {
		shards[i] = []byte{data[i]}
	}
	for i := dataLen; i < dataLen+parityBytes; i++ {
		shards[i] = make([]byte, 1)
	}

	if err := enc.Encode(shards); err != nil {
		return nil, fmt.Errorf("RS encode: %w", err)
	}

	out := make([]byte, 0, dataLen+parityBytes)
	for _, s := range shards {
		out = append(out, s[0])
	}
	return out, nil
}

// removeFEC corrects up to parityBytes/2 errors and returns the original data.
func removeFEC(data []byte, parityBytes int) ([]byte, error) {
	if parityBytes <= 0 {
		return data, nil
	}
	total := len(data)
	if total <= parityBytes {
		return nil, fmt.Errorf("encoded data (%d bytes) must be longer than parity (%d bytes)", total, parityBytes)
	}

	dataShards := total - parityBytes
	parityShards := parityBytes

	enc, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		return nil, fmt.Errorf("creating RS decoder: %w", err)
	}

	shards := make([][]byte, total)
	for i, b := range data {
		shards[i] = []byte{b}
	}

	ok, err := enc.Verify(shards)
	if err != nil {
		return nil, fmt.Errorf("RS verify: %w", err)
	}
	if !ok {
		if err := enc.Reconstruct(shards); err != nil {
			return nil, fmt.Errorf("RS reconstruct: %w", err)
		}
	}

	out := make([]byte, dataShards)
	for i := 0; i < dataShards; i++ {
		out[i] = shards[i][0]
	}
	return out, nil
}

// bytesToBits expands each byte into 8 bits, MSB first.
func bytesToBits(data []byte) []byte {
	bits := make([]byte, len(data)*8)
	for i, b := range data {
		for bit := 0; bit < 8; bit++ {
			bits[i*8+bit] = (b >> (7 - bit)) & 1
		}
	}
	return bits
}

// bitsToBytes packs a slice of bits into bytes, MSB first.
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
