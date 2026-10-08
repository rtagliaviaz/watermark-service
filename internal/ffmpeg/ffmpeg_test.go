package ffmpeg

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// makeTestVideo generates a small 5-second test video with ffmpeg.
func makeTestVideo(t *testing.T, path string) {
	t.Helper()
	cmd := exec.Command("ffmpeg",
		"-y",
		"-f", "lavfi",
		"-i", "testsrc=duration=5:size=320x240:rate=30",
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to create test video: %v\n%s", err, out)
	}
}

// TestProbe verifies ffprobe reports the expected dimensions and fps.
func TestProbe(t *testing.T) {
	tmp := t.TempDir()
	input := filepath.Join(tmp, "input.mp4")
	makeTestVideo(t, input)

	dims, fps, duration, err := Probe(input)
	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}

	if dims.Width != 320 || dims.Height != 240 {
		t.Errorf("expected 320x240, got %dx%d", dims.Width, dims.Height)
	}
	if fps < 29.9 || fps > 30.1 {
		t.Errorf("expected ~30 fps, got %f", fps)
	}
	if duration < 4.9 || duration > 5.1 {
		t.Errorf("expected ~5s duration, got %f", duration)
	}
}

// TestReaderWriterRoundTrip verifies that decoding + re-encoding produces a
// video of the same dimensions and frame count.
func TestReaderWriterRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	input := filepath.Join(tmp, "input.mp4")
	output := filepath.Join(tmp, "output.mp4")
	makeTestVideo(t, input)

	dims, fps, _, err := Probe(input)
	if err != nil {
		t.Fatal(err)
	}

	// Decode all frames
	reader, err := NewReader(input, dims)
	if err != nil {
		t.Fatal(err)
	}

	writer, err := NewWriter(dims, WriterOptions{
		FPS:        fps,
		OutputPath: output,
		CRF:        23,
		Preset:     "ultrafast",
	})
	if err != nil {
		t.Fatal(err)
	}

	frameCount := 0
	for {
		frame, err := reader.ReadFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadFrame failed: %v", err)
		}
		frameCount++

		if err := writer.WriteFrame(frame); err != nil {
			t.Fatalf("WriteFrame failed: %v", err)
		}
	}

	if err := reader.Close(); err != nil {
		t.Fatalf("Reader.Close failed: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Writer.Close failed: %v", err)
	}

	// Verify the output exists and has the right dimensions
	info, err := os.Stat(output)
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("output file is empty")
	}

	outDims, _, _, err := Probe(output)
	if err != nil {
		t.Fatal(err)
	}
	if outDims.Width != dims.Width || outDims.Height != dims.Height {
		t.Errorf("dimension mismatch: input %dx%d, output %dx%d",
			dims.Width, dims.Height, outDims.Width, outDims.Height)
	}

	// A 5-second video at 30 fps should have ~150 frames
	if frameCount < 140 || frameCount > 160 {
		t.Errorf("expected ~150 frames, got %d", frameCount)
	}
}
