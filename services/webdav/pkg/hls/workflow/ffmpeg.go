package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Executor runs the media tools. It exists so the workflow can be tested without
// ffmpeg on the box, and so a deployment could later put the work somewhere other
// than an exec.Command.
type Executor interface {
	// Probe describes the source's streams and duration.
	Probe(ctx context.Context, src string) (*probeResult, error)
	// Keyframes lists the video stream's keyframe timestamps, in seconds. It is
	// only called on the copy path.
	Keyframes(ctx context.Context, src string) ([]float64, error)
	// Generate produces one media segment and the initialization segment that
	// goes with it.
	Generate(ctx context.Context, src string, r Rendition, segment Segment) (GeneratedSegment, error)
}

// GeneratedSegment is one segment run's output. Init and Media come out of the
// same ffmpeg invocation on purpose: an initialization segment has to describe
// exactly the codec configuration the media segments carry, and producing them
// together means they cannot disagree — which matters most on the re-encode
// path, where the SPS/PPS come from x264 rather than from the source.
type GeneratedSegment struct {
	Init  []byte
	Media []byte
}

// FfmpegExecutor runs the ffmpeg and ffprobe command line tools.
type FfmpegExecutor struct {
	ffmpeg  string
	ffprobe string

	// sem bounds how many ffmpeg processes run at once. ffmpeg will use every
	// core it is given, and this process also has to answer PROPFIND, so the
	// limit is what keeps a handful of films from starving webdav. A request
	// that cannot get a slot is refused with GeneratorBusyError, which the HTTP
	// layer turns into 429 with Retry-After — the same contract the thumbnail
	// generator's own limit has.
	sem chan struct{}

	// threads caps each run's internal parallelism, so the semaphore's count is
	// a meaningful bound on total CPU rather than a bound on process count.
	threads int

	retryAfterSeconds int

	// queueWait is how long a request waits for a slot before it is refused.
	queueWait time.Duration
}

