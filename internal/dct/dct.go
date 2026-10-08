package dct

import "math"

// cosTable[u][x] = C(u) * cos((2x+1) * u * pi / 16) / 2
// where C(0) = 1/sqrt(2), C(u) = 1 for u > 0.
//
// This is the standard orthonormal DCT-II matrix for 8x8 blocks.
var cosTable [8][8]float64

func init() {
	for u := 0; u < 8; u++ {
		var cu float64 = 1.0
		if u == 0 {
			cu = 1.0 / math.Sqrt2
		}
		for x := 0; x < 8; x++ {
			cosTable[u][x] = cu * math.Cos(float64(2*x+1)*float64(u)*math.Pi/16.0) / 2.0
		}
	}
}

// Block8x8 is an 8x8 matrix of float64 values.
// Used for pixel values (input) and DCT coefficients (output).
type Block8x8 [8][8]float64

// Forward applies the 2D DCT-II to an 8x8 block.
//
// F = M * f * M^T
//
// where M is the precomputed cosine matrix.
func Forward(f Block8x8) Block8x8 {
	var temp, result Block8x8

	// temp = M * f
	for u := 0; u < 8; u++ {
		for y := 0; y < 8; y++ {
			var sum float64
			for x := 0; x < 8; x++ {
				sum += cosTable[u][x] * f[x][y]
			}
			temp[u][y] = sum
		}
	}

	// result = temp * M^T
	for u := 0; u < 8; u++ {
		for v := 0; v < 8; v++ {
			var sum float64
			for y := 0; y < 8; y++ {
				sum += temp[u][y] * cosTable[v][y]
			}
			result[u][v] = sum
		}
	}

	return result
}

// Inverse applies the 2D IDCT-III (inverse of Forward) to an 8x8 block.
//
// f = M^T * F * M
func Inverse(F Block8x8) Block8x8 {
	var temp, result Block8x8

	// temp = M^T * F
	for x := 0; x < 8; x++ {
		for v := 0; v < 8; v++ {
			var sum float64
			for u := 0; u < 8; u++ {
				sum += cosTable[u][x] * F[u][v]
			}
			temp[x][v] = sum
		}
	}

	// result = temp * M
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			var sum float64
			for v := 0; v < 8; v++ {
				sum += temp[x][v] * cosTable[v][y]
			}
			result[x][y] = sum
		}
	}

	return result
}
