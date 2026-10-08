package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	flag "github.com/spf13/pflag"

	"github.com/rtagliaviaz/watermark-service/internal/session"
)

func runABAssemble(args []string) {
	fs := flag.NewFlagSet("ab-assemble", flag.ExitOnError)
	outputDir := fs.String("output", "",
		"Output directory (default: session_<sanitized-id>/)")
	segmentDuration := fs.Float64("segment-duration", 2.0,
		"Segment duration in seconds (must match ab-prepare)")
	registryPath := fs.String("registry", "",
		"Optional registry file to store hash->id mapping")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr,
			"usage: watermark ab-assemble [flags] <segments-dir> <session-id>")
		os.Exit(1)
	}
	segmentsDir, sessionID := rest[0], rest[1]

	if info, err := os.Stat(segmentsDir); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "segments dir not found or not a directory: %s\n", segmentsDir)
		os.Exit(1)
	}

	profiles, err := discoverProfiles(segmentsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discovering profiles: %v\n", err)
		os.Exit(1)
	}
	if len(profiles) == 0 {
		fmt.Fprintf(os.Stderr, "no quality profiles found in %s\n", segmentsDir)
		fmt.Fprintln(os.Stderr, "expected <segments-dir>/<quality>/{A,B}/*.m4s")
		os.Exit(1)
	}

	refName := profiles[0].name
	refIndices := profiles[0].indices
	for _, p := range profiles[1:] {
		if !equalInts(p.indices, refIndices) {
			fmt.Fprintf(os.Stderr,
				"❌ profile %s has different segments than %s\n", p.name, refName)
			os.Exit(1)
		}
	}
	numSegments := len(refIndices)

	bits, err := session.BitsForSession(sessionID, numSegments)
	if err != nil {
		fmt.Fprintf(os.Stderr, "computing session bits: %v\n", err)
		os.Exit(1)
	}

	if *registryPath != "" {
		reg, err := session.OpenRegistry(*registryPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "opening registry: %v\n", err)
			os.Exit(1)
		}
		hash := session.HashID(sessionID)
		if err := reg.Store(hash, sessionID); err != nil {
			fmt.Fprintf(os.Stderr, "storing in registry: %v\n", err)
			os.Exit(1)
		}
	}

	if *outputDir == "" {
		safe := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(sessionID)
		*outputDir = "session_" + safe
	}
	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "creating output dir: %v\n", err)
		os.Exit(1)
	}

	absOut, err := filepath.Abs(*outputDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving output dir: %v\n", err)
		os.Exit(1)
	}
	absSeg, err := filepath.Abs(segmentsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving segments dir: %v\n", err)
		os.Exit(1)
	}
	relSeg, err := filepath.Rel(absOut, absSeg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "computing relative path: %v\n", err)
		os.Exit(1)
	}
	relSeg = filepath.ToSlash(relSeg)

	for _, p := range profiles {
		mediaPath := filepath.Join(*outputDir, p.name+".m3u8")
		var sb strings.Builder
		sb.WriteString("#EXTM3U\n")
		sb.WriteString("#EXT-X-VERSION:7\n")
		fmt.Fprintf(&sb, "#EXT-X-TARGETDURATION:%d\n", int(*segmentDuration+0.5))
		sb.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
		sb.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

		initURI := fmt.Sprintf("%s/%s/A/init.mp4", relSeg, p.name)
		fmt.Fprintf(&sb, "#EXT-X-MAP:URI=\"%s\"\n", initURI)

		for i, idx := range refIndices {
			variant := "A"
			if bits[i] == 0 {
				variant = "B"
			}
			fmt.Fprintf(&sb, "#EXTINF:%.6f,\n", *segmentDuration)
			fmt.Fprintf(&sb, "%s/%s/%s/%d.m4s\n", relSeg, p.name, variant, idx)
		}
		sb.WriteString("#EXT-X-ENDLIST\n")

		if err := os.WriteFile(mediaPath, []byte(sb.String()), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "writing %s: %v\n", mediaPath, err)
			os.Exit(1)
		}
	}

	masterPath := filepath.Join(*outputDir, "master.m3u8")
	var msb strings.Builder
	msb.WriteString("#EXTM3U\n")
	msb.WriteString("#EXT-X-VERSION:7\n")
	msb.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	msb.WriteString("\n")

	for _, p := range profiles {
		bw := estimateBandwidth(absSeg, p.name, numSegments, *segmentDuration)
		codec := avcCodecString(p.height)
		fmt.Fprintf(&msb,
			"#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=\"%s\"\n",
			bw, p.width, p.height, codec)
		fmt.Fprintf(&msb, "%s.m3u8\n\n", p.name)
	}

	if err := os.WriteFile(masterPath, []byte(msb.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", masterPath, err)
		os.Exit(1)
	}

	var ones, zeros int
	for _, b := range bits {
		if b == 1 {
			ones++
		} else {
			zeros++
		}
	}

	fmt.Printf("🔑 Session:    %s\n", sessionID)
	fmt.Printf("📂 Segments:   %d × %d profiles (from %s)\n",
		numSegments, len(profiles), segmentsDir)
	fmt.Printf("🔢 Bits:       %d ones, %d zeros\n", ones, zeros)
	fmt.Printf("📄 Output dir: %s\n", *outputDir)
	fmt.Printf("   master:     %s\n", masterPath)
	for _, p := range profiles {
		fmt.Printf("   %-10s %s.m3u8 (%dx%d)\n", p.name, p.name, p.width, p.height)
	}

	if *registryPath != "" {
		reg, _ := session.OpenRegistry(*registryPath)
		fmt.Printf("🗂  Registry:   %s (%d entries)\n", *registryPath, reg.Size())
	}
}