// NewFfmpegExecutor creates an executor backed by the ffmpeg CLI. concurrency
// and threads below 1 are treated as 1.
func NewFfmpegExecutor(ffmpegBin, ffprobeBin string, concurrency, threads int) *FfmpegExecutor {
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	if ffprobeBin == "" {
		ffprobeBin = "ffprobe"
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if threads < 1 {
		threads = 1
	}

	return &FfmpegExecutor{
		ffmpeg:            ffmpegBin,
		ffprobe:           ffprobeBin,
		sem:               make(chan struct{}, concurrency),
		threads:           threads,
		retryAfterSeconds: 5,
		queueWait:         30 * time.Second,
	}
}

// acquire takes a transcode slot, waiting a while for one before refusing.
//
// It waits because a player does not fetch artifacts one at a time: hls.js asks
// for the initialization segment and the first media segment together, every
// time playback starts. Refusing instantly turns the second of that pair into a
// 429 on every single play, which is not what the limit is for -- the limit
// exists to stop a crowd from burying the box, not to stop one viewer from
// starting. Past the wait it still refuses, so the queue stays bounded.
func (e *FfmpegExecutor) acquire(ctx context.Context) (func(), error) {
	select {
	case e.sem <- struct{}{}:
		return func() { <-e.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	timer := time.NewTimer(e.queueWait)
	defer timer.Stop()

	select {
	case e.sem <- struct{}{}:
		return func() { <-e.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, &GeneratorBusyError{RetryAfterSeconds: e.retryAfterSeconds}
	}
}

// Probe runs ffprobe for the container's streams and duration. It reads only
// the header, so on a seekable source this is a couple of range requests.
func (e *FfmpegExecutor) Probe(ctx context.Context, src string) (*probeResult, error) {
	out, err := e.run(ctx, e.ffprobe,
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		src,
	)
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}

	var result probeResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}

	return &result, nil
}

// Keyframes lists the video stream's keyframe timestamps.
//
// This is the expensive part of planning, and the honest cost of the copy path:
// finding every keyframe means demuxing the whole file, so the first
// master.m3u8 request for a source reads it end to end. Nothing is decoded, so
// it is I/O bound rather than CPU bound, and the result is cached in the plan —
// every later request for that source, including every segment, is served
// without it. A source that gets re-encoded never pays it at all, because there
// we place the keyframes ourselves.
func (e *FfmpegExecutor) Keyframes(ctx context.Context, src string) ([]float64, error) {
	out, err := e.run(ctx, e.ffprobe,
		"-v", "error",
		"-select_streams", "v:0",
		"-skip_frame", "nokey",
		"-show_entries", "frame=best_effort_timestamp_time",
		"-print_format", "csv=print_section=0:nokey=1",
		src,
	)
	if err != nil {
		return nil, fmt.Errorf("ffprobe keyframes: %w", err)
	}

	var times []float64
	for line := range strings.Lines(string(out)) {
		field := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if field == "" || field == "N/A" {
			continue
		}
		t, err := strconv.ParseFloat(field, 64)
		if err != nil {
			continue
		}
		times = append(times, t)
	}

	if len(times) == 0 {
		// A clean run that found nothing is an answer: the source cannot be cut
		// on keyframes, so it has to be re-encoded. That is different from the
		// run failing, which is handled above and must not be read as an answer.
		return nil, nil
	}

	return times, nil
}

// Generate produces one segment.
//
// Each segment is an independent ffmpeg run that seeks straight to its own
// start, so segments can be produced in any order, in parallel, by any replica,
// and a player seeking two hours in waits for one segment rather than for a
// transcode to catch up. The cost is that the re-encode path re-reads up to one
// GOP before the segment start to have something to decode from; the copy path
// pays almost nothing, because our boundaries are the source's own keyframes.
//
// The output is written with the fragmented MP4 muxer and split into the CMAF
// pair here, rather than handed to ffmpeg's HLS muxer. That is not a stylistic
// choice: the HLS muxer restarts its timeline on every invocation, so a segment
// it writes always claims to begin at t=0 and three such segments stack on top
// of one another. The MP4 muxer does the same, but its output is simple enough
// to correct — see shiftDecodeTime — and it writes a byte-identical init for
// every segment of a source, which is what makes one cached init usable with
// all of them.
func (e *FfmpegExecutor) Generate(ctx context.Context, src string, r Rendition, segment Segment) (GeneratedSegment, error) {
	release, err := e.acquire(ctx)
	if err != nil {
		return GeneratedSegment{}, err
	}
	defer release()

	args := []string{
		"-nostdin",
		"-hide_banner",
		"-loglevel", "error",
		"-threads", strconv.Itoa(e.threads),

		// Input seek, with accurate_seek so a re-encoded segment starts on the
		// frame we asked for rather than on the keyframe before it.
		"-accurate_seek",
		"-ss", formatSeconds(segment.Start),
		"-i", src,
		"-t", formatSeconds(e.segmentLength(r, segment)),

		"-map", "0:v:0",
	}
	if r.HasAudio {
		args = append(args, "-map", "0:a:0")
	}

	args = append(args, videoArgs(r)...)
	if r.HasAudio {
		args = append(args, audioArgs(r)...)
	}

	args = append(args,
		"-muxdelay", "0",
		"-muxpreload", "0",

		// empty_moov keeps per-segment state out of the moov, which is what
		// makes the init identical across runs. default_base_moof keeps the
		// data offsets moof-relative, so shifting a decode time afterwards
		// cannot invalidate them. frag_duration is set past any segment length
		// so one run yields exactly one fragment, as CMAF wants. Its maximum is
		// a little over 2147 seconds, so it is pinned just under that.
		"-movflags", "+empty_moov+default_base_moof",
		"-frag_duration", "2000000000",
		"-f", "mp4",
		"pipe:1",
	)

	out, err := e.run(ctx, e.ffmpeg, args...)
	if err != nil {
		return GeneratedSegment{}, fmt.Errorf("ffmpeg: %w", err)
	}

	init, media, err := splitInitAndMedia(out)
	if err != nil {
		return GeneratedSegment{}, err
	}

	scales, err := trackTimescales(init)
	if err != nil {
		return GeneratedSegment{}, err
	}

	// Put the segment where the playlist says it is. Without this the segment
	// is correct in every respect except the only one that lets a player place
	// it after its predecessor.
	if err := shiftDecodeTime(media, scales, segment.Start); err != nil {
		return GeneratedSegment{}, err
	}

	return GeneratedSegment{Init: init, Media: media}, nil
}

// segmentLength is how much of the source one run is asked for.
//
// On the re-encode path this is the segment duration exactly: we decide which
// frames to encode, and -accurate_seek plus -t lands on precisely the window the
// playlist advertises.
//
// On the copy path packets are passed through whole, and ffmpeg includes the one
// that straddles the end, which makes consecutive segments overlap by a frame or
// two. Asking for a frame less is what keeps a player from being handed the same
// presentation timestamp twice at a boundary.
func (e *FfmpegExecutor) segmentLength(r Rendition, segment Segment) float64 {
	if !r.VideoCopy || r.FrameRate <= 0 {
		return segment.Duration
	}

	trimmed := segment.Duration - 1/r.FrameRate
	if trimmed <= 0 {
		return segment.Duration
	}

	return trimmed
}

// videoArgs is the copy-or-encode branch for video. The encode side pins every
// parameter of the output contract, so two runs of two different segments of the
// same source produce streams a player can concatenate.
func videoArgs(r Rendition) []string {
	if r.VideoCopy {
		return []string{"-c:v", "copy"}
	}

	return []string{
		"-c:v", "libx264",
		"-profile:v", "high",
		"-level:v", "4.1",
		"-pix_fmt", "yuv420p",
		"-preset", "veryfast",
		"-crf", "20",
		// One IDR at the segment start and none after it: the segment is its own
		// GOP, which is what EXT-X-INDEPENDENT-SEGMENTS promises.
		"-g", "999999",
		"-keyint_min", "999999",
		"-sc_threshold", "0",
		"-force_key_frames", "expr:eq(n,0)",
	}
}

// audioArgs is the copy-or-encode branch for audio.
func audioArgs(r Rendition) []string {
	if r.AudioCopy {
		return []string{"-c:a", "copy"}
	}

	return []string{
		"-c:a", "aac",
		"-profile:a", "aac_low",
		"-ac", strconv.Itoa(outputMaxAudioChan),
		"-b:a", "128k",
	}
}

// run executes a tool and returns stdout, folding stderr into the error so a
// failure says what ffmpeg actually complained about.
func (e *FfmpegExecutor) run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = msg[:2000] + " ..."
		}
		if msg == "" {
			return nil, err
		}

		return nil, fmt.Errorf("%w: %s", err, msg)
	}

	return stdout.Bytes(), nil
}

