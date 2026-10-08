package types

// WatermarkConfig holds the parameters for embedding/extracting a watermark.
type WatermarkConfig struct {
	// CoefficientPosition is the (u,v) position in the 8x8 DCT block
	// used to embed each bit. Mid-frequencies are robust to re-encoding.
	CoefficientU int
	CoefficientV int

	// Delta is the magnitude added/subtracted to the coefficient to encode a bit.
	// Larger delta = more robust but more visible.
	Delta float64

	// Redundancy is how many times each bit is repeated across blocks.
	// Higher = more robust to corruption but slower and requires more space.
	Redundancy int
}

// DefaultConfig returns a sensible default configuration for mid-frequency
// watermarking with high redundancy.
func DefaultConfig() WatermarkConfig {
	return WatermarkConfig{
		CoefficientU: 4,
		CoefficientV: 4,
		Delta:        25.0,
		Redundancy:   8,
	}
}

// FrameDimensions describes the size of a video frame.
type FrameDimensions struct {
	Width  int
	Height int
}

// YUVFrame holds the three planes of a YUV420p frame.
// Y is the luma plane (width * height bytes).
// U and V are the chroma planes (width/2 * height/2 bytes each).
type YUVFrame struct {
	Y []byte
	U []byte
	V []byte
}