type profileInfo struct {
	name    string
	width   int
	height  int
	indices []int
}

func discoverProfiles(segmentsDir string) ([]profileInfo, error) {
	entries, err := os.ReadDir(segmentsDir)
	if err != nil {
		return nil, err
	}

	var profiles []profileInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		dirA := filepath.Join(segmentsDir, name, "A")
		dirB := filepath.Join(segmentsDir, name, "B")

		indicesA, err := scanM4S(dirA)
		if err != nil {
			continue
		}
		indicesB, err := scanM4S(dirB)
		if err != nil {
			continue
		}
		if len(indicesA) == 0 || len(indicesA) != len(indicesB) {
			return nil, fmt.Errorf("profile %s: A has %d segments, B has %d",
				name, len(indicesA), len(indicesB))
		}

		initA := filepath.Join(dirA, "init.mp4")
		if _, err := os.Stat(initA); err != nil {
			return nil, fmt.Errorf("profile %s: missing %s", name, initA)
		}

		w, h := parseResolutionFromName(name)
		profiles = append(profiles, profileInfo{
			name:    name,
			width:   w,
			height:  h,
			indices: indicesA,
		})
	}

	sort.Slice(profiles, func(i, j int) bool {
		return profiles[i].height > profiles[j].height
	})

	return profiles, nil
}

func parseResolutionFromName(name string) (int, int) {
	n := strings.TrimSuffix(name, "p")
	h, err := strconv.Atoi(n)
	if err != nil || h <= 0 {
		return 0, 0
	}
	w := h * 16 / 9
	if w%2 != 0 {
		w++
	}
	return w, h
}

func scanM4S(dir string) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var indices []int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".m4s" {
			continue
		}
		base := strings.TrimSuffix(name, ".m4s")
		n, err := strconv.Atoi(base)
		if err != nil {
			continue
		}
		indices = append(indices, n)
	}
	sort.Ints(indices)
	return indices, nil
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func estimateBandwidth(segmentsDir, profileName string, numSegments int, segDuration float64) int {
	dirA := filepath.Join(segmentsDir, profileName, "A")
	entries, err := os.ReadDir(dirA)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".m4s" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	totalDuration := float64(numSegments) * segDuration
	if totalDuration <= 0 {
		return 0
	}
	return int(float64(total) * 8 / totalDuration)
}

func avcCodecString(height int) string {
	switch {
	case height <= 480:
		return "avc1.64001e"
	case height <= 720:
		return "avc1.64001f"
	case height <= 1080:
		return "avc1.640028"
	default:
		return "avc1.640032"
	}
}
