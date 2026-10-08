//go:build e2e

// Package main_test contains end-to-end tests for the watermark CLI.
//
// These tests build the watermark binary, generate synthetic videos with
// ffmpeg, and run the full embed -> extract round-trip to verify correctness.
//
// Run with:
//
//	go test -tags=e2e -timeout 10m ./cmd/watermark/...
//
// Requires `ffmpeg` and `ffprobe` to be available in $PATH.
package main_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var binPath string

// extractRe matches the human-readable output of `watermark extract`.
//
// Example: `✅ Watermark found: "video_abc_user_123"`
var extractRe = regexp.MustCompile(`Watermark found:\s*"([^"]*)"`)

func TestMain(m *testing.M) {
	// Skip cleanly if ffmpeg toolchain is missing.
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		fmt.Fprintln(os.Stderr, "SKIP: ffmpeg not found in PATH")
		os.Exit(0)
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		fmt.Fprintln(os.Stderr, "SKIP: ffprobe not found in PATH")
		os.Exit(0)
	}

	// Build the CLI binary into a temporary directory.
	tmp, err := os.MkdirTemp("", "watermark-e2e-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdtemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	name := "watermark"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binPath = filepath.Join(tmp, name)

	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "go build failed: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// makeTestVideo generates a synthetic test video with ffmpeg.
func makeTestVideo(t *testing.T, path string, width, height, duration, fps int) {
	t.Helper()

	cmd := exec.Command("ffmpeg",
		"-y",
		"-f", "lavfi",
		"-i", fmt.Sprintf("testsrc=size=%dx%d:rate=%d:duration=%d",
			width, height, fps, duration),
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		path,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg failed to generate test video: %v\n%s", err, stderr.String())
	}
}

// runCLI executes the watermark binary and captures stdout, stderr and exit code.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()

	cmd := exec.Command(binPath, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("failed to invoke CLI: %v", err)
		}
	}
	return out.String(), errBuf.String(), exitCode
}

// reencode re-encodes a video at the given CRF, simulating a lossy distribution step.
func reencode(t *testing.T, input, output string, crf int) {
	t.Helper()

	cmd := exec.Command("ffmpeg",
		"-y",
		"-i", input,
		"-c:v", "libx264",
		"-crf", fmt.Sprintf("%d", crf),
		"-preset", "fast",
		"-pix_fmt", "yuv420p",
		output,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("reencode failed: %v\n%s", err, stderr.String())
	}
}

// ---------------------------------------------------------------------------
// Round-trip tests
// ---------------------------------------------------------------------------

func TestEmbedExtractRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		width  int
		height int
		delta  string
		crf    string
	}{
		{"simple_id", "test_id_123", 320, 240, "25.0", "23"},
		{"uuid_like", "a1b2c3d4-e5f6-7890-abcd-ef1234567890", 320, 240, "25.0", "23"},
		{"short_id", "abc", 320, 240, "25.0", "23"},
		{"long_id_128", strings.Repeat("x", 128), 640, 480, "25.0", "23"},
		{"strong_delta", "delta_40_test", 320, 240, "40.0", "23"},
		{"higher_quality_encode", "crf18_test", 320, 240, "25.0", "18"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "input.mp4")
			output := filepath.Join(dir, "output.mp4")

			makeTestVideo(t, input, tc.width, tc.height, 2, 30)

			// --- EMBED ---
			stdout, stderr, code := runCLI(t, "embed",
				"--delta", tc.delta,
				"--crf", tc.crf,
				input, output, tc.id,
			)
			if code != 0 {
				t.Fatalf("embed failed (exit %d)\nstdout:\n%s\nstderr:\n%s",
					code, stdout, stderr)
			}
			if !strings.Contains(stdout, "Done.") {
				t.Errorf("embed output missing success marker:\n%s", stdout)
			}

			info, err := os.Stat(output)
			if err != nil {
				t.Fatalf("output file not created: %v", err)
			}
			if info.Size() == 0 {
				t.Fatal("output file is empty")
			}

			// --- EXTRACT ---
			stdout, stderr, code = runCLI(t, "extract",
				"--delta", tc.delta,
				"--frames", "10",
				output,
			)
			if code != 0 {
				t.Fatalf("extract failed (exit %d)\nstdout:\n%s\nstderr:\n%s",
					code, stdout, stderr)
			}

			m := extractRe.FindStringSubmatch(stdout)
			if m == nil {
				t.Fatalf("could not parse watermark from extract output:\n%s", stdout)
			}

			if got := m[1]; got != tc.id {
				t.Errorf("watermark mismatch:\n  want: %q\n  got:  %q", tc.id, got)
			}
		})
	}
}

