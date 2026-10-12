package watermark

import (
	"errors"

	"github.com/rtagliaviaz/watermark-service/internal/codec"
	"github.com/rtagliaviaz/watermark-service/internal/dct"
	"github.com/rtagliaviaz/watermark-service/internal/types"
)

// Extract reads the watermark ID from a single frame.
func Extract(frame *types.YUVFrame, dims types.FrameDimensions, cfg types.WatermarkConfig) (string, error) {
	acc := NewVoteAccumulator(dims, cfg)
	if err := acc.AddFrame(frame); err != nil {
		return "", err
	}
	return acc.Extract()
}

// VoteAccumulator aggregates evidence from multiple frames.
type VoteAccumulator struct {
	dims        types.FrameDimensions
	cfg         types.WatermarkConfig
	signedVotes []int
	numBlocks   int
	frameCount  int
}

// NewVoteAccumulator creates an accumulator for frames of the given dimensions.
func NewVoteAccumulator(dims types.FrameDimensions, cfg types.WatermarkConfig) *VoteAccumulator {
	if cfg.Delta <= 0 {
		cfg = types.DefaultConfig()
	}
	blocksPerRow := dims.Width / 8
	blocksPerCol := dims.Height / 8
	numBlocks := blocksPerRow * blocksPerCol
	return &VoteAccumulator{
		dims:        dims,
		cfg:         cfg,
		signedVotes: make([]int, numBlocks),
		numBlocks:   numBlocks,
	}
}

// AddFrame extracts the DCT sign of every 8x8 block in the Y plane and
// accumulates it into the vote counters. When multiple coefficient positions
// are configured, each position contributes one vote for the same block.
func (v *VoteAccumulator) AddFrame(frame *types.YUVFrame) error {
	if v.numBlocks == 0 {
		return errors.New("frame is smaller than 8x8")
	}

	blocksPerRow := v.dims.Width / 8
	blocksPerCol := v.dims.Height / 8
	positions := v.cfg.EffectivePositions()

	idx := 0
	for by := 0; by < blocksPerCol; by++ {
		for bx := 0; bx < blocksPerRow; bx++ {
			var block dct.Block8x8
			baseX := bx * 8
			baseY := by * 8
			for y := 0; y < 8; y++ {
				row := (baseY + y) * v.dims.Width
				for x := 0; x < 8; x++ {
					block[x][y] = float64(frame.Y[row+baseX+x])
				}
			}
			coeffs := dct.Forward(block)
			for _, pos := range positions {
				c := coeffs[pos.U][pos.V]
				if c >= 0 {
					v.signedVotes[idx]++
				} else {
					v.signedVotes[idx]--
				}
			}
			idx++
		}
	}

	v.frameCount++
	return nil
}

// Extract runs the brute-force search over candidate cycle lengths.
func (v *VoteAccumulator) Extract() (string, error) {
	if v.frameCount == 0 {
		return "", errors.New("no frames accumulated")
	}
	return decodeFromVotes(v.signedVotes)
}

// decodeFromVotes brute-forces the cycle length and returns the first ID whose
// CRC32 passes.
func decodeFromVotes(signedVotes []int) (string, error) {
	numBlocks := len(signedVotes)
	if numBlocks < codec.MinStreamBits {
		return "", errors.New("not enough blocks to contain a watermark")
	}

	maxL := numBlocks
	if maxL > codec.MaxStreamBits {
		maxL = codec.MaxStreamBits
	}

	for L := codec.MinStreamBits; L <= maxL; L++ {
		bits := make([]byte, L)
		for j := 0; j < L; j++ {
			sum := 0
			for k := j; k < numBlocks; k += L {
				sum += signedVotes[k]
			}
			if sum > 0 {
				bits[j] = 1
			}
		}
		if id, err := codec.Decode(bits); err == nil {
			return id, nil
		}
	}

	return "", errors.New("could not extract watermark (no valid CRC found)")
}
