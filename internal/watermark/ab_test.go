package watermark

import (
	"fmt"
	"math/rand"
	"testing"

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

// --- Multi-coefficient tests -------------------------------------------------

func TestMultiCoefficientRoundTrip(t *testing.T) {
	dims := types.FrameDimensions{Width: 320, Height: 240}
	cfg := types.DefaultConfig()
	cfg.Positions = types.MultiCoefficientPositions

	for _, bit := range []byte{0, 1} {
		t.Run(fmt.Sprintf("bit=%d", bit), func(t *testing.T) {
			frame := makeFlatNoisyFrame(dims, int64(bit+1))

			if _, err := EmbedSegmentBit(frame, dims, bit, cfg); err != nil {
				t.Fatal(err)
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
			if confidence < 0.9 {
				t.Errorf("expected high confidence with 3 positions, got %.3f",
					confidence)
			}
		})
	}
}

func TestMultiCoefficientSurvivesStrongerNoise(t *testing.T) {
	dims := types.FrameDimensions{Width: 320, Height: 240}
	cfg := types.DefaultConfig()
	cfg.Positions = types.MultiCoefficientPositions

	// With three positions voting, we should survive a sigma where the
	// single-coefficient configuration would already be marginal.
	for _, bit := range []byte{0, 1} {
		for _, sigma := range []float64{15.0, 20.0} {
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
					t.Errorf("bit mismatch: want %d, got %d (confidence %.3f)",
						bit, got, confidence)
				}
			})
		}
	}
}