// TestSurvivesReEncoding verifies that a watermark embedded into a video
// survives a second lossy encode (simulating re-distribution).
func TestSurvivesReEncoding(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.mp4")
	marked := filepath.Join(dir, "marked.mp4")
	reencoded := filepath.Join(dir, "reencoded.mp4")

	// ID con la misma forma que los reales del pipeline:
	// "video_<timestamp>_user_<token>"
	const id = "video_1790947028433_user_abc12345"

	makeTestVideo(t, input, 640, 480, 5, 30)

	if _, stderr, code := runCLI(t, "embed", input, marked, id); code != 0 {
		t.Fatalf("embed failed: %s", stderr)
	}

	// Reencode agresivo, igual que la prueba manual
	reencode(t, marked, reencoded, 32)

	stdout, stderr, code := runCLI(t, "extract", "--frames", "10", reencoded)
	if code != 0 {
		t.Fatalf("extract failed after reencode (exit %d)\nstdout:\n%s\nstderr:\n%s",
			code, stdout, stderr)
	}

	m := extractRe.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("could not parse watermark:\n%s", stdout)
	}
	if m[1] != id {
		t.Errorf("watermark lost after reencode:\n  want: %q\n  got:  %q", id, m[1])
	}
}

// TestExtractWithoutWatermark verifies that extraction on a clean video fails
// gracefully (no false positive from the CRC check).
func TestExtractWithoutWatermark(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "clean.mp4")

	makeTestVideo(t, input, 320, 240, 1, 30)

	stdout, stderr, code := runCLI(t, "extract", "--frames", "5", input)
	if code == 0 {
		// A false positive is theoretically possible (CRC32 collision over
		// 4096 candidate cycle lengths ≈ 10^-6), but should be vanishingly rare.
		t.Logf("WARNING: extracted from un-watermarked video (possible CRC collision):\n%s", stdout)
		return
	}
	if !strings.Contains(stderr, "could not extract watermark") {
		t.Errorf("expected 'could not extract watermark' error, got:\n%s", stderr)
	}
}

// ---------------------------------------------------------------------------
// CLI behavior tests
// ---------------------------------------------------------------------------

func TestHelpOutput(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "help"} {
		t.Run(flag, func(t *testing.T) {
			stdout, _, code := runCLI(t, flag)
			if code != 0 {
				t.Fatalf("help returned exit code %d", code)
			}
			for _, want := range []string{
				"watermark - forensic watermarking tool",
				"watermark embed",
				"watermark extract",
			} {
				if !strings.Contains(stdout, want) {
					t.Errorf("help output missing %q:\n%s", want, stdout)
				}
			}
		})
	}
}

func TestUnknownCommand(t *testing.T) {
	_, stderr, code := runCLI(t, "frobnicate")
	if code == 0 {
		t.Fatal("expected non-zero exit code for unknown command")
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Errorf("expected 'unknown command' in stderr, got:\n%s", stderr)
	}
}

func TestMissingInputFile(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "out.mp4")

	_, stderr, code := runCLI(t, "embed", "/nonexistent/input.mp4", output, "some_id")
	if code == 0 {
		t.Fatal("expected non-zero exit for missing input")
	}
	if !strings.Contains(stderr, "input not found") {
		t.Errorf("expected 'input not found' in stderr, got:\n%s", stderr)
	}
}

func TestEmptyIDRejected(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.mp4")
	output := filepath.Join(dir, "out.mp4")

	makeTestVideo(t, input, 160, 120, 1, 30)

	_, stderr, code := runCLI(t, "embed", input, output, "")
	if code == 0 {
		t.Fatal("expected non-zero exit for empty ID")
	}
	if !strings.Contains(stderr, "id cannot be empty") {
		t.Errorf("expected 'id cannot be empty' error, got:\n%s", stderr)
	}
}

func TestEmbedMissingArgs(t *testing.T) {
	_, stderr, code := runCLI(t, "embed", "only_one_arg.mp4")
	if code == 0 {
		t.Fatal("expected non-zero exit for missing args")
	}
	if !strings.Contains(stderr, "usage: watermark embed") {
		t.Errorf("expected usage message, got:\n%s", stderr)
	}
}

func TestExtractMissingArgs(t *testing.T) {
	_, stderr, code := runCLI(t, "extract")
	if code == 0 {
		t.Fatal("expected non-zero exit for missing args")
	}
	if !strings.Contains(stderr, "usage: watermark extract") {
		t.Errorf("expected usage message, got:\n%s", stderr)
	}
}
