package watermark

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/rtagliaviaz/watermark-service/internal/codec"
	"github.com/rtagliaviaz/watermark-service/internal/types"
)

// makeFlatNoisyFrame returns a YUV frame filled with uniform random luma in
// [100, 156) so that DCT coefficients at (4,4) are small and dominated by the
// embedded Delta. U and V are flat gray.
//
// Different from makeTestFrame (embed_test.go) which uses a diagonal gradient:
// the gradient's (4,4) coefficient is near zero too, but the A/B tests want a
// noisy baseline that exercises the sign voting more realistically.
func makeFlatNoisyFrame(dims types.FrameDimensions, seed int64) *types.YUVFrame {
	r := rand.New(rand.NewSource(seed))
	ySize := dims.Width * dims.Height
	uvSize := (dims.Width / 2) * (dims.Height / 2)

	frame := &types.YUVFrame{
		Y: make([]byte, ySize),
		U: make([]byte, uvSize),
		V: make([]byte, uvSize),
	}
	for i := range frame.Y {
		frame.Y[i] = byte(100 + r.Intn(56))
	}
	for i := range frame.U {
		frame.U[i] = 128
		frame.V[i] = 128
	}
	return frame
}

func addGaussianNoise(frame *types.YUVFrame, sigma float64, seed int64) {
	r := rand.New(rand.NewSource(seed))
	for i := range frame.Y {
		v := float64(frame.Y[i]) + r.NormFloat64()*sigma
		if v < 0 {
			v = 0
		} else if v > 255 {
			v = 255
		}
		frame.Y[i] = byte(v + 0.5)
	}
}

func TestSegmentBitRoundTrip(t *testing.T) {
	dims := types.FrameDimensions{Width: 320, Height: 240}
	cfg := types.DefaultConfig()

	for _, bit := range []byte{0, 1} {
		t.Run(fmt.Sprintf("bit=%d", bit), func(t *testing.T) {
			frame := makeFlatNoisyFrame(dims, int64(bit+1))

			n, err := EmbedSegmentBit(frame, dims, bit, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Fatal("no blocks written")
			}

			acc := NewSegmentAccumulator(cfg)
			if err := acc.AddFrame(frame, dims); err != nil {
				t.Fatal(err)
			}

			got, confidence := acc.ExtractBit()
			if got != bit {
				t.Errorf("bit mismatch: want %d, got %d (confidence %.3f)",
					bit, got, confidence)
			}
			if confidence < 0.8 {
				t.Errorf("low confidence %.3f", confidence)
			}
		})
	}
}

func TestSegmentBitSurvivesNoise(t *testing.T) {
	dims := types.FrameDimensions{Width: 320, Height: 240}
	cfg := types.DefaultConfig()

	for _, bit := range []byte{0, 1} {
		for _, sigma := range []float64{2.0, 5.0, 10.0} {
			name := fmt.Sprintf("bit=%d/sigma=%.0f", bit, sigma)
			t.Run(name, func(t *testing.T) {
				frame := makeFlatNoisyFrame(dims, int64(bit+1))

				if _, err := EmbedSegmentBit(frame, dims, bit, cfg); err != nil {
					t.Fatal(err)
				}
				addGaussianNoise(frame, sigma, 42)

				acc := NewSegmentAccumulator(cfg)
				if err := acc.AddFrame(frame, dims); err != nil {
					t.Fatal(err)
				}
				got, confidence := acc.ExtractBit()
				if got != bit {
					t.Errorf("bit mismatch after noise sigma=%.1f: want %d, got %d (confidence %.3f)",
						sigma, bit, got, confidence)
				}
			})
		}
	}
}

func TestSegmentBitInvalid(t *testing.T) {
	dims := types.FrameDimensions{Width: 320, Height: 240}
	frame := makeFlatNoisyFrame(dims, 1)
	cfg := types.DefaultConfig()

	if _, err := EmbedSegmentBit(frame, dims, 2, cfg); err == nil {
		t.Error("expected error for bit=2")
	}
}

func TestSegmentAccumulatorReset(t *testing.T) {
	dims := types.FrameDimensions{Width: 320, Height: 240}
	cfg := types.DefaultConfig()

	frame := makeFlatNoisyFrame(dims, 1)
	if _, err := EmbedSegmentBit(frame, dims, 1, cfg); err != nil {
		t.Fatal(err)
	}

	acc := NewSegmentAccumulator(cfg)
	if err := acc.AddFrame(frame, dims); err != nil {
		t.Fatal(err)
	}
	bit1, _ := acc.ExtractBit()
	if bit1 != 1 {
		t.Fatalf("first extraction: want 1, got %d", bit1)
	}

	acc.Reset()

	frame2 := makeFlatNoisyFrame(dims, 2)
	if _, err := EmbedSegmentBit(frame2, dims, 0, cfg); err != nil {
		t.Fatal(err)
	}
	if err := acc.AddFrame(frame2, dims); err != nil {
		t.Fatal(err)
	}
	bit2, _ := acc.ExtractBit()
	if bit2 != 0 {
		t.Errorf("after reset: want 0, got %d", bit2)
	}
}

func TestDecodeSessionFromSegmentBits(t *testing.T) {
	ids := []string{"a", "abc", "sess_123", "user_abc"}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			bits, err := codec.Encode(id)
			if err != nil {
				t.Fatal(err)
			}

			// Simulate 3 full cycles of the payload over the segment stream.
			n := len(bits) * 3
			repeated := make([]byte, n)
			for i := 0; i < n; i++ {
				repeated[i] = bits[i%len(bits)]
			}

			got, err := DecodeSessionFromSegmentBits(repeated)
			if err != nil {
				t.Fatalf("decode failed: %v", err)
			}
			if got != id {
				t.Errorf("want %q, got %q", id, got)
			}
		})
	}
}

func TestDecodeSessionFromNoisySegmentBits(t *testing.T) {
	id := "abc"
	bits, err := codec.Encode(id)
	if err != nil {
		t.Fatal(err)
	}

	const cycles = 5
	n := len(bits) * cycles
	r := rand.New(rand.NewSource(7))
	noisy := make([]byte, n)
	for i := 0; i < n; i++ {
		b := bits[i%len(bits)]
		// 1% random bit flip rate, simulating segment-level extraction errors
		if r.Float64() < 0.01 {
			b ^= 1
		}
		noisy[i] = b
	}

	got, err := DecodeSessionFromSegmentBits(noisy)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if got != id {
		t.Errorf("want %q, got %q", id, got)
	}
}

func TestDecodeSessionEmptyInput(t *testing.T) {
	if _, err := DecodeSessionFromSegmentBits(nil); err == nil {
		t.Error("expected error for empty input")
	}
}
