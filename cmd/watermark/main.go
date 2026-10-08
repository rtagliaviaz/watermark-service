// cmd/watermark/main.go
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/rtagliaviaz/watermark-service/internal/ffmpeg"
	"github.com/rtagliaviaz/watermark-service/internal/types"
	"github.com/rtagliaviaz/watermark-service/internal/watermark"
)

const usage = `watermark - forensic watermarking tool

Usage:
  watermark embed       <input> <output> <id>    Embed a forensic watermark
  watermark extract     <input>                  Extract a forensic watermark

  watermark ab-prepare  <input> <output-dir>     Prepare A/B segment pairs
  watermark ab-assemble <segments-dir> <session-id>  Assemble a session playlist
  watermark ab-extract  <input>                  Extract session from A/B segments

Options for embed / ab-prepare:
  --crf int                 Encoding quality (lower = better, default 23)
  --preset string           x264 preset (default fast)
  --delta float             DCT coefficient strength (default 25.0)
  --u int                   Coefficient U position (0-7, default 4)
  --v int                   Coefficient V position (0-7, default 4)

Options for ab-prepare:
  --profiles string         JSON file with quality profiles
  --hwaccel string          Hardware acceleration: none, auto, nvenc, ...
  --keep-masters            Keep intermediate files
  --segment-duration float  Segment duration in seconds (default 2.0)
  --kid string              Content key ID (hex, 32 chars)
  --key string              Content key (hex, 32 chars)
  --license-url string      Base URL of the license service
  --packager-path string    Path to the Shaka Packager binary

Options for ab-assemble:
  --output string           Output directory (default: session_<sanitized-id>/)
  --registry string         Optional registry file
  --segment-duration float  Segment duration in seconds (default 2.0)
  --kid string              Content key ID (hex, 32 chars)
  --license-url string      Base URL of the license service

Options for ab-extract:
  --registry string         Optional registry file
  --segment-duration float  Segment duration in seconds (default 2.0)
  --kid string              Content key ID (hex, 32 chars) for decryption
  --key string              Content key (hex, 32 chars) for decryption
  --packager-path string    Path to the Shaka Packager binary

Examples:
  watermark embed input.mp4 output.mp4 "video_abc_user_123"
  watermark extract output.mp4

  watermark ab-prepare master.mp4 segments/ --profiles profiles.json --hwaccel auto
  watermark ab-prepare master.mp4 segments/ --kid $kid --key $key --license-url "http://localhost:4000" --packager-path ".../packager.exe"
  watermark ab-assemble --output session_x --registry registry.json segments/ "user_abc"
  watermark ab-extract --registry registry.json --kid $kid --key $key session_x/master.m3u8
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "embed":
		runEmbed(os.Args[2:])
	case "extract":
		runExtract(os.Args[2:])
	case "ab-prepare":
		runABPrepare(os.Args[2:])
	case "ab-extract":
		runABExtract(os.Args[2:])
	case "ab-assemble":
		runABAssemble(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
}

func runEmbed(args []string) {
	fs := flag.NewFlagSet("embed", flag.ExitOnError)
	crf := fs.Int("crf", 23, "x264 CRF quality (lower = better)")
	preset := fs.String("preset", "fast", "x264 preset")
	delta := fs.Float64("delta", 25.0, "DCT coefficient strength")
	u := fs.Int("u", 4, "Coefficient U position")
	v := fs.Int("v", 4, "Coefficient V position")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 3 {
		fmt.Fprintln(os.Stderr, "usage: watermark embed <input> <output> <id>")
		os.Exit(1)
	}
	inputPath, outputPath, id := rest[0], rest[1], rest[2]

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

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create output dir: %v\n", err)
		os.Exit(1)
	}

	start := time.Now()
	fmt.Printf("📥 Reading:  %s\n", inputPath)
	fmt.Printf("📤 Writing:  %s\n", outputPath)
	fmt.Printf("🔑 Watermark ID: %q\n", id)
	fmt.Println()

	dims, fps, duration, err := ffmpeg.Probe(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("📐 Dimensions: %dx%d @ %.2f fps, %.2fs\n", dims.Width, dims.Height, fps, duration)
	fmt.Println()

	reader, err := ffmpeg.NewReader(inputPath, dims)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reader failed: %v\n", err)
		os.Exit(1)
	}
	defer reader.Close()

	writer, err := ffmpeg.NewWriter(dims, ffmpeg.WriterOptions{
		FPS: fps, OutputPath: outputPath, CRF: *crf, Preset: *preset,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "writer failed: %v\n", err)
		os.Exit(1)
	}

	totalFramesEstimate := int(duration * fps)
	frameIdx := 0
	var totalBlocks int

	for {
		frame, err := reader.ReadFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "read frame error: %v\n", err)
			os.Exit(1)
		}

		res, err := watermark.Embed(frame, dims, watermark.EmbedOptions{
			ID: id, Config: cfg,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "embed error on frame %d: %v\n", frameIdx, err)
			os.Exit(1)
		}
		totalBlocks += res.BitsEmbedded

		if err := writer.WriteFrame(frame); err != nil {
			fmt.Fprintf(os.Stderr, "write frame error: %v\n", err)
			os.Exit(1)
		}

		frameIdx++
		if totalFramesEstimate > 0 && frameIdx%30 == 0 {
			pct := float64(frameIdx) / float64(totalFramesEstimate) * 100
			if pct > 100 {
				pct = 100
			}
			fmt.Printf("\r   Frames: %d / ~%d (%.0f%%)", frameIdx, totalFramesEstimate, pct)
		}
	}

	if err := reader.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "reader close error: %v\n", err)
		os.Exit(1)
	}
	if err := writer.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "writer close error: %v\n", err)
		os.Exit(1)
	}

	elapsed := time.Since(start)
	fmt.Println()
	fmt.Println()
	fmt.Printf("✅ Done. %d frames processed in %.1fs (%.1f fps)\n",
		frameIdx, elapsed.Seconds(), float64(frameIdx)/elapsed.Seconds())
	fmt.Printf("   Watermark blocks written: %d\n", totalBlocks)
	fmt.Printf("   Output: %s\n", outputPath)
}

func runExtract(args []string) {
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	framesToVote := fs.Int("frames", 10, "Number of frames to aggregate")
	delta := fs.Float64("delta", 25.0, "DCT coefficient strength")
	u := fs.Int("u", 4, "Coefficient U position")
	v := fs.Int("v", 4, "Coefficient V position")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: watermark extract <input>")
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

	fmt.Printf("🔍 Extracting watermark from: %s\n", inputPath)

	dims, _, _, err := ffmpeg.Probe(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("📐 Dimensions: %dx%d\n", dims.Width, dims.Height)

	reader, err := ffmpeg.NewReader(inputPath, dims)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reader failed: %v\n", err)
		os.Exit(1)
	}
	defer reader.Close()

	acc := watermark.NewVoteAccumulator(dims, cfg)

	framesRead := 0
	for framesRead < *framesToVote {
		frame, err := reader.ReadFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "read frame error: %v\n", err)
			os.Exit(1)
		}
		if err := acc.AddFrame(frame); err != nil {
			fmt.Fprintf(os.Stderr, "add frame error: %v\n", err)
			os.Exit(1)
		}
		framesRead++
	}

	reader.Close()

	fmt.Printf("🔢 Frames aggregated: %d\n\n", framesRead)

	id, err := acc.Extract()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Extraction failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Watermark found: %q\n", id)
}
