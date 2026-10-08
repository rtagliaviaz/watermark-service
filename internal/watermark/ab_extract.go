package watermark

import (
	"errors"
	"runtime"
	"sync"

	"github.com/rtagliaviaz/watermark-service/internal/dct"
	"github.com/rtagliaviaz/watermark-service/internal/types"
)

// SegmentAccumulator collects DCT sign votes across all 8x8 blocks of all
// frames in a segment.
//
// For each block, the sign of the coefficient at (CoefficientU, CoefficientV)
// contributes +1 (positive) or -1 (negative) to a global counter. After all
// frames of a segment have been added, ExtractBit returns the majority-voted
// bit and a confidence score.
//
// This is the A/B counterpart of VoteAccumulator: instead of accumulating per
// block position (for the forensic decoder), it collapses everything into a
// single segment-level vote.
type SegmentAccumulator struct {
	cfg        types.WatermarkConfig
	totalVotes int64
	blockCount int64
}

// NewSegmentAccumulator creates an accumulator with the given config.
func NewSegmentAccumulator(cfg types.WatermarkConfig) *SegmentAccumulator {
	if cfg.Delta <= 0 {
		cfg = types.DefaultConfig()
	}
	return &SegmentAccumulator{cfg: cfg}
}

// AddFrame adds all 8x8 blocks of a frame to the segment vote.
//
// The work is parallelized across CPU cores: the block grid is split into
// horizontal bands, and each worker accumulates its own local sum. The
// partial sums are combined at the end, so no synchronization is needed
// inside the hot loop and no locks are taken while scanning blocks.
func (a *SegmentAccumulator) AddFrame(frame *types.YUVFrame, dims types.FrameDimensions) error {
	blocksPerRow := dims.Width / 8
	blocksPerCol := dims.Height / 8
	if blocksPerRow == 0 || blocksPerCol == 0 {
		return errors.New("frame is smaller than 8x8")
	}

	// Use all logical CPUs, but never more workers than there are block
	// rows: a worker with no rows would only add spawn overhead.
	workers := runtime.NumCPU()
	if workers > blocksPerCol {
		workers = blocksPerCol
	}
	if workers < 1 {
		workers = 1
	}

	rowsPerWorker := (blocksPerCol + workers - 1) / workers

	// partials[w] holds the signed sum computed by worker w. Each worker
	// writes to its own slot, so there is no data race.
	partials := make([]int64, workers)

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
		go func(w, startRow, endRow int) {
			defer wg.Done()

			var sum int64
			for by := startRow; by < endRow; by++ {
				baseY := by * 8
				for bx := 0; bx < blocksPerRow; bx++ {
					baseX := bx * 8

					var block dct.Block8x8
					for y := 0; y < 8; y++ {
						row := (baseY + y) * dims.Width
						for x := 0; x < 8; x++ {
							block[x][y] = float64(frame.Y[row+baseX+x])
						}
					}

					coeffs := dct.Forward(block)
					c := coeffs[a.cfg.CoefficientU][a.cfg.CoefficientV]
					if c >= 0 {
						sum++
					} else {
						sum--
					}
				}
			}
			partials[w] = sum
		}(w, startRow, endRow)
	}

	wg.Wait()

	var total int64
	for _, p := range partials {
		total += p
	}
	a.totalVotes += total
	a.blockCount += int64(blocksPerRow * blocksPerCol)
	return nil
}

// ExtractBit returns the majority-voted bit (0 or 1) and a confidence score
// in [0, 1] defined as |totalVotes| / blockCount.
//
// Confidence close to 1 means the sign of (u,v) is consistent across nearly
// all blocks of the segment. Values above ~0.3 generally indicate a reliable
// A/B decision; below that, the segment is probably too corrupted to trust.
func (a *SegmentAccumulator) ExtractBit() (byte, float64) {
	if a.blockCount == 0 {
		return 0, 0
	}
	abs := a.totalVotes
	if abs < 0 {
		abs = -abs
	}
	confidence := float64(abs) / float64(a.blockCount)
	if a.totalVotes >= 0 {
		return 1, confidence
	}
	return 0, confidence
}

// Reset clears the accumulator so it can be reused for the next segment.
func (a *SegmentAccumulator) Reset() {
	a.totalVotes = 0
	a.blockCount = 0
}

// DecodeSessionFromSegmentBits reconstructs a session ID from a sequence of
// per-segment bits (one bit per segment, in playback order).
//
// Deprecated: this function has moved to the session package as
// session.Decode. It is kept here only so older callers keep compiling.
// New code should import session.Decode directly.
func DecodeSessionFromSegmentBits(bits []byte) (string, error) {
	if len(bits) == 0 {
		return "", errors.New("no segment bits provided")
	}
	signedVotes := make([]int, len(bits))
	for i, b := range bits {
		if b == 1 {
			signedVotes[i] = 1
		} else {
			signedVotes[i] = -1
		}
	}
	return decodeFromVotes(signedVotes)
}
