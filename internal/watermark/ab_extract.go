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
// Work is parallelized across CPU cores. Each configured coefficient position
// contributes one vote per block. Partial sums are combined at the end.
func (a *SegmentAccumulator) AddFrame(frame *types.YUVFrame, dims types.FrameDimensions) error {
	blocksPerRow := dims.Width / 8
	blocksPerCol := dims.Height / 8
	if blocksPerRow == 0 || blocksPerCol == 0 {
		return errors.New("frame is smaller than 8x8")
	}

	positions := a.cfg.EffectivePositions()

	workers := runtime.NumCPU()
	if workers > blocksPerCol {
		workers = blocksPerCol
	}
	if workers < 1 {
		workers = 1
	}
	rowsPerWorker := (blocksPerCol + workers - 1) / workers

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
					for _, pos := range positions {
						c := coeffs[pos.U][pos.V]
						if c >= 0 {
							sum++
						} else {
							sum--
						}
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
	a.blockCount += int64(blocksPerRow * blocksPerCol * len(positions))
	return nil
}

// ExtractBit returns the majority-voted bit (0 or 1) and a confidence score.
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
