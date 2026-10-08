package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	flag "github.com/spf13/pflag"

	"github.com/rtagliaviaz/watermark-service/internal/ffmpeg"
	"github.com/rtagliaviaz/watermark-service/internal/types"
	"github.com/rtagliaviaz/watermark-service/internal/watermark"
)

// QualityProfile describes one output rendition.
type QualityProfile struct {
	Name   string `json:"name"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func runABPrepare(args []string) {
	fs := flag.NewFlagSet("ab-prepare", flag.ExitOnError)
	segmentDuration := fs.Float64("segment-duration", 2.0,
		"Segment duration in seconds (must match streaming-service HLS config)")
	crf := fs.Int("crf", 23, "x264 CRF quality (lower = better)")
	preset := fs.String("preset", "fast", "x264 preset")
	delta := fs.Float64("delta", 25.0, "DCT coefficient strength")
	u := fs.Int("u", 4, "Coefficient U position (0-7)")
	v := fs.Int("v", 4, "Coefficient V position (0-7)")
	profilesPath := fs.String("profiles", "",
		"Path to a JSON file with quality profiles. If empty, a single profile "+
			"matching the input resolution is used.")
	hwaccelFlag := fs.String("hwaccel", "none",
		"Hardware acceleration: none, auto, nvenc, qsv, amf, vaapi, videotoolbox")
	keepMasters := fs.Bool("keep-masters", false,
		"Keep intermediate scaled and master MP4 files for debugging")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: watermark ab-prepare [flags] <input> <output-dir>")
		os.Exit(1)
	}
	inputPath, outputDir := rest[0], rest[1]

	if _, err := os.Stat(inputPath); err != nil {
		fmt.Fprintf(os.Stderr, "input not found: %s\n", inputPath)
		os.Exit(1)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create output dir: %v\n", err)
		os.Exit(1)
	}

	requestedAccel := ffmpeg.ParseHWAccel(*hwaccelFlag)
	if requestedAccel == "" {
		fmt.Fprintf(os.Stderr, "invalid --hwaccel value %q\n", *hwaccelFlag)
		fmt.Fprintln(os.Stderr,
			"valid values: none, auto, nvenc, qsv, amf, vaapi, videotoolbox")
		os.Exit(1)
	}
	resolvedAccel := ffmpeg.ResolveHWAccel(requestedAccel)
	switch {
	case requestedAccel == ffmpeg.HWAccelAuto && resolvedAccel == ffmpeg.HWAccelNone:
		fmt.Println("⚙  HW accel: auto-detected none (using libx264)")
	case requestedAccel == ffmpeg.HWAccelAuto:
		fmt.Printf("⚙  HW accel: auto-detected %s\n", resolvedAccel)
	case resolvedAccel == ffmpeg.HWAccelNone:
		fmt.Println("⚙  HW accel: disabled (using libx264)")
	default:
		fmt.Printf("⚙  HW accel: %s\n", resolvedAccel)
	}

	profiles, err := loadProfiles(*profilesPath, inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading profiles: %v\n", err)
		os.Exit(1)
	}

	cfg := types.WatermarkConfig{
		CoefficientU: *u,
		CoefficientV: *v,
		Delta:        *delta,
		Redundancy:   8,
	}

	start := time.Now()
	fmt.Printf("📥 Input:            %s\n", inputPath)
	fmt.Printf("📤 Output dir:       %s\n", outputDir)
	fmt.Printf("⏱  Segment duration: %.2fs (GOP = %.2f × fps)\n", *segmentDuration, *segmentDuration)
	fmt.Printf("🎞  Profiles:         %d", len(profiles))
	for i, p := range profiles {
		if i == 0 {
			fmt.Printf(" (%s", p.Name)
		} else {
			fmt.Printf(", %s", p.Name)
		}
	}
	fmt.Println(")")
	fmt.Println()

	type profileResult struct {
		name      string
		width     int
		height    int
		segmentsA int
		segmentsB int
	}

	results := make([]profileResult, 0, len(profiles))

	for _, profile := range profiles {
		fmt.Printf("━━━ Profile: %s (%dx%d) ━━━\n", profile.Name, profile.Width, profile.Height)

		scaledPath := filepath.Join(outputDir, fmt.Sprintf("_scaled_%s.mp4", profile.Name))
		if err := scaleMaster(inputPath, scaledPath, profile, resolvedAccel); err != nil {
			fmt.Fprintf(os.Stderr, "scaling to %s failed: %v\n", profile.Name, err)
			os.Exit(1)
		}

		dims, fps, duration, err := ffmpeg.Probe(scaledPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "probe of scaled master failed: %v\n", err)
			os.Exit(1)
		}
		if fps <= 0 {
			fmt.Fprintf(os.Stderr, "invalid fps in scaled master: %.2f\n", fps)
			os.Exit(1)
		}

		framesPerSegment := int(*segmentDuration * fps)
		if framesPerSegment <= 0 {
			fmt.Fprintf(os.Stderr, "segment duration %.2fs is too short for fps %.2f\n",
				*segmentDuration, fps)
			os.Exit(1)
		}
		totalFrames := int(duration * fps)
		expectedSegments := (totalFrames + framesPerSegment - 1) / framesPerSegment

		fmt.Printf("  📐 %dx%d @ %.2f fps, %.1fs (%d frames), ~%d segments\n",
			dims.Width, dims.Height, fps, duration, totalFrames, expectedSegments)

		masterA := filepath.Join(outputDir, fmt.Sprintf("_master_%s_A.mp4", profile.Name))
		masterB := filepath.Join(outputDir, fmt.Sprintf("_master_%s_B.mp4", profile.Name))

		if err := encodeAB(scaledPath, masterA, masterB, dims, fps,
			*segmentDuration, *crf, *preset, cfg, totalFrames, resolvedAccel); err != nil {
			fmt.Fprintf(os.Stderr, "encoding %s failed: %v\n", profile.Name, err)
			os.Exit(1)
		}

		dirA := filepath.Join(outputDir, profile.Name, "A")
		dirB := filepath.Join(outputDir, profile.Name, "B")
		if err := os.MkdirAll(dirA, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "mkdir %s failed: %v\n", dirA, err)
			os.Exit(1)
		}
		if err := os.MkdirAll(dirB, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "mkdir %s failed: %v\n", dirB, err)
			os.Exit(1)
		}

		if err := packageFMP4(masterA, dirA, *segmentDuration); err != nil {
			fmt.Fprintf(os.Stderr, "packaging %s/A failed: %v\n", profile.Name, err)
			os.Exit(1)
		}
		if err := packageFMP4(masterB, dirB, *segmentDuration); err != nil {
			fmt.Fprintf(os.Stderr, "packaging %s/B failed: %v\n", profile.Name, err)
			os.Exit(1)
		}

		countA, _ := countM4S(dirA)
		countB, _ := countM4S(dirB)
		fmt.Printf("  ✓ %s: %d A segments, %d B segments\n", profile.Name, countA, countB)

		if countA != countB {
			fmt.Fprintf(os.Stderr, "  ❌ %s: A and B segment counts differ\n", profile.Name)
			os.Exit(1)
		}

		results = append(results, profileResult{
			name:      profile.Name,
			width:     dims.Width,
			height:    dims.Height,
			segmentsA: countA,
			segmentsB: countB,
		})

		if !*keepMasters {
			os.Remove(scaledPath)
			os.Remove(masterA)
			os.Remove(masterB)
		}

		fmt.Println()
	}

	refCount := results[0].segmentsA
	for _, r := range results[1:] {
		if r.segmentsA != refCount {
			fmt.Fprintf(os.Stderr,
				"❌ segment count mismatch across profiles: %s has %d, %s has %d\n",
				results[0].name, refCount, r.name, r.segmentsA)
			fmt.Fprintln(os.Stderr,
				"   All renditions of the same video must produce the same number of segments.")
			os.Exit(1)
		}
	}
	fmt.Printf("✓ All %d profiles produced %d segments each\n", len(results), refCount)

	elapsed := time.Since(start)
	fmt.Println()
	fmt.Printf("✅ Done. %d profiles × 2 variants in %.1fs\n", len(results), elapsed.Seconds())
	fmt.Printf("   Output dir: %s\n", outputDir)
}

func loadProfiles(path, inputPath string) ([]QualityProfile, error) {
	if path == "" {
		dims, _, _, err := ffmpeg.Probe(inputPath)
		if err != nil {
			return nil, fmt.Errorf("probing input: %w", err)
		}
		return []QualityProfile{{
			Name:   fmt.Sprintf("%dp", dims.Height),
			Width:  dims.Width,
			Height: dims.Height,
		}}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading profiles file: %w", err)
	}
	var profiles []QualityProfile
	if err := json.Unmarshal(raw, &profiles); err != nil {
		return nil, fmt.Errorf("parsing profiles file: %w", err)
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("no profiles in %s", path)
	}
	for i := range profiles {
		if profiles[i].Name == "" {
			profiles[i].Name = fmt.Sprintf("%dp", profiles[i].Height)
		}
		if profiles[i].Width <= 0 || profiles[i].Height <= 0 {
			return nil, fmt.Errorf("profile %d has invalid dimensions", i)
		}
	}
	return profiles, nil
}

func scaleMaster(inputPath, outputPath string, profile QualityProfile, hwAccel ffmpeg.HWAccel) error {
	args := []string{
		"-y",
		"-i", inputPath,
		"-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos", profile.Width, profile.Height),
	}
	switch hwAccel {
	case ffmpeg.HWAccelNVENC:
		args = append(args, "-c:v", "h264_nvenc", "-preset", "p1", "-rc", "vbr", "-cq", "18", "-no-scenecut", "1")
	case ffmpeg.HWAccelQSV:
		args = append(args, "-c:v", "h264_qsv", "-preset", "veryfast", "-global_quality", "18")
	case ffmpeg.HWAccelAMF:
		args = append(args, "-c:v", "h264_amf", "-quality", "speed", "-rc", "cqp", "-qp_i", "18", "-qp_p", "18")
	case ffmpeg.HWAccelVideoToolbox:
		args = append(args, "-c:v", "h264_videotoolbox", "-q:v", "18")
	default:
		args = append(args, "-c:v", "libx264", "-crf", "18", "-preset", "veryfast")
	}
	args = append(args, "-pix_fmt", "yuv420p", "-an", "-movflags", "+faststart", outputPath)

	cmd := exec.Command("ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w\n%s", err, stderr.String())
	}
	return nil
}

func encodeAB(
	inputPath, masterA, masterB string,
	dims types.FrameDimensions,
	fps, segmentDuration float64,
	crf int,
	preset string,
	cfg types.WatermarkConfig,
	totalFrames int,
	hwAccel ffmpeg.HWAccel,
) error {
	reader, err := ffmpeg.NewReader(inputPath, dims)
	if err != nil {
		return fmt.Errorf("reader: %w", err)
	}
	defer reader.Close()

	writerA, err := ffmpeg.NewWriter(dims, ffmpeg.WriterOptions{
		FPS: fps, OutputPath: masterA, CRF: crf, Preset: preset,
		KeyframeIntervalSeconds: segmentDuration, HWAccel: hwAccel,
	})
	if err != nil {
		return fmt.Errorf("writer A: %w", err)
	}

	writerB, err := ffmpeg.NewWriter(dims, ffmpeg.WriterOptions{
		FPS: fps, OutputPath: masterB, CRF: crf, Preset: preset,
		KeyframeIntervalSeconds: segmentDuration, HWAccel: hwAccel,
	})
	if err != nil {
		writerA.Close()
		return fmt.Errorf("writer B: %w", err)
	}

	var frameB *types.YUVFrame
	frameIdx := 0
	for {
		frame, err := reader.ReadFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			writerA.Close()
			writerB.Close()
			return fmt.Errorf("read frame: %w", err)
		}
		if frameB == nil {
			frameB = &types.YUVFrame{
				Y: make([]byte, len(frame.Y)),
				U: make([]byte, len(frame.U)),
				V: make([]byte, len(frame.V)),
			}
		}
		copy(frameB.Y, frame.Y)
		copy(frameB.U, frame.U)
		copy(frameB.V, frame.V)

		if _, err := watermark.EmbedSegmentBit(frame, dims, 1, cfg); err != nil {
			writerA.Close()
			writerB.Close()
			return fmt.Errorf("embed A: %w", err)
		}
		if _, err := watermark.EmbedSegmentBit(frameB, dims, 0, cfg); err != nil {
			writerA.Close()
			writerB.Close()
			return fmt.Errorf("embed B: %w", err)
		}
		if err := writerA.WriteFrame(frame); err != nil {
			writerA.Close()
			writerB.Close()
			return fmt.Errorf("write A: %w", err)
		}
		if err := writerB.WriteFrame(frameB); err != nil {
			writerA.Close()
			writerB.Close()
			return fmt.Errorf("write B: %w", err)
		}
		frameIdx++
		if totalFrames > 0 && frameIdx%30 == 0 {
			pct := float64(frameIdx) / float64(totalFrames) * 100
			if pct > 100 {
				pct = 100
			}
			fmt.Printf("\r   Frames: %d / ~%d (%.0f%%)", frameIdx, totalFrames, pct)
		}
	}
	fmt.Println()

	if err := reader.Close(); err != nil {
		return fmt.Errorf("reader close: %w", err)
	}
	if err := writerA.Close(); err != nil {
		return fmt.Errorf("writer A close: %w", err)
	}
	if err := writerB.Close(); err != nil {
		return fmt.Errorf("writer B close: %w", err)
	}
	return nil
}

func packageFMP4(masterPath, outDir string, segmentDuration float64) error {
	absMaster, err := filepath.Abs(masterPath)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", masterPath, err)
	}
	absOutDir, err := filepath.Abs(outDir)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", outDir, err)
	}

	cmd := exec.Command("ffmpeg",
		"-y",
		"-i", absMaster,
		"-c", "copy",
		"-f", "hls",
		"-hls_segment_type", "fmp4",
		"-hls_time", fmt.Sprintf("%.6f", segmentDuration),
		"-hls_playlist_type", "vod",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", "%d.m4s",
		"playlist.m3u8",
	)
	cmd.Dir = absOutDir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w\n%s", err, stderr.String())
	}
	os.Remove(filepath.Join(absOutDir, "playlist.m3u8"))
	return nil
}

func countM4S(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".m4s" {
			n++
		}
	}
	return n, nil
}
