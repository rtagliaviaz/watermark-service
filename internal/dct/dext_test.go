package dct

import (
	"math"
	"testing"
)

// TestDCTRoundTrip verifies that applying Forward then Inverse
// returns the original values within floating-point tolerance.
func TestDCTRoundTrip(t *testing.T) {
	original := Block8x8{
		{128, 130, 132, 134, 136, 138, 140, 142},
		{128, 130, 132, 134, 136, 138, 140, 142},
		{128, 130, 132, 134, 136, 138, 140, 142},
		{128, 130, 132, 134, 136, 138, 140, 142},
		{128, 130, 132, 134, 136, 138, 140, 142},
		{128, 130, 132, 134, 136, 138, 140, 142},
		{128, 130, 132, 134, 136, 138, 140, 142},
		{128, 130, 132, 134, 136, 138, 140, 142},
	}

	coeffs := Forward(original)
	reconstructed := Inverse(coeffs)

	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			diff := math.Abs(original[x][y] - reconstructed[x][y])
			if diff > 1e-6 {
				t.Errorf("mismatch at [%d][%d]: original=%f reconstructed=%f diff=%e",
					x, y, original[x][y], reconstructed[x][y], diff)
			}
		}
	}
}

// TestDCTUniformBlock verifies the DC coefficient is the average * 8.
// For a uniform block of value V, F[0][0] should be 8*V.
func TestDCTUniformBlock(t *testing.T) {
	block := Block8x8{}
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			block[x][y] = 100
		}
	}

	coeffs := Forward(block)

	// DC coefficient = average * 8 (because of the 2D normalization: 1/4 * 1/sqrt(2) * 1/sqrt(2) * sum)
	expectedDC := 100.0 * 8.0
	if math.Abs(coeffs[0][0]-expectedDC) > 1e-6 {
		t.Errorf("DC coefficient mismatch: got %f, expected %f", coeffs[0][0], expectedDC)
	}

	// All AC coefficients should be ~0
	for u := 0; u < 8; u++ {
		for v := 0; v < 8; v++ {
			if u == 0 && v == 0 {
				continue
			}
			if math.Abs(coeffs[u][v]) > 1e-6 {
				t.Errorf("AC coefficient at [%d][%d] should be 0, got %f", u, v, coeffs[u][v])
			}
		}
	}
}
