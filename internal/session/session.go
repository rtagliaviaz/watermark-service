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

const (
	MinStreamBits = codec.MinStreamBits
	MaxStreamBits = codec.MaxStreamBits
)

// hashIDLen is how many bytes of the SHA-256 digest we keep.
const hashIDLen = 12

// hashIDPrefix is mixed into the hash to avoid cross-purpose collisions.
const hashIDPrefix = "wm:v1:"

// NewID generates a unique session ID with the given prefix.
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
func HashID(id string) string {
	h := sha256.Sum256([]byte(hashIDPrefix + id))
	return base64.RawURLEncoding.EncodeToString(h[:hashIDLen])
}

// BitsForSession returns the per-segment bit sequence for the given session
// ID and number of segments, with no error correction.
func BitsForSession(id string, numSegments int) ([]byte, error) {
	return BitsForSessionWithParity(id, numSegments, 0)
}

// BitsForSessionWithParity returns the per-segment bit sequence for the given
// session ID and number of segments. When parityBytes > 0, Reed-Solomon
// parity is appended to the framed hash, allowing the decoder to correct
// up to parityBytes/2 byte errors during extraction.
func BitsForSessionWithParity(id string, numSegments int, parityBytes int) ([]byte, error) {
	if numSegments <= 0 {
		return nil, fmt.Errorf("numSegments must be positive, got %d", numSegments)
	}
	hash := HashID(id)
	payload, err := codec.EncodeWithParity(hash, parityBytes)
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

// Decode reconstructs a session hash from observed bits, with no FEC.
func Decode(observed []byte) (string, error) {
	return DecodeWithParity(observed, 0)
}

// DecodeWithParity reconstructs a session hash from observed bits, applying
// Reed-Solomon error correction with the given parity byte count.
func DecodeWithParity(observed []byte, parityBytes int) (string, error) {
	if len(observed) == 0 {
		return "", errors.New("no bits to decode")
	}
	if len(observed) < MinStreamBits {
		return "", fmt.Errorf("observed only %d bits, need at least %d", len(observed), MinStreamBits)
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
		if parityBytes > 0 && L%8 != 0 {
			continue
		}
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
		if hash, err := codec.DecodeWithParity(bits, parityBytes); err == nil {
			return hash, nil
		}
	}
	return "", errors.New("could not decode session hash (no valid CRC found)")
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// FileRegistry is a JSON-backed mapping from session hash to the original ID.
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
			return r, nil
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
