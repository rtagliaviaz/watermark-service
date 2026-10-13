# watermark-service

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Forensic and A/B watermarking for video, implemented in pure Go.

`watermark-service` is a CLI tool that embeds invisible identifiers into video
streams for two distinct purposes:

- **Forensic watermarking** — embed a full ID into every frame, robust enough
  to survive a second lossy encode. Used to trace a leaked copy back to the
  specific recipient it was delivered to.
- **A/B watermarking** — embed a single bit per video segment, so each viewer
  receives a unique combination of segment variants. Used to trace which
  session a leaked stream came from, at scale.

The tool implements its own 2D DCT, CRC32-framed ID codec, and multi-frame
voting scheme. It does not depend on any watermarking library.


## Table of contents

- [How it works](#how-it-works)
- [Forensic watermarking](#forensic-watermarking)
- [A/B watermarking](#ab-watermarking)
- [Installation](#installation)
- [Usage](#usage)
  - [Forensic commands](#forensic-commands)
  - [A/B commands](#ab-commands)
  - [Robustness enhancements](#robustness-enhancements)
    - [Multi-coefficient embedding](#multi-coefficient-embedding)
    - [Reed–Solomon FEC](#reedsolomon-fec)
    - [Verifying the enhancements](#verifying-the-enhancements)
  - [Minimum video length and segment duration](#minimum-video-length-and-segment-duration)
- [Performance](#performance)
- [Design decisions](#design-decisions)
- [Testing](#testing)
- [Limitations and future work](#limitations-and-future-work)
- [License](#license)


## How it works

The core idea is the same for both modes: embed information in the **sign** of
a mid-frequency DCT coefficient of every 8×8 block of the luma plane.

### The DCT

The Discrete Cosine Transform converts a block of 8×8 pixels into 64 frequency
coefficients. It is the same transform that JPEG and H.264 use internally, which
is why a watermark hidden in the DCT domain survives the transformations that
the codec applies.

- Coefficient **(0,0)** is the average brightness of the block. Changing it
  would be visible.
- Coefficients **(7,7)** are very high frequency, and H.264 quantizes them
  aggressively. Anything embedded there is destroyed by the first recompression.
- Coefficient **(4,4)** is the sweet spot. High enough in frequency to be
  invisible to the eye, low enough to survive aggressive compression.

### Sign modulation

Instead of adding a fixed value to the coefficient, the tool forces its
**sign**:

```
bit = 1  →  c' = +|c| + Δ
bit = 0  →  c' = −|c| − Δ
```

This is much more robust than additive watermarking. The magnitude of the
coefficient may change drastically after quantization, but its **direction**
usually survives because H.264 preserves the sign of the signal.

### Parallel DCT

Blocks within a frame are independent: modifying block (5,3) does not affect
block (5,4). The frame's block grid is therefore split into horizontal bands,
each processed by a separate goroutine with no synchronization required:

```go
workers := runtime.NumCPU()
rowsPerWorker := (blocksPerCol + workers - 1) / workers
// ... launch one goroutine per band
```

The speedup is roughly linear in the number of cores, with no locks because
each goroutine writes to a disjoint region of the luma plane.

## Forensic watermarking

The forensic mode embeds a **complete ID** into every frame, cycling through
all its bits across the frame's blocks. It is designed for scenarios where the
number of copies is small and controlled: screeners sent to reviewers,
deliveries to distributors, samples shared under NDA.

### ID framing

IDs are encoded as a length-prefixed, CRC32-verified bit stream:

```
[4 bytes length][N bytes payload][4 bytes CRC32]
```

- The **length prefix** tells the decoder how many bytes to read. Without it,
  there is no way to distinguish a valid ID from random bits.
- The **CRC32** is the arbiter. The extractor does not know the ID length in
  advance, so it tries every candidate cycle length between 72 and 4096 bits
  and returns the first one whose CRC32 matches. The probability of a false
  positive is ~2.3 × 10⁻¹⁰.

### Redundancy

For a 640×480 video, a frame contains 4,800 blocks. A typical 352-bit ID is
therefore embedded **~13 times per frame**. Aggregating votes over 10 frames
gives roughly 130 samples per bit, which is what allows the majority vote to
absorb the noise introduced by recompression.


## A/B watermarking

The A/B mode is designed for streaming at scale, where embedding a full ID
per frame is impractical.

Instead of generating one personalized copy per viewer, the video is prepared
once as **two variants of each segment**:

- `A` — bit `1` embedded in every block of the segment.
- `B` — bit `0` embedded in every block of the segment.

Each viewer then receives a **unique combination** of A and B segments,
derived from the hash of their session ID. The playlist that the player
receives is assembled on demand and points to the correct variant per segment.

### Why this scales

- Storage is **constant** regardless of the number of viewers: two variants
  per segment, always.
- The bandwidth cost of serving a session is identical to serving a single
  static playlist.
- Traceability is per-session. Extracting from a leaked stream returns the
  hash of the session ID, which the operator resolves through a registry.

### Session IDs are hashed, not embedded directly

A raw session ID such as `user_abc_1735000000000_a1b2c3d4` frames to 312 bits,
which requires over 10 minutes of video at 2-second segments. To make the
required video duration independent of the ID length, the tool hashes the
session ID to a **fixed 16-character string** (SHA-256 truncated to 12 bytes,
base64-url encoded), which frames to exactly 192 bits.

The mapping `hash → original ID` is stored in a registry (JSON file). After
extraction, the operator looks up the hash to recover the human-readable
session ID.


### Diagram

```text
                 ┌──────────────┐
  master.mp4 ──> │  ab-prepare  │
                 └──────┬───────┘
                        │  (per quality profile)
                 ┌──────┴──────┐
                 ▼             ▼
           segments/A/    segments/B/
           (bit=1)        (bit=0)
           init.mp4       init.mp4
           0.m4s, 1.m4s…  0.m4s, 1.m4s…
                 │             │
                 └──────┬──────┘
                        │
                 ┌──────▼───────┐
   session_id ──>│ ab-assemble  │  (hash → bits → pick A or B per segment)
                 └──────┬───────┘
                        ▼
             session/<quality>.m3u8    personalized playlist
             session/master.m3u8       ABR manifest
             registry.json             hash → session_id
                        │
                        ▼
                   session.mp4  ─────┐
                                     │ (leaked)
                                     ▼
                              ┌──────────────┐
                              │  ab-extract  │
                              └──────┬───────┘
                                     ▼
                              "user_abc_..."
```
## Installation

Requires Go 1.25 or later, and `ffmpeg` + `ffprobe` in `$PATH`.

```bash
git clone https://github.com/rtagliaviaz/watermark-service.git
cd watermark-service
go build -o watermark ./cmd/watermark
```

On Windows:

```powershell
go build -o watermark.exe ./cmd/watermark
```


## Usage

### Forensic commands

#### `embed`

```bash
watermark embed <input> <output> <id> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--crf int` | 23 | x264 CRF quality (lower is better) |
| `--preset string` | `fast` | x264 preset |
| `--delta float` | 25.0 | DCT coefficient strength |
| `--u int` | 4 | Coefficient U position (0–7) |
| `--v int` | 4 | Coefficient V position (0–7) |

```bash
watermark embed input.mp4 output.mp4 "video_1790947028433_user_abc12345"
```

#### `extract`

```bash
watermark extract <input> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--frames int` | 10 | Number of frames to aggregate for voting |
| `--delta float` | 25.0 | Must match `embed` |
| `--u int` | 4 | Must match `embed` |
| `--v int` | 4 | Must match `embed` |

```bash
watermark extract output.mp4
```

Example output:

```
🔍 Extracting watermark from: output.mp4
📐 Dimensions: 640x480
🔢 Frames aggregated: 10

✅ Watermark found: "video_1790947028433_user_abc12345"
```

### A/B commands

#### `ab-prepare`

```bash
watermark ab-prepare <input> <output-dir> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--profiles string` | | JSON file with quality profiles |
| `--hwaccel string` | `none` | `none`, `auto`, `nvenc`, `qsv`, `amf`, `vaapi`, `videotoolbox` |
| `--segment-duration float` | 2.0 | Segment duration in seconds |
| `--crf int` | 23 | x264 CRF quality |
| `--preset string` | `fast` | x264 preset |
| `--keep-masters` | false | Keep intermediate scaled and master files |

Profiles file format:

```json
[
  {"name": "720p", "width": 1280, "height": 720},
  {"name": "480p", "width": 854,  "height": 480},
  {"name": "360p", "width": 640,  "height": 360}
]
```

```bash
watermark ab-prepare master.mp4 segments/ --profiles profiles.json --hwaccel auto
```

Output structure:

```
segments/
  720p/A/init.mp4  720p/A/0.m4s  720p/A/1.m4s  ...
  720p/B/init.mp4  720p/B/0.m4s  720p/B/1.m4s  ...
  480p/A/...
  480p/B/...
  360p/A/...
  360p/B/...
```

#### `ab-assemble`

```bash
watermark ab-assemble <segments-dir> <session-id> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--output string` | `session_<id>/` | Output directory |
| `--registry string` | | Optional registry file to record `hash → id` |
| `--segment-duration float` | 2.0 | Must match `ab-prepare` |

```bash
watermark ab-assemble segments/ "user_abc_1735000000000_a1b2c3d4" \
    --output session_x \
    --registry registry.json
```

Output structure:

```
session_x/
  master.m3u8      ← master playlist with all renditions
  720p.m3u8        ← personalized media playlist
  480p.m3u8
  360p.m3u8
```

To materialize the session into a playable MP4:

```bash
ffmpeg -y -i session_x/master.m3u8 -c copy session.mp4
```

#### `ab-extract`

```bash
watermark ab-extract <input> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--registry string` | | Optional registry to resolve hash → ID |
| `--segment-duration float` | 2.0 | Must match `ab-prepare` |

```bash
watermark ab-extract --registry registry.json session.mp4
```

Example output:

```
🔍 Extracting session from: session.mp4
📐 1280x720 @ 30.00 fps, 420.0s
🔢 Segmenting into 210 segments of 60 frames

   Segments: 210 / 210 (100%)  last: bit=0 conf=1.00

✅ Session hash: "zvVwtWvTDCx1_JXS"
✅ Original session ID: "user_abc_1735000000000_a1b2c3d4"
   Bits collected: 210 segments in 44.3s
```


### Robustness enhancements

Two optional mechanisms extend the watermark's tolerance to corruption. They
are independent and can be combined.

#### Multi-coefficient embedding

When `--multi-coefficient` is passed to `ab-prepare` and `ab-extract`, the
same bit is embedded at three DCT positions — `(4,4)`, `(3,5)`, and `(5,3)` —
instead of one. Extraction votes across all three positions per block,
tripling the number of samples per bit. Robustness improves without any change
to the required video duration, at the cost of roughly 3× slower embedding and
extraction.

#### Reed–Solomon FEC

When `--fec-parity N` is passed to `ab-assemble` and `ab-extract`, N bytes of
Reed–Solomon parity are appended to the framed session hash. The decoder can
then correct up to N/2 byte errors before the CRC32 check, instead of merely
detecting them. Recommended value: 16. The cost is a longer payload, so the
video must be correspondingly longer — see [Minimum video length](#minimum-video-length-and-segment-duration).

#### Verifying the enhancements

The commands below assume:
- A source video `master.mp4` of any length.
- A profiles file `profiles_single.json`:
  ```json
  [{"name": "720p", "width": 1280, "height": 720}]
  ```
- PowerShell (Windows). On Linux/macOS, replace `.\watermark.exe` with
  `./watermark` and use forward slashes.

**1. Multi-coefficient only**

```powershell
go build -o watermark.exe ./cmd/watermark
Remove-Item -Recurse -Force segments_test, session_drm

# Prepare with multi-coefficient
.\watermark.exe ab-prepare master.mp4 segments_test --profiles profiles_single.json `
    --hwaccel auto --multi-coefficient

# Assemble (no FEC)
.\watermark.exe ab-assemble --output session_drm --registry registry.json `
    segments_test "user_abc_1735000000000_a1b2c3d4"

# Extract (must use multi-coefficient too)
ffmpeg -y -i session_drm\720p.m3u8 -c copy session.mp4
.\watermark.exe ab-extract --registry registry.json --multi-coefficient session.mp4
```

Expected: extraction recovers the original session ID with confidence close
to 1.00. The session hash and ID are identical to a run without the flag.

**2. FEC only**

FEC requires a longer payload, which in turn requires more segments. If the
source video is shorter than about 11 minutes, reduce the segment duration to
1 second so the payload fits.

```powershell
Remove-Item -Recurse -Force segments_test, session_drm

# Prepare at 1-second segments
.\watermark.exe ab-prepare master.mp4 segments_test --profiles profiles_single.json `
    --hwaccel auto --segment-duration 1

# Assemble with FEC parity
.\watermark.exe ab-assemble --output session_drm --registry registry.json `
    --segment-duration 1 --fec-parity 16 `
    segments_test "user_abc_1735000000000_a1b2c3d4"

# Extract with matching segment duration and FEC setting
ffmpeg -y -i session_drm\720p.m3u8 -c copy session.mp4
.\watermark.exe ab-extract --registry registry.json `
    --segment-duration 1 --fec-parity 16 session.mp4
```

Expected output during assembly:

```
FEC parity: 16 bytes (corrects up to 8 byte errors)
```

And during extraction:

```
✅ Session hash: "zvVwtWvTDCx1_JXS"
✅ Original session ID: "user_abc_1735000000000_a1b2c3d4"
```

If the video is too short for the FEC payload even at 1-second segments,
`ab-assemble` will fail with a message stating the minimum required segment
count.

**3. Both enhancements together**

```powershell
Remove-Item -Recurse -Force segments_test, session_drm

.\watermark.exe ab-prepare master.mp4 segments_test --profiles profiles_single.json `
    --hwaccel auto --multi-coefficient --segment-duration 1

.\watermark.exe ab-assemble --output session_drm --registry registry.json `
    --segment-duration 1 --fec-parity 16 `
    segments_test "user_abc_1735000000000_a1b2c3d4"

ffmpeg -y -i session_drm\720p.m3u8 -c copy session.mp4

.\watermark.exe ab-extract --registry registry.json `
    --segment-duration 1 --multi-coefficient --fec-parity 16 session.mp4
```

Expected: three positions voting per block, plus Reed–Solomon correction on
top. This is the most robust configuration available, and also the one with
the longest minimum video duration.

### Minimum video length and segment duration

The A/B payload must fit entirely within the video's segment count. A single
payload cycle is 192 bits long (framing + 16-character hash), or 320 bits when
FEC parity 16 is enabled. The table below lists the minimum video duration for
each configuration, at different segment durations:

| Configuration | Segment duration | Minimum video length |
|---|---|---|
| No FEC | 2.0 s | 6.4 min |
| No FEC | 1.0 s | 3.2 min |
| FEC parity = 16 | 2.0 s | 10.7 min |
| FEC parity = 16 | 1.0 s | 5.3 min |

**Videos shorter than the minimum for their configuration cannot be
watermarked with A/B mode.** The payload will not fit, and the extractor will
not be able to reconstruct a valid CRC. For those cases, use the forensic
mode (`embed` / `extract`), which has no minimum duration and works down to a
single frame.

To process a shorter video with A/B mode, reduce `--segment-duration` to 1.0
or 0.5 seconds. This doubles (or quadruples) the number of segments for the
same content, allowing the payload to fit. The cost is a larger number of
smaller segment files, slightly worse compression due to more frequent
keyframes, and — most importantly — **incompatibility with a
production HLS packager configured for 2-second segments**. Use
`--segment-duration 1` only when testing the watermarking pipeline in
isolation; when integrating with a 2-second HLS backend, keep the default
and apply the duration limits above.

The exact minimum can be computed as `payload_bits × segment_duration`. For
the default configuration, `192 × 2.0 = 384 seconds`.

## Performance

All numbers measured on a machine with an 8-core CPU and an RTX-class GPU,
processing 7 minutes of 720p content.

| Operation | Before optimization | After optimization |
|---|---|---|
| `ab-prepare` (3 profiles) | ~15–25 min | **272 s** |
| `ab-extract` (720p, 210 segments) | 157 s | **44 s** |

Optimizations applied:

| Optimization | Speedup |
|---|---|
| Parallel DCT with goroutines | ~5× on 720p |
| Frame buffer reuse in encode loop | 30–40% wall-clock |
| NVENC hardware encoding | ~4–5× vs libx264 |

The parallel DCT splits the block grid into horizontal bands and processes
each band in a separate goroutine. Because blocks are independent, no
synchronization is required in the hot loop.


## Design decisions

### Why sign modulation instead of additive watermarking

Additive watermarking (`c' = c + α·w`) requires `α` to be large enough to
dominate the natural content. Sign modulation (`c' = ±(|c| + Δ)`) stores the
information in a property that quantization preserves: the direction of the
coefficient. It is dramatically more robust against recompression.

### Why CRC32 as an arbiter

The extractor does not know the ID length in advance, so it tries all plausible
cycle lengths. A verification step is needed to decide which candidate is
correct. CRC32 is fast, has no external dependency, and gives a false-positive
rate of ~2.3 × 10⁻¹⁰ — effectively zero for practical purposes. A cryptographic
hash would be overkill: the threat model is accidental corruption, not
adversarial forging.

### Why (4,4) as the coefficient position

- Lower frequencies are visible to the human eye.
- Higher frequencies are quantized into oblivion by H.264.
- (4,4) is the balance point.

### Why Δ = 25

The coefficient error introduced by CRF 32 recompression on realistic content
is ±15–20. Δ = 25 dominates that noise without being visible in flat areas
(sky, walls). Larger values improve robustness at the cost of visibility;
smaller values go the other way.

### Why hash the session ID

The raw ID can be arbitrarily long, which would force the required video
duration to grow with the ID. Hashing to a fixed 16-character string makes the
required video duration constant and independent of the ID, at the cost of
needing a registry to reverse the mapping.

### Why `pflag` instead of the standard `flag`

Go's standard `flag` package stops parsing at the first positional argument,
which means a flag placed after a positional argument is silently ignored.
This is a real source of subtle bugs. `pflag` supports flags in any position,
which matches what users expect.

### Why ffmpeg as a subprocess

Decoding and encoding video correctly is extremely complex. ffmpeg does it
better than any custom implementation could, and supports every relevant
codec. The tool's job is to process decoded frames, not to reimplement H.264.
The pipe-based architecture (`ffmpeg → raw YUV → Go → raw YUV → ffmpeg`) keeps
the service codec-agnostic.


## Testing

Unit tests:

```bash
go test ./...
```

End-to-end tests (require `ffmpeg` and `ffprobe`):

```bash
go test -tags=e2e -v -timeout 20m ./cmd/watermark/...
```

The E2E suite covers:

- Forensic round-trip at multiple resolutions and deltas.
- Survival of a second CRF 32 encode.
- Graceful failure on un-watermarked input.
- Full A/B pipeline: prepare → assemble → extract.
- A/B survival of a lossy re-encode.
- CLI behavior (help, unknown commands, missing files, invalid input).

Skip the slow tests:

```bash
go test -tags=e2e -short ./cmd/watermark/...
```


## Limitations

### DRM integration

The A/B pipeline is functional **without** DRM: segments are unencrypted and
the personalized playlist references them directly. An integration with Shaka
Packager + AES-128 was prototyped but is not included in this repository.
The blocker is that ffmpeg cannot decrypt the fMP4 segments that Shaka
Packager produces — production players (hls.js, Shaka Player) can, but the
inability to validate the pipeline end-to-end with ffmpeg made the integration
impractical to ship. This is a known ffmpeg limitation, not a limitation of the
watermarking scheme itself.

### Geometric attacks

The watermark does not survive cropping, rotation, or resolution changes that
break the alignment of the 8×8 block grid. Content-aware synchronization
patterns would be required to handle these.


## License

This project is licensed under the MIT License - see the [LICENSE](./LICENSE) file for details.