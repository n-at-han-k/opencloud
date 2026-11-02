package workflow

import (
	"math"
	"strings"
	"testing"
)

func h264(profile string, level int, pixFmt string) *probeStream {
	return &probeStream{
		CodecName: "h264", CodecType: "video", Profile: profile, Level: level,
		PixFmt: pixFmt, Width: 1920, Height: 1080, AvgFrameRate: "24/1",
	}
}

func TestVideoIsPlayable(t *testing.T) {
	cases := []struct {
		name string
		in   *probeStream
		want bool
	}{
		{"H.264 High 4.1 yuv420p", h264("High", 41, "yuv420p"), true},
		{"H.264 Main 3.1", h264("Main", 31, "yuv420p"), true},
		{"H.264 Constrained Baseline", h264("Constrained Baseline", 30, "yuv420p"), true},
		{"H.264 yuvj420p", h264("High", 40, "yuvj420p"), true},

		{"above level 4.1", h264("High", 50, "yuv420p"), false},
		{"10-bit", h264("High 10", 41, "yuv420p10le"), false},
		{"4:2:2", h264("High 4:2:2", 41, "yuv422p"), false},
		{"unreported level", h264("High", 0, "yuv420p"), false},
	}
	for _, tc := range cases {
		if got := videoIsPlayable(tc.in); got != tc.want {
			t.Errorf("%s: videoIsPlayable = %v, want %v", tc.name, got, tc.want)
		}
	}

	interlaced := h264("High", 41, "yuv420p")
	interlaced.FieldOrder = "tt"
	if videoIsPlayable(interlaced) {
		t.Error("interlaced source should be re-encoded")
	}

	for _, codec := range []string{"hevc", "vp9", "mpeg2video", "prores", "vc1"} {
		s := h264("High", 41, "yuv420p")
		s.CodecName = codec
		if videoIsPlayable(s) {
			t.Errorf("%s should be re-encoded", codec)
		}
	}
}

func TestAudioIsPlayable(t *testing.T) {
	aac := func(profile string, channels int) *probeStream {
		return &probeStream{CodecName: "aac", CodecType: "audio", Profile: profile, Channels: channels}
	}
	if !audioIsPlayable(aac("LC", 2)) {
		t.Error("AAC-LC stereo should be copied")
	}
	if !audioIsPlayable(aac("LC", 1)) {
		t.Error("AAC-LC mono should be copied")
	}
	if audioIsPlayable(aac("LC", 6)) {
		t.Error("5.1 should be downmixed, not copied")
	}
	if audioIsPlayable(aac("HE-AAC", 2)) {
		t.Error("HE-AAC should be re-encoded")
	}
	for _, codec := range []string{"ac3", "dts", "truehd", "flac", "vorbis", "opus"} {
		s := aac("LC", 2)
		s.CodecName = codec
		if audioIsPlayable(s) {
			t.Errorf("%s should be re-encoded", codec)
		}
	}
}

func TestH264CodecString(t *testing.T) {
	// the one everybody recognises: High profile, level 4.1
	if got := h264CodecString(h264("High", 41, "yuv420p")); got != "avc1.640029" {
		t.Errorf("High 4.1 = %q, want avc1.640029", got)
	}
	if got := h264CodecString(h264("Main", 31, "yuv420p")); got != "avc1.4D001F" {
		t.Errorf("Main 3.1 = %q, want avc1.4D001F", got)
	}
	if got := h264CodecString(h264("Constrained Baseline", 30, "yuv420p")); got != "avc1.42401E" {
		t.Errorf("Constrained Baseline 3.0 = %q, want avc1.42401E", got)
	}
	// a profile we cannot map must say so, so the caller falls back rather than
	// advertising a codec string that is a guess
	if got := h264CodecString(h264("High 4:4:4 Predictive", 51, "yuv444p")); got != "" {
		t.Errorf("unmappable profile = %q, want \"\"", got)
	}
}

func TestSegmentOnGrid(t *testing.T) {
	segs := segmentOnGrid(10, 4)
	want := []Segment{{0, 4}, {4, 4}, {8, 2}}
	if len(segs) != len(want) {
		t.Fatalf("got %d segments, want %d: %+v", len(segs), len(want), segs)
	}
	for i := range want {
		if math.Abs(segs[i].Start-want[i].Start) > 1e-9 || math.Abs(segs[i].Duration-want[i].Duration) > 1e-9 {
			t.Errorf("segment %d = %+v, want %+v", i, segs[i], want[i])
		}
	}

	// a sliver at the end is folded into its predecessor rather than
	// advertised as its own segment
	segs = segmentOnGrid(8.1, 4)
	if len(segs) != 2 {
		t.Fatalf("got %d segments for 8.1s, want 2: %+v", len(segs), segs)
	}
	if math.Abs(segs[1].Duration-4.1) > 1e-9 {
		t.Errorf("final segment duration = %v, want 4.1", segs[1].Duration)
	}

	// the table must tile the source exactly, with no gap and no overlap
	for _, duration := range []float64{1, 3.7, 10, 60, 7200.5} {
		segs := segmentOnGrid(duration, 4)
		if len(segs) == 0 {
			t.Errorf("duration %v produced no segments", duration)
			continue
		}
		at := 0.0
		for i, s := range segs {
			if math.Abs(s.Start-at) > 1e-9 {
				t.Errorf("duration %v: segment %d starts at %v, want %v", duration, i, s.Start, at)
			}
			at += s.Duration
		}
		if math.Abs(at-duration) > 1e-6 {
			t.Errorf("duration %v: segments cover %v", duration, at)
		}
	}
}

