package ffmpeg

import (
	"os/exec"
	"strings"
)

// HWAccel identifies a hardware acceleration backend for H.264 encoding.
type HWAccel string

const (
	HWAccelNone         HWAccel = "none"
	HWAccelAuto         HWAccel = "auto"
	HWAccelNVENC        HWAccel = "nvenc"
	HWAccelQSV          HWAccel = "qsv"
	HWAccelAMF          HWAccel = "amf"
	HWAccelVAAPI        HWAccel = "vaapi"
	HWAccelVideoToolbox HWAccel = "videotoolbox"
)

// ParseHWAccel converts a user-provided string to an HWAccel, or returns an
// empty string if the value is not recognized.
func ParseHWAccel(s string) HWAccel {
	switch strings.ToLower(s) {
	case "none", "":
		return HWAccelNone
	case "auto":
		return HWAccelAuto
	case "nvenc":
		return HWAccelNVENC
	case "qsv":
		return HWAccelQSV
	case "amf":
		return HWAccelAMF
	case "vaapi":
		return HWAccelVAAPI
	case "videotoolbox", "vtb":
		return HWAccelVideoToolbox
	}
	return ""
}

// ResolveHWAccel returns the HWAccel to use for a given request. If the
// request is Auto, it probes each backend in priority order and returns the
// first one that works. If the request is explicit, it is returned as-is
// without verification: the caller is responsible for handling encoder
// failures.
//
// A nil logger means silent operation.
func ResolveHWAccel(requested HWAccel) HWAccel {
	if requested != HWAccelAuto {
		return requested
	}
	// Order: NVENC > QSV > AMF > VideoToolbox. VAAPI is excluded from
	// auto-detection because it requires a display server on most Linux
	// setups and its probe is unreliable in headless environments.
	for _, candidate := range []HWAccel{
		HWAccelNVENC,
		HWAccelQSV,
		HWAccelAMF,
		HWAccelVideoToolbox,
	} {
		if HWAccelAvailable(candidate) {
			return candidate
		}
	}
	return HWAccelNone
}

// HWAccelAvailable probes whether an encoder for the given backend can
// successfully encode a single frame. Returns false on any error.
//
// The probe uses 320x240 rather than a smaller size because NVENC rejects
// frames below roughly 145x49, and 320x240 is well above every backend's
// minimum while still finishing in milliseconds.
func HWAccelAvailable(accel HWAccel) bool {
	enc := encoderForAccel(accel)
	if enc == "" {
		return false
	}
	cmd := exec.Command("ffmpeg",
		"-hide_banner",
		"-loglevel", "error",
		"-f", "lavfi",
		"-i", "nullsrc=s=320x240:d=0.1:r=30",
		"-c:v", enc,
		"-frames:v", "1",
		"-f", "null",
		"-",
	)
	return cmd.Run() == nil
}

func encoderForAccel(accel HWAccel) string {
	switch accel {
	case HWAccelNVENC:
		return "h264_nvenc"
	case HWAccelQSV:
		return "h264_qsv"
	case HWAccelAMF:
		return "h264_amf"
	case HWAccelVAAPI:
		return "h264_vaapi"
	case HWAccelVideoToolbox:
		return "h264_videotoolbox"
	}
	return ""
}

// hwAccelEncoderArgs returns the encoder-specific arguments for a given
// backend. The arguments are appended after the input flags and before any
// container-level flags.
//
// The goal is to match the visual quality and keyframe behavior of the
// libx264 path as closely as possible, so that swapping backends does not
// change the watermark's robustness or the segment boundaries.
func hwAccelEncoderArgs(
	accel HWAccel,
	crf int,
	preset string,
	keyframeIntervalSeconds float64,
	fps float64,
) []string {
	switch accel {
	case HWAccelNVENC:
		return nvencArgs(crf, preset, keyframeIntervalSeconds, fps)
	case HWAccelQSV:
		return qsvArgs(crf, preset, keyframeIntervalSeconds, fps)
	case HWAccelAMF:
		return amfArgs(crf, preset, keyframeIntervalSeconds, fps)
	case HWAccelVideoToolbox:
		return vtbArgs(crf, preset, keyframeIntervalSeconds, fps)
	default:
		return x264Args(crf, preset, keyframeIntervalSeconds, fps)
	}
}

// x264Args mirrors the software path used by video-streaming-service when no
// GPU is available: -g, -strict_gop, -force_key_frames.
func x264Args(crf int, preset string, keyframeIntervalSeconds, fps float64) []string {
	args := []string{
		"-c:v", "libx264",
		"-flags", "+bitexact",
		"-crf", itoa(crf),
		"-preset", preset,
	}
	args = append(args, keyframeArgs(keyframeIntervalSeconds, fps, keyframeModeX264)...)
	return args
}

