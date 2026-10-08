package watermark

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/rtagliaviaz/watermark-service/internal/codec"
	"github.com/rtagliaviaz/watermark-service/internal/dct"
	"github.com/rtagliaviaz/watermark-service/internal/types"
)

// EmbedOptions configures how the watermark is embedded.
type EmbedOptions struct {
	ID     string
	Config types.WatermarkConfig
}

// EmbedResult describes what happened during embedding.
type EmbedResult struct {
	BitsEmbedded int
	BitLength    int
	Redundancy   int
}

// Embed encodes the ID as bits and writes them into the Y plane of the frame
// using DCT on non-overlapping 8x8 blocks.
//
// This is the sequential forensic path, kept for compatibility with the
// existing tests and the `embed` CLI command.
func Embed(frame *types.YUVFrame, dims types.FrameDimensions, opts EmbedOptions) (*EmbedResult, error) {
	bits, err := codec.Encode(opts.ID)
	if err != nil {
		return nil, err
	}
	if len(bits) == 0 {
		return &EmbedResult{}, nil
	}

	cfg := opts.Config
	if cfg.Delta <= 0 {
		cfg = types.DefaultConfig()
	}

	blocksPerRow := dims.Width / 8
	blocksPerCol := dims.Height / 8
	if blocksPerRow == 0 || blocksPerCol == 0 {
		return &EmbedResult{}, nil
	}

	bitIndex := 0
	blocksWritten := 0
	for by := 0; by < blocksPerCol; by++ {
		for bx := 0; bx < blocksPerRow; bx++ {
			embedOneBlock(frame, dims, bx, by, bits[bitIndex], cfg)
			bitIndex = (bitIndex + 1) % len(bits)
			blocksWritten++
		}
	}

	return &EmbedResult{
		BitsEmbedded: blocksWritten,
		BitLength:    len(bits),
		Redundancy:   blocksWritten / len(bits),
	}, nil
}

// EmbedBatch embeds the same ID into multiple frames sequentially.
func EmbedBatch(frames []*types.YUVFrame, dims types.FrameDimensions, opts EmbedOptions) (int, error) {
	bits, err := codec.Encode(opts.ID)
	if err != nil {
		return 0, err
	}
	cfg := opts.Config
	if cfg.Delta <= 0 {
		cfg = types.DefaultConfig()
	}
	total := 0
	for _, frame := range frames {
		n, err := embedBits(frame, dims, bits, cfg)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// EmbedSegmentBit embeds a single bit (0 or 1) into every 8x8 block of the
// Y plane. This is the building block of A/B watermarking.
//
// The work is parallelized across CPU cores. The block grid is split into
// horizontal bands, and each band is processed by an independent goroutine.
// Because the bands occupy disjoint rows of the Y plane, no synchronization
// is needed beyond the WaitGroup — there is no shared mutable state.
func EmbedSegmentBit(
	frame *types.YUVFrame,
	dims types.FrameDimensions,
	bit byte,
	cfg types.WatermarkConfig,
) (int, error) {
	if bit != 0 && bit != 1 {
		return 0, fmt.Errorf("bit must be 0 or 1, got %d", bit)
	}
	if cfg.Delta <= 0 {
		cfg = types.DefaultConfig()
	}

	blocksPerRow := dims.Width / 8
	blocksPerCol := dims.Height / 8
	if blocksPerRow == 0 || blocksPerCol == 0 {
		return 0, nil
	}

	// Choose a worker count. Use all logical CPUs but never more workers
	// than there are block rows, otherwise some goroutines would have no
	// work and their spawn cost would be pure overhead.
	workers := runtime.NumCPU()
	if workers > blocksPerCol {
		workers = blocksPerCol
	}
	if workers < 1 {
		workers = 1
	}

	rowsPerWorker := (blocksPerCol + workers - 1) / workers

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		startRow := w * rowsPerWorker
		endRow := startRow + rowsPerWorker
		if endRow > blocksPerCol {
			endRow = blocksPerCol
		}
		if startRow >= endRow {
			break
		}

		wg.Add(1)
		go func(startRow, endRow int) {
			defer wg.Done()
			for by := startRow; by < endRow; by++ {
				for bx := 0; bx < blocksPerRow; bx++ {
					embedOneBlock(frame, dims, bx, by, bit, cfg)
				}
			}
		}(startRow, endRow)
	}

	wg.Wait()
	return blocksPerRow * blocksPerCol, nil
}

// embedBits is the sequential primitive used by Embed and EmbedBatch.
func embedBits(frame *types.YUVFrame, dims types.FrameDimensions, bits []byte, cfg types.WatermarkConfig) (int, error) {
	if len(bits) == 0 {
		return 0, nil
	}
	blocksPerRow := dims.Width / 8
	blocksPerCol := dims.Height / 8
	if blocksPerRow == 0 || blocksPerCol == 0 {
		return 0, nil
	}

	bitIndex := 0
	blocksWritten := 0
	for by := 0; by < blocksPerCol; by++ {
		for bx := 0; bx < blocksPerRow; bx++ {
			embedOneBlock(frame, dims, bx, by, bits[bitIndex], cfg)
			bitIndex = (bitIndex + 1) % len(bits)
			blocksWritten++
		}
	}
	return blocksWritten, nil
}

// embedOneBlock extracts the 8x8 block at (bx, by), applies the DCT, embeds
// one bit in the coefficient at (CoefficientU, CoefficientV), applies the
// inverse DCT, and writes the result back to the Y plane. This is the inner
// kernel called millions of times per video.
//
// The function is pure: it reads and writes only the memory of the block
// at (bx, by), so calling it concurrently for different (bx, by) pairs is
// safe.
func embedOneBlock(
	frame *types.YUVFrame,
	dims types.FrameDimensions,
	bx, by int,
	bit byte,
	cfg types.WatermarkConfig,
) {
	var block dct.Block8x8
	baseX := bx * 8
	baseY := by * 8

	// Load the 8x8 block from the Y plane.
	for y := 0; y < 8; y++ {
		row := (baseY + y) * dims.Width
		for x := 0; x < 8; x++ {
			block[x][y] = float64(frame.Y[row+baseX+x])
		}
	}

	coeffs := dct.Forward(block)

	u, v := cfg.CoefficientU, cfg.CoefficientV
	c := coeffs[u][v]
	if bit == 1 {
		if c < 0 {
			c = -c
		}
		coeffs[u][v] = c + cfg.Delta
	} else {
		if c > 0 {
			c = -c
		}
		coeffs[u][v] = c - cfg.Delta
	}

	reconstructed := dct.Inverse(coeffs)

	// Write the modified block back, clamping to [0, 255].
	for y := 0; y < 8; y++ {
		row := (baseY + y) * dims.Width
		for x := 0; x < 8; x++ {
			val := reconstructed[x][y]
			if val < 0 {
				val = 0
			} else if val > 255 {
				val = 255
			}
			frame.Y[row+baseX+x] = byte(val + 0.5)
		}
	}
}
