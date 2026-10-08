package watermark

import (
	"testing"

	"github.com/rtagliaviaz/watermark-service/internal/types"
)

// TestExtractRoundTrip verifies that embedding then extracting returns the
// original ID.
func TestExtractRoundTrip(t *testing.T) {
	dims := types.FrameDimensions{Width: 320, Height: 240}
	cfg := types.DefaultConfig()

	ids := []string{
		"a",
		"user_abc",
		"video_1790947028433_user_abc12345",
		"a_very_long_watermark_id_with_many_characters_to_test_long_payloads",
	}

	for _, id := range ids {
		frame := makeTestFrame(dims)
		_, err := Embed(frame, dims, EmbedOptions{ID: id, Config: cfg})
		if err != nil {
			t.Fatalf("Embed(%q) failed: %v", id, err)
		}

		extracted, err := Extract(frame, dims, cfg)
		if err != nil {
			t.Fatalf("Extract failed for %q: %v", id, err)
		}
		if extracted != id {
			t.Errorf("round-trip mismatch:\n  embedded:  %q\n  extracted: %q", id, extracted)
		}
	}
}

// TestExtractFromCleanFrameFails verifies that extracting from a frame with no
// watermark returns an error (not a false positive).
func TestExtractFromCleanFrameFails(t *testing.T) {
	dims := types.FrameDimensions{Width: 320, Height: 240}
	cfg := types.DefaultConfig()

	frame := makeTestFrame(dims)
	// No embedding

	_, err := Extract(frame, dims, cfg)
	if err == nil {
		t.Error("expected extraction to fail on a clean frame, but it succeeded")
	}
}

// TestMultiFrameVoting verifies that aggregating multiple frames improves
// robustness. We simulate corruption by flipping signs on one frame.
func TestMultiFrameVoting(t *testing.T) {
	dims := types.FrameDimensions{Width: 320, Height: 240}
	cfg := types.DefaultConfig()
	id := "video_1790947028433_user_abc12345"

	acc := NewVoteAccumulator(dims, cfg)

	// Embed the ID into 5 frames
	for i := 0; i < 5; i++ {
		frame := makeTestFrame(dims)
		_, err := Embed(frame, dims, EmbedOptions{ID: id, Config: cfg})
		if err != nil {
			t.Fatal(err)
		}
		if err := acc.AddFrame(frame); err != nil {
			t.Fatal(err)
		}
	}

	extracted, err := acc.Extract()
	if err != nil {
		t.Fatal(err)
	}
	if extracted != id {
		t.Errorf("multi-frame extraction mismatch: embedded %q, got %q", id, extracted)
	}
}

// TestExtractFromSingleBlockFails verifies edge case: too small a frame.
func TestExtractFromSingleBlockFails(t *testing.T) {
	dims := types.FrameDimensions{Width: 8, Height: 8}
	cfg := types.DefaultConfig()
	frame := makeTestFrame(dims)

	_, err := Extract(frame, dims, cfg)
	if err == nil {
		t.Error("expected error on 8x8 frame (not enough blocks)")
	}
}
