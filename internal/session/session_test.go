package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewIDFormat(t *testing.T) {
	id, err := NewID("user_abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "user_abc_") {
		t.Errorf("ID should start with prefix, got %q", id)
	}
}

func TestNewIDUniqueness(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		id, err := NewID("user_abc")
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("duplicate ID generated: %q", id)
		}
		seen[id] = true
	}
}

func TestNewIDEmptyPrefix(t *testing.T) {
	if _, err := NewID(""); err == nil {
		t.Error("expected error for empty prefix")
	}
}

func TestHashIDLengthAndDeterminism(t *testing.T) {
	ids := []string{
		"a",
		"user_abc_1735000000000_a1b2c3d4",
		"video_1790947028433_user_abc12345",
	}
	for _, id := range ids {
		h1 := HashID(id)
		h2 := HashID(id)
		if h1 != h2 {
			t.Errorf("HashID not deterministic for %q", id)
		}
		if len(h1) != 16 {
			t.Errorf("HashID(%q) length = %d, want 16", id, len(h1))
		}
	}
}

func TestHashIDDifferentIDsDifferentHashes(t *testing.T) {
	a := HashID("user_abc_1")
	b := HashID("user_abc_2")
	if a == b {
		t.Errorf("different IDs produced same hash: %q", a)
	}
}

func TestBitsForSessionLength(t *testing.T) {
	for _, n := range []int{200, 500, 5000} {
		bits, err := BitsForSession("user_abc_1735000000000_a1b2c3d4", n)
		if err != nil {
			t.Fatal(err)
		}
		if len(bits) != n {
			t.Errorf("n=%d: got %d bits", n, len(bits))
		}
	}
}

func TestBitsForSessionInvalid(t *testing.T) {
	if _, err := BitsForSession("abc", 0); err == nil {
		t.Error("expected error for numSegments=0")
	}
	if _, err := BitsForSession("abc", -1); err == nil {
		t.Error("expected error for numSegments=-1")
	}
}

func TestDecodeRoundTripHash(t *testing.T) {
	ids := []string{
		"a",
		"user_abc",
		"user_abc_1735000000000_a1b2c3d4",
		"video_1790947028433_user_abc12345",
	}

	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			const n = 2048
			bits, err := BitsForSession(id, n)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Decode(bits)
			if err != nil {
				t.Fatalf("decode failed: %v", err)
			}
			want := HashID(id)
			if got != want {
				t.Errorf("want hash %q, got %q", want, got)
			}
		})
	}
}

func TestDecodeWithNoise(t *testing.T) {
	id := "user_abc_1735000000000_a1b2c3d4"
	const n = 2048

	bits, err := BitsForSession(id, n)
	if err != nil {
		t.Fatal(err)
	}
	for i := range bits {
		if i%20 == 0 {
			bits[i] ^= 1
		}
	}
	got, err := Decode(bits)
	if err != nil {
		t.Fatalf("decode failed with noise: %v", err)
	}
	if got != HashID(id) {
		t.Errorf("want hash %q, got %q", HashID(id), got)
	}
}

func TestDecodeTooShort(t *testing.T) {
	if _, err := Decode(make([]byte, 50)); err == nil {
		t.Error("expected error for 50-bit input")
	}
}

func TestDecodeEmpty(t *testing.T) {
	if _, err := Decode(nil); err == nil {
		t.Error("expected error for nil input")
	}
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

func TestRegistryStoreLookup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	r, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}

	id := "user_abc_1735000000000_a1b2c3d4"
	hash := HashID(id)

	if err := r.Store(hash, id); err != nil {
		t.Fatal(err)
	}

	got, ok := r.Lookup(hash)
	if !ok {
		t.Fatal("lookup failed")
	}
	if got != id {
		t.Errorf("want %q, got %q", id, got)
	}
	if r.Size() != 1 {
		t.Errorf("size = %d, want 1", r.Size())
	}
}

func TestRegistryPersistsAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	id := "user_abc_1735000000000_a1b2c3d4"
	hash := HashID(id)

	r1, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.Store(hash, id); err != nil {
		t.Fatal(err)
	}

	r2, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r2.Lookup(hash)
	if !ok || got != id {
		t.Errorf("persistence failed: ok=%v got=%q", ok, got)
	}
}

func TestRegistryCollisionRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	r, _ := OpenRegistry(path)

	if err := r.Store("h", "id1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Store("h", "id2"); err == nil {
		t.Error("expected error for hash collision")
	}
}

func TestRegistrySameIDIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	r, _ := OpenRegistry(path)

	if err := r.Store("h", "id"); err != nil {
		t.Fatal(err)
	}
	if err := r.Store("h", "id"); err != nil {
		t.Errorf("re-storing same pair should be a no-op, got %v", err)
	}
}

func TestRegistryEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRegistry(path)
	if err != nil {
		t.Fatalf("empty file should be accepted: %v", err)
	}
	if r.Size() != 0 {
		t.Errorf("size = %d, want 0", r.Size())
	}
}