// formatSeconds writes a timestamp ffmpeg reads back exactly, with no exponent
// and no locale surprises.
func formatSeconds(s float64) string {
	return strconv.FormatFloat(s, 'f', 6, 64)
}

// probeResult is the subset of ffprobe's JSON this package reads.
type probeResult struct {
	Streams []probeStream `json:"streams"`
	Format  probeFormat   `json:"format"`
}

type probeFormat struct {
	Duration string `json:"duration"`
	BitRate  string `json:"bit_rate"`
}

type probeStream struct {
	Index        int    `json:"index"`
	CodecName    string `json:"codec_name"`
	CodecType    string `json:"codec_type"`
	Profile      string `json:"profile"`
	Level        int    `json:"level"`
	PixFmt       string `json:"pix_fmt"`
	FieldOrder   string `json:"field_order"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Channels     int    `json:"channels"`
	AvgFrameRate string `json:"avg_frame_rate"`
	BitRate      string `json:"bit_rate"`
	Duration     string `json:"duration"`
	Disposition  struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
}

// videoStream is the first real video stream.
//
// Cover art counts as a video stream in ffprobe's eyes — an MKV or MP4 with
// embedded artwork reports a tiny mjpeg/png stream, and mapping it instead of
// the film produces a single-frame "video". The attached_pic disposition is what
// tells them apart.
func (p *probeResult) videoStream() *probeStream {
	for i := range p.Streams {
		s := &p.Streams[i]
		if s.CodecType == "video" && s.Disposition.AttachedPic == 0 {
			return s
		}
	}

	return nil
}

// audioStream is the first audio stream, or nil for a silent source.
func (p *probeResult) audioStream() *probeStream {
	for i := range p.Streams {
		if p.Streams[i].CodecType == "audio" {
			return &p.Streams[i]
		}
	}

	return nil
}

// duration prefers the container's duration and falls back to the video
// stream's, which is all some MPEG-TS and FLV sources report.
func (p *probeResult) duration() float64 {
	if d := parseFloatSafe(p.Format.Duration); d > 0 {
		return d
	}
	if v := p.videoStream(); v != nil {
		return parseFloatSafe(v.Duration)
	}

	return 0
}

func parseFloatSafe(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}

	return f
}
