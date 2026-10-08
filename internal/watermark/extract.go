package watermark

import (
	"errors"

	"github.com/rtagliaviaz/watermark-service/internal/codec"
	"github.com/rtagliaviaz/watermark-service/internal/dct"
	"github.com/rtagliaviaz/watermark-service/internal/types"
)

// Stream length bounds.
//
// The minimum stream is [4 bytes length][1 byte payload][4 bytes CRC] = 72 bits.
// The maximum stream is [4 bytes length][1024 bytes payload][4 bytes CRC] = 8256 bits.
// We cap at a practical 4096 bits to keep extraction fast for typical IDs.
const (
	MinStreamBits = codec.MinStreamBits
	MaxStreamBits = codec.MaxStreamBits
)

// Extract reads the watermark ID from a single frame.
//
// It tries all plausible cycle lengths from MinStreamBits to MaxStreamBits,
// picks the one whose bit stream passes the CRC32 check inside the codec,
// and returns the decoded ID.
func Extract(frame *types.YUVFrame, dims types.FrameDimensions, cfg types.WatermarkConfig) (string, error) {
	acc := NewVoteAccumulator(dims, cfg)
	if err := acc.AddFrame(frame); err != nil {
		return "", err
	}
	return acc.Extract()
}

// VoteAccumulator aggregates evidence from multiple frames.
//
// For each 8x8 block position, it accumulates a signed vote:
//
//	+1 for each frame where the DCT coefficient is positive (bit = 1)
//	-1 for each frame where the DCT coefficient is negative (bit = 0)
//
// Summing across many frames dramatically improves robustness: even if a
// single frame is corrupted by re-encoding, the majority of frames agree on
// the true bit value.
type VoteAccumulator struct {
	dims        types.FrameDimensions
	cfg         types.WatermarkConfig
	signedVotes []int // length = number of blocks per frame
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
// accumulates it into the vote counters.
func (v *VoteAccumulator) AddFrame(frame *types.YUVFrame) error {
	if v.numBlocks == 0 {
		return errors.New("frame is smaller than 8x8")
	}

	blocksPerRow := v.dims.Width / 8
	blocksPerCol := v.dims.Height / 8

	idx := 0
	for by := 0; by < blocksPerCol; by++ {
		for bx := 0; bx < blocksPerRow; bx++ {
			var block dct.Block8x8
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					px := bx*8 + x
					py := by*8 + y
					block[x][y] = float64(frame.Y[py*v.dims.Width+px])
				}
			}

			coeffs := dct.Forward(block)
			c := coeffs[v.cfg.CoefficientU][v.cfg.CoefficientV]

			if c >= 0 {
				v.signedVotes[idx]++
			} else {
				v.signedVotes[idx]--
			}
			idx++
		}
	}

	v.frameCount++
	return nil
}

// Extract runs the brute-force search over candidate cycle lengths using the
// accumulated votes.
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
	if numBlocks < MinStreamBits {
		return "", errors.New("not enough blocks to contain a watermark")
	}

	maxL := numBlocks
	if maxL > MaxStreamBits {
		maxL = MaxStreamBits
	}

	// Try each candidate cycle length, smallest first.
	for L := MinStreamBits; L <= maxL; L++ {
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

		// Try to decode. If CRC passes, we found our ID.
		if id, err := codec.Decode(bits); err == nil {
			return id, nil
		}
	}

	return "", errors.New("could not extract watermark (no valid CRC found)")
}
