// Package session provides utilities for mapping session IDs to the bit
// sequences used by A/B watermarking, and back.
//
// Session IDs are arbitrary strings (e.g. "user_abc_1735000000000_a1b2c3d4").
// Because the A/B watermark can only store as many bits as there are video
// segments, we do NOT embed the ID itself. Instead we embed a fixed-length
// hash of the ID and store the mapping hash -> ID in a Registry.
//
// This is the standard production pattern: the hash is one-way and always
// fits; the original ID lives in a database that the forensic operator
// consults after extracting the hash from a leaked video.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/rtagliaviaz/watermark-service/internal/codec"
)

// Stream length bounds, matching the codec's framing.
//
// The framed payload is [4 bytes length][N bytes payload][4 bytes CRC32],
// so the minimum is 4 + 1 + 4 = 9 bytes = 72 bits.
const (
	MinStreamBits = 72
	MaxStreamBits = 4096
)

// hashIDLen is how many bytes of the SHA-256 digest we keep. 12 bytes (96
// bits) gives a birthday-bound collision probability of roughly 1 in 2^48,
// which is effectively zero for any realistic deployment.
const hashIDLen = 12

// hashIDPrefix is mixed into the hash to avoid cross-purpose collisions.
// Bumping this string would let us evolve the scheme without invalidating
// hashes stored by older versions.
const hashIDPrefix = "wm:v1:"

// NewID generates a unique session ID with the given prefix.
//
// Format: <prefix>_<unix-millis>_<8-hex-random>
func NewID(prefix string) (string, error) {
	if prefix == "" {
		return "", errors.New("prefix cannot be empty")
	}
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generating random suffix: %w", err)
	}
	ms := time.Now().UnixMilli()
	return fmt.Sprintf("%s_%d_%s", prefix, ms, hex.EncodeToString(buf[:])), nil
}

// HashID maps an arbitrary session ID to a short, fixed-length string.
//
// The result is always exactly 16 characters, using base64 URL-safe encoding
// without padding. The mapping is deterministic: the same ID always produces
// the same hash.
//
// The hash is one-way. To recover the original ID you must store it
// separately in a Registry at embed time.
func HashID(id string) string {
	h := sha256.Sum256([]byte(hashIDPrefix + id))
	return base64.RawURLEncoding.EncodeToString(h[:hashIDLen])
}

// BitsForSession returns the per-segment bit sequence for the given session
// ID and number of segments.
//
// Internally it hashes the ID to a fixed-length string, encodes that hash
// with the codec's framing, and repeats the resulting bits cyclically to
// fill exactly numSegments bits.
func BitsForSession(id string, numSegments int) ([]byte, error) {
	if numSegments <= 0 {
		return nil, fmt.Errorf("numSegments must be positive, got %d", numSegments)
	}
	return BitsForHash(HashID(id), numSegments)
}

// BitsForHash is the low-level primitive that turns a hash string (already
// produced by HashID) into a per-segment bit sequence.
func BitsForHash(hash string, numSegments int) ([]byte, error) {
	if numSegments <= 0 {
		return nil, fmt.Errorf("numSegments must be positive, got %d", numSegments)
	}
	payload, err := codec.Encode(hash)
	if err != nil {
		return nil, fmt.Errorf("encoding hash: %w", err)
	}
	if len(payload) == 0 {
		return nil, errors.New("codec produced an empty bit stream")
	}
	bits := make([]byte, numSegments)
	for i := 0; i < numSegments; i++ {
		bits[i] = payload[i%len(payload)]
	}
	return bits, nil
}

// Decode reconstructs the hash that was embedded in an observed bit sequence.
//
// The caller is responsible for turning the returned hash back into a
// session ID via a Registry.Lookup.
func Decode(observed []byte) (string, error) {
	if len(observed) == 0 {
		return "", errors.New("no bits to decode")
	}
	if len(observed) < MinStreamBits {
		return "", fmt.Errorf(
			"observed only %d bits, need at least %d to contain a framed payload",
			len(observed), MinStreamBits)
	}

	signed := make([]int, len(observed))
	for i, b := range observed {
		if b == 1 {
			signed[i] = 1
		} else {
			signed[i] = -1
		}
	}

	maxL := len(observed)
	if maxL > MaxStreamBits {
		maxL = MaxStreamBits
	}

	for L := MinStreamBits; L <= maxL; L++ {
		bits := make([]byte, L)
		for j := 0; j < L; j++ {
			sum := 0
			for k := j; k < len(signed); k += L {
				sum += signed[k]
			}
			if sum > 0 {
				bits[j] = 1
			}
		}
		if hash, err := codec.Decode(bits); err == nil {
			return hash, nil
		}
	}
	return "", errors.New("could not decode session hash (no valid CRC found)")
}

// ---------------------------------------------------------------------------
// Registry: hash -> session ID mapping
// ---------------------------------------------------------------------------

// FileRegistry is a simple JSON-backed mapping from session hash to the
// original session ID.
//
// It is safe for concurrent use. Each Store rewrites the entire file, which
// is fine for a CLI or a single-node service but would not scale to a
// distributed deployment. For that, replace it with SQLite/Postgres.
type FileRegistry struct {
	path string

	mu   sync.Mutex
	data map[string]string
}

// OpenRegistry loads (or creates) a registry file at the given path.
func OpenRegistry(path string) (*FileRegistry, error) {
	r := &FileRegistry{
		path: path,
		data: make(map[string]string),
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil // new registry
		}
		return nil, fmt.Errorf("reading registry: %w", err)
	}
	if len(raw) == 0 {
		return r, nil
	}
	if err := json.Unmarshal(raw, &r.data); err != nil {
		return nil, fmt.Errorf("parsing registry: %w", err)
	}
	return r, nil
}

// Store records a hash -> id mapping and persists the file.
//
// Storing the same hash twice with the same ID is a no-op. Storing a
// different ID for an existing hash returns an error (that would indicate a
// hash collision, which should be investigated).
func (r *FileRegistry) Store(hash, id string) error {
	if hash == "" {
		return errors.New("hash cannot be empty")
	}
	if id == "" {
		return errors.New("id cannot be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.data[hash]; ok {
		if existing == id {
			return nil
		}
		return fmt.Errorf("hash collision: %q already maps to %q", hash, existing)
	}
	r.data[hash] = id
	return r.persistLocked()
}

// Lookup returns the original session ID for a given hash.
func (r *FileRegistry) Lookup(hash string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.data[hash]
	return id, ok
}

// Size returns the number of entries in the registry.
func (r *FileRegistry) Size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.data)
}

func (r *FileRegistry) persistLocked() error {
	raw, err := json.MarshalIndent(r.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling registry: %w", err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("writing temp registry: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("renaming registry: %w", err)
	}
	return nil
}
