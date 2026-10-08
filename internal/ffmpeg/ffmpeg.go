package ffmpeg

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"

	"github.com/rtagliaviaz/watermark-service/internal/types"
)

// Probe reads the video's dimensions, frame rate, and duration using ffprobe.
func Probe(inputPath string) (types.FrameDimensions, float64, float64, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,r_frame_rate,nb_frames",
		"-show_entries", "format=duration",
		"-of", "json",
		inputPath,
	)

	out, err := cmd.Output()
	if err != nil {
		return types.FrameDimensions{}, 0, 0, fmt.Errorf("ffprobe failed: %w", err)
	}

	var probe struct {
		Streams []struct {
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			RFrameRate string `json:"r_frame_rate"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}

	if err := json.Unmarshal(out, &probe); err != nil {
		return types.FrameDimensions{}, 0, 0, fmt.Errorf("parsing ffprobe output: %w", err)
	}

	if len(probe.Streams) == 0 {
		return types.FrameDimensions{}, 0, 0, errors.New("no video stream found")
	}

	stream := probe.Streams[0]
	fps, err := parseFraction(stream.RFrameRate)
	if err != nil {
		return types.FrameDimensions{}, 0, 0, fmt.Errorf("parsing frame rate: %w", err)
	}

	duration, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil {
		return types.FrameDimensions{}, 0, 0, fmt.Errorf("parsing duration: %w", err)
	}

	return types.FrameDimensions{
		Width:  stream.Width,
		Height: stream.Height,
	}, fps, duration, nil
}

// parseFraction converts "30000/1001" into 29.97...
func parseFraction(s string) (float64, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			num, err := strconv.ParseFloat(s[:i], 64)
			if err != nil {
				return 0, err
			}
			den, err := strconv.ParseFloat(s[i+1:], 64)
			if err != nil {
				return 0, err
			}
			return num / den, nil
		}
	}
	return strconv.ParseFloat(s, 64)
}

// Reader wraps a running ffmpeg process that decodes a video into raw YUV420p
// frames on stdout.
type Reader struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	reader *bufio.Reader
	dims   types.FrameDimensions
}

// NewReader launches ffmpeg to decode the input video into raw YUV420p frames.
func NewReader(inputPath string, dims types.FrameDimensions) (*Reader, error) {
	cmd := exec.Command("ffmpeg",
		"-i", inputPath,
		"-f", "rawvideo",
		"-pix_fmt", "yuv420p",
		"-",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ffmpeg: %w", err)
	}

	return &Reader{
		cmd:    cmd,
		stdout: stdout,
		reader: bufio.NewReaderSize(stdout, 64*1024),
		dims:   dims,
	}, nil
}

// ReadFrame reads the next YUV420p frame from the decoder.
// Returns io.EOF when there are no more frames.
func (r *Reader) ReadFrame() (*types.YUVFrame, error) {
	ySize := r.dims.Width * r.dims.Height
	uvSize := (r.dims.Width / 2) * (r.dims.Height / 2)

	frame := &types.YUVFrame{
		Y: make([]byte, ySize),
		U: make([]byte, uvSize),
		V: make([]byte, uvSize),
	}

	if _, err := io.ReadFull(r.reader, frame.Y); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("reading Y plane: %w", err)
	}
	if _, err := io.ReadFull(r.reader, frame.U); err != nil {
		return nil, fmt.Errorf("reading U plane: %w", err)
	}
	if _, err := io.ReadFull(r.reader, frame.V); err != nil {
		return nil, fmt.Errorf("reading V plane: %w", err)
	}

	return frame, nil
}

// Close waits for the ffmpeg process to finish.
func (r *Reader) Close() error {
	r.stdout.Close()
	return r.cmd.Wait()
}

// Writer wraps a running ffmpeg process that encodes raw YUV420p frames from
// stdin into a video file.
type Writer struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	writer *bufio.Writer
	dims   types.FrameDimensions
}

// WriterOptions configures the encoder.
type WriterOptions struct {
	// FPS is the frame rate to encode at.
	FPS float64
	// OutputPath is where the encoded video will be written.
	OutputPath string
	// CRF is the quality (lower = better, 18-28 is typical).
	CRF int
	// Preset controls encoding speed vs compression (ultrafast, fast, medium, slow).
	Preset string
	// KeyframeIntervalSeconds, if > 0, forces keyframes at this interval
	// (in seconds) AND limits the GOP size accordingly. Required when the
	// output will be segmented by an external packager (ffmpeg HLS muxer,
	// Shaka Packager, mp4fragment), because those tools can only split on
	// keyframes.
	//
	// This mirrors the behavior of video-streaming-service's transcodeToMp4,
	// which uses -g (fps*2) and -force_key_frames expr:gte(t,n_forced*2).
	KeyframeIntervalSeconds float64

	// HWAccel selects the hardware acceleration backend. HWAccelNone (or
	// empty string) uses libx264 and is the safest default. Use
	// HWAccelAuto to let the library pick the first available GPU.
	HWAccel HWAccel
}

// NewWriter launches ffmpeg to encode raw YUV420p frames from stdin into a
// video file.
func NewWriter(dims types.FrameDimensions, opts WriterOptions) (*Writer, error) {
	if opts.CRF == 0 {
		opts.CRF = 23
	}
	if opts.Preset == "" {
		opts.Preset = "fast"
	}
	if opts.HWAccel == "" {
		opts.HWAccel = HWAccelNone
	}

	args := []string{
		"-y",
		"-f", "rawvideo",
		"-pix_fmt", "yuv420p",
		"-s", fmt.Sprintf("%dx%d", dims.Width, dims.Height),
		"-r", strconv.FormatFloat(opts.FPS, 'f', 6, 64),
		"-i", "-",
	}

	if opts.HWAccel == HWAccelNone {
		// Legacy path: bit-exact flags for deterministic output.
		args = append(args, "-fflags", "+bitexact")
	}

	args = append(args,
		hwAccelEncoderArgs(opts.HWAccel, opts.CRF, opts.Preset,
			opts.KeyframeIntervalSeconds, opts.FPS)...,
	)

	args = append(args, "-pix_fmt", "yuv420p")
	args = append(args, "-movflags", "+faststart", opts.OutputPath)

	cmd := exec.Command("ffmpeg", args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdin pipe: %w", err)
	}

	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ffmpeg: %w", err)
	}

	return &Writer{
		cmd:    cmd,
		stdin:  stdin,
		writer: bufio.NewWriterSize(stdin, 64*1024),
		dims:   dims,
	}, nil
}

// WriteFrame writes a YUV420p frame to the encoder.
func (w *Writer) WriteFrame(frame *types.YUVFrame) error {
	if _, err := w.writer.Write(frame.Y); err != nil {
		return fmt.Errorf("writing Y plane: %w", err)
	}
	if _, err := w.writer.Write(frame.U); err != nil {
		return fmt.Errorf("writing U plane: %w", err)
	}
	if _, err := w.writer.Write(frame.V); err != nil {
		return fmt.Errorf("writing V plane: %w", err)
	}
	return nil
}

// Close flushes buffers and waits for ffmpeg to finish writing the output.
func (w *Writer) Close() error {
	if err := w.writer.Flush(); err != nil {
		return fmt.Errorf("flushing writer: %w", err)
	}
	if err := w.stdin.Close(); err != nil {
		return fmt.Errorf("closing stdin: %w", err)
	}
	return w.cmd.Wait()
}

func strconvItoa(n int) string {
	return strconv.Itoa(n)
}

func strconvFtoa(f float64) string {
	return strconv.FormatFloat(f, 'f', 3, 64)
}
