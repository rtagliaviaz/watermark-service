package watermark

import (
	"math"
	"testing"

	"github.com/rtagliaviaz/watermark-service/internal/codec"
	"github.com/rtagliaviaz/watermark-service/internal/dct"
	"github.com/rtagliaviaz/watermark-service/internal/types"
)

// makeTestFrame creates a synthetic YUV420p frame with a smooth gradient on Y.
func makeTestFrame(dims types.FrameDimensions) *types.YUVFrame {
	ySize := dims.Width * dims.Height
	uvSize := (dims.Width / 2) * (dims.Height / 2)

	frame := &types.YUVFrame{
		Y: make([]byte, ySize),
		U: make([]byte, uvSize),
		V: make([]byte, uvSize),
	}

	// Gradient on Y: 0 to 255 diagonally
	for y := 0; y < dims.Height; y++ {
		for x := 0; x < dims.Width; x++ {
			v := (x + y) * 255 / (dims.Width + dims.Height)
			frame.Y[y*dims.Width+x] = byte(v)
		}
	}
	// U and V = neutral (128)
	for i := range frame.U {
		frame.U[i] = 128
		frame.V[i] = 128
	}

	return frame
}

// TestEmbedChangesFrame verifies that embedding modifies the Y plane.
func TestEmbedChangesFrame(t *testing.T) {
	dims := types.FrameDimensions{Width: 320, Height: 240}
	frame := makeTestFrame(dims)
	original := make([]byte, len(frame.Y))
	copy(original, frame.Y)

	_, err := Embed(frame, dims, EmbedOptions{
		ID:     "user_abc",
		Config: types.DefaultConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Frame should be different
	same := true
	for i := range frame.Y {
		if frame.Y[i] != original[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("expected frame to be modified, but it was identical")
	}
}

// TestEmbedInvisible verifies the change is subtle (mean absolute difference small).
func TestEmbedInvisible(t *testing.T) {
	dims := types.FrameDimensions{Width: 320, Height: 240}
	frame := makeTestFrame(dims)
	original := make([]byte, len(frame.Y))
	copy(original, frame.Y)

	_, err := Embed(frame, dims, EmbedOptions{
		ID:     "user_abc",
		Config: types.DefaultConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Compute mean absolute difference
	var sumAbsDiff float64
	for i := range frame.Y {
		diff := math.Abs(float64(frame.Y[i]) - float64(original[i]))
		sumAbsDiff += diff
	}
	meanDiff := sumAbsDiff / float64(len(frame.Y))

	// Should be subtle — under 3 levels of brightness on average
	if meanDiff > 3.0 {
		t.Errorf("watermark is too visible: mean absolute difference = %.2f", meanDiff)
	}
	t.Logf("Mean absolute difference: %.3f", meanDiff)
}

// TestEmbedSignCorrect verifies that the DCT coefficient at (u,v) has the
// correct sign for each block after embedding.
func TestEmbedSignCorrect(t *testing.T) {
	dims := types.FrameDimensions{Width: 320, Height: 240}
	frame := makeTestFrame(dims)

	id := "user_abc"
	cfg := types.DefaultConfig()
	bits, err := codec.Encode(id)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Embed(frame, dims, EmbedOptions{ID: id, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}

	// Verify every block has the expected sign
	blocksPerRow := dims.Width / 8
	blocksPerCol := dims.Height / 8
	bitIndex := 0

	for by := 0; by < blocksPerCol; by++ {
		for bx := 0; bx < blocksPerRow; bx++ {
			var block dct.Block8x8
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					px := bx*8 + x
					py := by*8 + y
					block[x][y] = float64(frame.Y[py*dims.Width+px])
				}
			}
			coeffs := dct.Forward(block)
			c := coeffs[cfg.CoefficientU][cfg.CoefficientV]

			expectedBit := bits[bitIndex]
			if expectedBit == 1 && c <= 0 {
				t.Errorf("block (%d,%d): expected positive coefficient, got %.2f", bx, by, c)
			}
			if expectedBit == 0 && c >= 0 {
				t.Errorf("block (%d,%d): expected negative coefficient, got %.2f", bx, by, c)
			}

			bitIndex = (bitIndex + 1) % len(bits)
		}
	}
}

// TestEmbedResultStats verifies the EmbedResult reports sensible numbers.
func TestEmbedResultStats(t *testing.T) {
	dims := types.FrameDimensions{Width: 320, Height: 240}
	frame := makeTestFrame(dims)

	id := "user_abc"
	res, err := Embed(frame, dims, EmbedOptions{
		ID:     id,
		Config: types.DefaultConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}

	blocksPerRow := dims.Width / 8
	blocksPerCol := dims.Height / 8
	expectedBlocks := blocksPerRow * blocksPerCol

	if res.BitsEmbedded != expectedBlocks {
		t.Errorf("expected %d blocks embedded, got %d", expectedBlocks, res.BitsEmbedded)
	}
	if res.Redundancy != expectedBlocks/res.BitLength {
		t.Errorf("redundancy mismatch: got %d, expected %d",
			res.Redundancy, expectedBlocks/res.BitLength)
	}

	t.Logf("Bit length: %d, blocks: %d, redundancy: %dx",
		res.BitLength, res.BitsEmbedded, res.Redundancy)
}
