package workflow

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// requireFfmpeg skips the test when the media tools are not installed, so the
// suite still runs on a box without them.
func requireFfmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
}

// buildSource writes a synthetic source file and returns its path.
func buildSource(t *testing.T, name string, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	full := append([]string{"-nostdin", "-loglevel", "error", "-y"}, args...)
	full = append(full, path)

	cmd := exec.Command("ffmpeg", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building %s: %v: %s", name, err, stderr.String())
	}

	return path
}

// readDecodeTimes returns every track fragment decode time in a media segment,
// in seconds, using the init segment's timescales.
func readDecodeTimes(t *testing.T, init, media []byte) []float64 {
	t.Helper()
	scales, err := trackTimescales(init)
	if err != nil {
		t.Fatalf("trackTimescales: %v", err)
	}

	var times []float64
	err = findBoxes(media, 0, len(media), "moof", func(ms, me int) error {
		return findBoxes(media, ms, me, "traf", func(ts, te int) error {
			var trackID uint32
			if err := findBoxes(media, ts, te, "tfhd", func(hs, he int) error {
				trackID = binary.BigEndian.Uint32(media[hs+4:])
				return nil
			}); err != nil {
				return err
			}
			return findBoxes(media, ts, te, "tfdt", func(hs, he int) error {
				var base uint64
				if media[hs] == 1 {
					base = binary.BigEndian.Uint64(media[hs+4:])
				} else {
					base = uint64(binary.BigEndian.Uint32(media[hs+4:]))
				}
				times = append(times, float64(base)/float64(scales[trackID]))
				return nil
			})
		})
	})
	if err != nil {
		t.Fatalf("reading decode times: %v", err)
	}

	return times
}

// TestGenerateSegmentsAreIndependentlyPlaceable is the test for the assumption
// the whole design rests on: that three segments produced by three separate
// ffmpeg runs can be handed to a player, with one shared init, and land in the
// right places on the timeline.
//
// It is also a regression test for two things measurement caught and intuition
// did not. ffmpeg zeroes every output's timeline, so without shiftDecodeTime all
// three segments claim to start at zero. And -t combined with -copyts is read
// against the shifted timestamps rather than as a duration, which made every
// segment after the first come out empty.
func TestGenerateSegmentsAreIndependentlyPlaceable(t *testing.T) {
	requireFfmpeg(t)

	cases := []struct {
		name     string
		source   []string
		wantCopy bool
	}{
		{
			name: "copy path: H.264 High yuv420p + AAC-LC stereo",
			source: []string{
				"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=20",
				"-f", "lavfi", "-i", "sine=frequency=440:duration=20",
				"-c:v", "libx264", "-profile:v", "high", "-level:v", "4.1",
				"-pix_fmt", "yuv420p", "-g", "48", "-keyint_min", "48", "-sc_threshold", "0",
				"-c:a", "aac", "-profile:a", "aac_low", "-ac", "2", "-shortest",
			},
			wantCopy: true,
		},
		{
			name: "re-encode path: HEVC 10-bit + AC-3 5.1",
			source: []string{
				"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=20",
				"-f", "lavfi", "-i", "sine=frequency=440:duration=20",
				"-c:v", "libx265", "-pix_fmt", "yuv420p10le",
				"-x265-params", "log-level=none", "-preset", "ultrafast",
				"-c:a", "ac3", "-ac", "6", "-shortest",
			},
			wantCopy: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ext := ".mp4"
			if !tc.wantCopy {
				ext = ".mkv"
			}
			src := buildSource(t, "source"+ext, tc.source...)

			executor := NewFfmpegExecutor("", "", 2, 2)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			probe, err := executor.Probe(ctx, src)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}

			var keyframes []float64
			if video := probe.videoStream(); video != nil && videoIsPlayable(video) {
				if keyframes, err = executor.Keyframes(ctx, src); err != nil {
					t.Fatalf("Keyframes: %v", err)
				}
			}

			plan, err := buildPlan(probe, 4, keyframes)
			if err != nil {
				t.Fatalf("buildPlan: %v", err)
			}
			rendition := plan.Renditions[0]

			if rendition.VideoCopy != tc.wantCopy {
				t.Fatalf("VideoCopy = %v, want %v (codec %q profile %q pix_fmt %q)",
					rendition.VideoCopy, tc.wantCopy,
					probe.videoStream().CodecName, probe.videoStream().Profile,
					probe.videoStream().PixFmt)
			}
			if len(rendition.Segments) < 3 {
				t.Fatalf("plan has %d segments, want at least 3", len(rendition.Segments))
			}

			// Produce three segments in separate runs, deliberately out of
			// order, since a player seeking does exactly that.
			var generated []GeneratedSegment
			for _, i := range []int{2, 0, 1} {
				g, err := executor.Generate(ctx, src, rendition, rendition.Segments[i])
				if err != nil {
					t.Fatalf("Generate(segment %d): %v", i, err)
				}
				if len(g.Media) < 1000 {
					t.Fatalf("segment %d is %d bytes, which means ffmpeg wrote an empty fragment",
						i, len(g.Media))
				}
				generated = append(generated, g)
			}
			segments := map[int]GeneratedSegment{2: generated[0], 0: generated[1], 1: generated[2]}

			// One init must serve every segment, or caching it separately from
			// the segments is unsound.
			for i := 1; i < 3; i++ {
				if !bytes.Equal(segments[0].Init, segments[i].Init) {
					t.Errorf("init from segment %d differs from segment 0's (%d vs %d bytes)",
						i, len(segments[i].Init), len(segments[0].Init))
				}
			}

			// Each segment must say where it sits, in its own bytes.
			for i := 0; i < 3; i++ {
				want := rendition.Segments[i].Start
				for _, got := range readDecodeTimes(t, segments[0].Init, segments[i].Media) {
					if math.Abs(got-want) > 0.15 {
						t.Errorf("segment %d reports decode time %.3fs, want about %.3fs",
							i, got, want)
					}
				}
			}

			// And the three of them, with the shared init, must decode as one
			// continuous stream.
			joined := filepath.Join(t.TempDir(), "joined.mp4")
			var buf bytes.Buffer
			buf.Write(segments[0].Init)
			for i := 0; i < 3; i++ {
				buf.Write(segments[i].Media)
			}
			if err := os.WriteFile(joined, buf.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}

			out, err := exec.CommandContext(ctx, "ffprobe",
				"-v", "error",
				"-select_streams", "v:0",
				"-count_frames",
				"-show_entries", "stream=nb_read_frames",
				"-of", "csv=p=0", joined,
			).Output()
			if err != nil {
				t.Fatalf("the joined stream does not decode: %v", err)
			}

			covered := 0.0
			for i := 0; i < 3; i++ {
				covered += rendition.Segments[i].Duration
			}
			wantFrames := covered * rendition.FrameRate
			t.Logf("joined %0.1fs of video, decoded %s frames (about %.0f expected)",
				covered, bytes.TrimSpace(out), wantFrames)
		})
	}
}