func TestSegmentOnKeyframes(t *testing.T) {
	// keyframes every 2s, 4s target: cut every other keyframe
	kf := []float64{0, 2, 4, 6, 8, 10}
	segs := segmentOnKeyframes(kf, 12, 4)
	for i, s := range segs {
		// every boundary must be a keyframe, or a copied segment is not
		// independently decodable
		found := false
		for _, k := range kf {
			if math.Abs(s.Start-k) < 1e-9 {
				found = true
			}
		}
		if !found {
			t.Errorf("segment %d starts at %v, which is not a keyframe", i, s.Start)
		}
		if s.Duration < 4 && i < len(segs)-1 {
			t.Errorf("segment %d is %vs, shorter than the 4s target", i, s.Duration)
		}
	}
	// and the table must still tile the source
	at := segs[0].Start
	for i, s := range segs {
		if math.Abs(s.Start-at) > 1e-9 {
			t.Errorf("segment %d starts at %v, want %v", i, s.Start, at)
		}
		at += s.Duration
	}
	if math.Abs(at-12) > 1e-6 {
		t.Errorf("segments cover %v, want 12", at)
	}

	// a sparse source: keyframes further apart than the target give longer
	// segments rather than boundaries off a keyframe
	segs = segmentOnKeyframes([]float64{0, 10, 20}, 30, 4)
	if len(segs) != 3 {
		t.Fatalf("got %d segments, want 3: %+v", len(segs), segs)
	}
	for _, s := range segs {
		if s.Duration < 9.9 {
			t.Errorf("segment %+v is shorter than the keyframe spacing", s)
		}
	}
}

// Without a keyframe index a stream cannot be copied, whatever its codec.
func TestBuildPlanFallsBackToEncodingWithoutKeyframes(t *testing.T) {
	probe := &probeResult{
		Streams: []probeStream{*h264("High", 41, "yuv420p")},
		Format:  probeFormat{Duration: "10", BitRate: "4000000"},
	}

	plan, err := buildPlan(probe, 4, []float64{0, 4, 8})
	if err != nil {
		t.Fatalf("buildPlan with keyframes: %v", err)
	}
	if !plan.Renditions[0].VideoCopy {
		t.Error("a conforming stream with a keyframe index should be copied")
	}

	plan, err = buildPlan(probe, 4, nil)
	if err != nil {
		t.Fatalf("buildPlan without keyframes: %v", err)
	}
	if plan.Renditions[0].VideoCopy {
		t.Error("without a keyframe index the stream must be re-encoded")
	}
	if len(plan.Renditions[0].Segments) == 0 {
		t.Error("the fallback produced no segments")
	}
}

// Cover art reports as a video stream; mapping it instead of the film yields a
// one-frame "video".
func TestVideoStreamIgnoresCoverArt(t *testing.T) {
	art := probeStream{CodecName: "mjpeg", CodecType: "video", Width: 600, Height: 600}
	art.Disposition.AttachedPic = 1
	film := *h264("High", 41, "yuv420p")

	probe := &probeResult{Streams: []probeStream{art, film}}
	got := probe.videoStream()
	if got == nil || got.CodecName != "h264" {
		t.Fatalf("videoStream picked %+v, want the h264 stream", got)
	}
}

func TestRenderPlaylists(t *testing.T) {
	plan := &Plan{
		Version:  PlanVersion,
		Duration: 10,
		Renditions: []Rendition{{
			Index: 0, HasAudio: true, Width: 1920, Height: 1080,
			FrameRate: 24, Bandwidth: 4000000, Codecs: "avc1.640029,mp4a.40.2",
			Segments: []Segment{{0, 4}, {4, 4}, {8, 2}},
		}},
	}

	master := string(renderMaster(plan, "abc123"))
	for _, want := range []string{
		"#EXTM3U", "#EXT-X-VERSION:7", "BANDWIDTH=4000000",
		`CODECS="avc1.640029,mp4a.40.2"`, "RESOLUTION=1920x1080", "?hls=v0/index.m3u8&v=abc123",
	} {
		if !strings.Contains(master, want) {
			t.Errorf("master playlist missing %q:\n%s", want, master)
		}
	}

	index := string(renderIndex(plan.Renditions[0], "abc123"))
	for _, want := range []string{
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-TARGETDURATION:4",
		`#EXT-X-MAP:URI="?hls=v0/init.mp4&v=abc123"`,
		"#EXTINF:4.000000,", "?hls=v0/seg00000.m4s&v=abc123",
		"?hls=v0/seg00002.m4s&v=abc123", "#EXT-X-ENDLIST",
	} {
		if !strings.Contains(index, want) {
			t.Errorf("media playlist missing %q:\n%s", want, index)
		}
	}

	// Every URI must be query-only. A plain relative path would resolve against
	// the file's DAV path and drop the filename.
	for _, line := range strings.Split(index, "\n") {
		if strings.HasSuffix(line, ".m4s") || strings.HasSuffix(line, ".m3u8") {
			if !strings.HasPrefix(line, "?hls=") {
				t.Errorf("playlist URI %q is not a query-only reference", line)
			}
		}
	}

	// TARGETDURATION is the ceiling of the longest segment (RFC 8216 4.3.3.1)
	r := Rendition{Segments: []Segment{{0, 4}, {4, 6.2}}}
	if got := r.TargetDuration(); got != 7 {
		t.Errorf("TargetDuration = %d, want 7", got)
	}
}
