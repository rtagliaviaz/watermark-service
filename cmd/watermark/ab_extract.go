package main

import (
	"fmt"
	"io"
	"os"
	"time"

	flag "github.com/spf13/pflag"

	"github.com/rtagliaviaz/watermark-service/internal/ffmpeg"
	"github.com/rtagliaviaz/watermark-service/internal/session"
	"github.com/rtagliaviaz/watermark-service/internal/types"
	"github.com/rtagliaviaz/watermark-service/internal/watermark"
)

func runABExtract(args []string) {
	fs := flag.NewFlagSet("ab-extract", flag.ExitOnError)
	segmentDuration := fs.Float64("segment-duration", 2.0, "Segment duration in seconds")
	delta := fs.Float64("delta", 25.0, "DCT coefficient strength")
	u := fs.Int("u", 4, "Coefficient U position")
	v := fs.Int("v", 4, "Coefficient V position")
	registryPath := fs.String("registry", "", "Optional registry file")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: watermark ab-extract [flags] <input>")
		os.Exit(1)
	}
	inputPath := rest[0]

	if _, err := os.Stat(inputPath); err != nil {
		fmt.Fprintf(os.Stderr, "input not found: %s\n", inputPath)
		os.Exit(1)
	}

	cfg := types.WatermarkConfig{
		CoefficientU: *u,
		CoefficientV: *v,
		Delta:        *delta,
		Redundancy:   8,
	}

	fmt.Printf("🔍 Extracting session from: %s\n", inputPath)

	start := time.Now()

	dims, fps, duration, err := ffmpeg.Probe(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe failed: %v\n", err)
		os.Exit(1)
	}
	if fps <= 0 {
		fmt.Fprintf(os.Stderr, "invalid fps: %.2f\n", fps)
		os.Exit(1)
	}

	fmt.Printf("📐 %dx%d @ %.2f fps, %.1fs\n", dims.Width, dims.Height, fps, duration)

	framesPerSegment := int(*segmentDuration * fps)
	if framesPerSegment <= 0 {
		fmt.Fprintf(os.Stderr, "segment duration too short for fps %.2f\n", fps)
		os.Exit(1)
	}

	totalFrames := int(duration * fps)
	numSegments := (totalFrames + framesPerSegment - 1) / framesPerSegment

	fmt.Printf("🔢 Segmenting into %d segments of %d frames\n\n", numSegments, framesPerSegment)

	reader, err := ffmpeg.NewReader(inputPath, dims)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reader failed: %v\n", err)
		os.Exit(1)
	}
	defer reader.Close()

	allBits := make([]byte, 0, numSegments)

	for segIdx := 0; segIdx < numSegments; segIdx++ {
		acc := watermark.NewSegmentAccumulator(cfg)

		framesInSegment := 0
		for framesInSegment < framesPerSegment {
			frame, err := reader.ReadFrame()
			if err == io.EOF {
				break
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "read frame error: %v\n", err)
				os.Exit(1)
			}
			if err := acc.AddFrame(frame, dims); err != nil {
				fmt.Fprintf(os.Stderr, "add frame error: %v\n", err)
				os.Exit(1)
			}
			framesInSegment++
		}

		bit, confidence := acc.ExtractBit()
		allBits = append(allBits, bit)

		pct := float64(segIdx+1) / float64(numSegments) * 100
		fmt.Printf("\r   Segments: %d / %d (%.0f%%)  last: bit=%d conf=%.2f",
			segIdx+1, numSegments, pct, bit, confidence)

		if framesInSegment < framesPerSegment {
			break
		}
	}

	fmt.Println()
	fmt.Println()

	if len(allBits) < session.MinStreamBits {
		fmt.Fprintf(os.Stderr, "❌ Not enough segments to decode a session "+
			"(%d bits, need at least %d)\n", len(allBits), session.MinStreamBits)
		fmt.Fprintf(os.Stderr, "   Increase video duration or reduce --segment-duration\n")
		os.Exit(1)
	}

	hash, err := session.Decode(allBits)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Extraction failed: %v\n", err)
		os.Exit(1)
	}

	elapsed := time.Since(start)
	fmt.Printf("✅ Session hash: %q\n", hash)

	if *registryPath != "" {
		reg, err := session.OpenRegistry(*registryPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "opening registry: %v\n", err)
			os.Exit(1)
		}
		if id, ok := reg.Lookup(hash); ok {
			fmt.Printf("✅ Original session ID: %q\n", id)
		} else {
			fmt.Printf("⚠  Hash not found in registry %s\n", *registryPath)
		}
	}

	fmt.Printf("   Bits collected: %d segments in %.1fs\n",
		len(allBits), elapsed.Seconds())
}