// nvencArgs mirrors video-streaming-service's nvenc path: VBR, CQ, spatial
// and temporal AQ, and no scenecut. NVENC does not support -strict_gop, so
// instead we set -no-scenecut 1 and -forced-idr 1 to force deterministic
// keyframe placement.
func nvencArgs(crf int, preset string, keyframeIntervalSeconds, fps float64) []string {
	// Map x264 preset names to NVENC preset names. NVENC uses p1 (fastest)
	// through p7 (slowest). We use conservative mappings so that visual
	// quality stays close to the software path.
	nvencPreset := "p4" // medium
	switch preset {
	case "ultrafast", "superfast", "veryfast":
		nvencPreset = "p1"
	case "faster", "fast":
		nvencPreset = "p3"
	case "medium":
		nvencPreset = "p4"
	case "slow", "slower":
		nvencPreset = "p6"
	case "veryslow":
		nvencPreset = "p7"
	}

	args := []string{
		"-c:v", "h264_nvenc",
		"-rc", "vbr",
		"-cq", itoa(crf),
		"-preset", nvencPreset,
		"-spatial_aq", "1",
		"-temporal_aq", "1",
		"-rc-lookahead", "32",
		// Critical for deterministic segmentation: no scenecut-driven
		// extra I-frames.
		"-no-scenecut", "1",
		"-b_ref_mode", "0",
	}
	args = append(args, keyframeArgs(keyframeIntervalSeconds, fps, keyframeModeNVENC)...)
	return args
}

func qsvArgs(crf int, preset string, keyframeIntervalSeconds, fps float64) []string {
	args := []string{
		"-c:v", "h264_qsv",
		"-global_quality", itoa(crf),
		"-look_ahead", "0",
	}
	args = append(args, keyframeArgs(keyframeIntervalSeconds, fps, keyframeModeQSV)...)
	return args
}

func amfArgs(crf int, preset string, keyframeIntervalSeconds, fps float64) []string {
	args := []string{
		"-c:v", "h264_amf",
		"-rc", "cqp",
		"-qp_i", itoa(crf),
		"-qp_p", itoa(crf),
		"-quality", "balanced",
	}
	args = append(args, keyframeArgs(keyframeIntervalSeconds, fps, keyframeModeAMF)...)
	return args
}

func vtbArgs(crf int, preset string, keyframeIntervalSeconds, fps float64) []string {
	args := []string{
		"-c:v", "h264_videotoolbox",
		"-q:v", itoa(crf),
	}
	args = append(args, keyframeArgs(keyframeIntervalSeconds, fps, keyframeModeVTB)...)
	return args
}

// keyframeMode selects which keyframe-forcing flags to emit for a backend.
type keyframeMode int

const (
	keyframeModeX264  keyframeMode = iota // -g -strict_gop -force_key_frames
	keyframeModeNVENC                     // -g -forced-idr -force_key_frames
	keyframeModeQSV                       // -g -forced_idr
	keyframeModeAMF                       // -g
	keyframeModeVTB                       // -g
)

func keyframeArgs(seconds, fps float64, mode keyframeMode) []string {
	if seconds <= 0 {
		return nil
	}
	gopFrames := int(seconds * fps)
	if gopFrames < 1 {
		gopFrames = 1
	}
	args := []string{"-g", itoa(gopFrames)}

	switch mode {
	case keyframeModeX264:
		args = append(args,
			"-strict_gop", "1",
			"-force_key_frames",
			forceKeyFramesExpr(seconds),
		)
	case keyframeModeNVENC:
		args = append(args,
			"-forced-idr", "1",
			"-force_key_frames",
			forceKeyFramesExpr(seconds),
		)
	case keyframeModeQSV:
		args = append(args, "-forced_idr", "1")
	case keyframeModeAMF, keyframeModeVTB:
		// These backends honor -g only. The keyframe interval is still
		// deterministic because fps and gop are both fixed.
	}
	return args
}

func forceKeyFramesExpr(seconds float64) string {
	return "expr:gte(t,n_forced*" + ftoa(seconds) + ")"
}

// itoa and ftoa are tiny helpers to keep the argument builders readable
// without importing strconv in this file.
func itoa(n int) string {
	return strconvItoa(n)
}

func ftoa(f float64) string {
	return strconvFtoa(f)
}
