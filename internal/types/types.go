package types

// CoefficientPos identifies a position in the 8x8 DCT grid.
type CoefficientPos struct {
	U, V int
}

// WatermarkConfig holds the parameters for embedding/extracting a watermark.
type WatermarkConfig struct {
	// Positions lists the DCT coefficient positions used by the watermark.
	// When non-empty, it takes precedence over CoefficientU/CoefficientV.
	// Multiple positions increase robustness: the same bit is embedded at
	// each position and extraction takes a majority vote across them.
	Positions []CoefficientPos

	// Deprecated: kept for backward compatibility. If Positions is empty,
	// the watermark uses (CoefficientU, CoefficientV) alone.
	CoefficientU int
	CoefficientV int

	// Delta is the magnitude added/subtracted to the coefficient to encode
	// a bit. Larger delta = more robust but more visible.
	Delta float64

	// Redundancy is how many times each bit is repeated across blocks.
	Redundancy int
}

// EffectivePositions returns the list of positions to use. If Positions is
// non-empty it is returned as-is. Otherwise, a single-element list containing
// the legacy (CoefficientU, CoefficientV) is returned.
func (c WatermarkConfig) EffectivePositions() []CoefficientPos {
	if len(c.Positions) > 0 {
		return c.Positions
	}
	return []CoefficientPos{{U: c.CoefficientU, V: c.CoefficientV}}
}

// DefaultConfig returns a sensible default configuration for mid-frequency
// watermarking with high redundancy.
func DefaultConfig() WatermarkConfig {
	return WatermarkConfig{
		Positions:  []CoefficientPos{{U: 4, V: 4}},
		Delta:      25.0,
		Redundancy: 8,
	}
}

// MultiCoefficientPositions is the recommended set of positions when
// multi-coefficient embedding is enabled. All three are mid-frequency and
// respond similarly to recompression.
var MultiCoefficientPositions = []CoefficientPos{
	{U: 4, V: 4},
	{U: 3, V: 5},
	{U: 5, V: 3},
}

// FrameDimensions describes the size of a video frame.
type FrameDimensions struct {
	Width  int
	Height int
}

// YUVFrame holds the three planes of a YUV420p frame.
type YUVFrame struct {
	Y []byte
	U []byte
	V []byte
}
