package workflow

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// PlanVersion is hashed into every cache key. Bump it whenever the segmenting
// rules, the output contract or anything else that makes a playlist and its
// segments agree with each other changes.
const PlanVersion = 1

// ErrUnknownArtifact is returned for an hls query value that is not one of the
// four known artifact shapes. Callers should surface it as HTTP 400.
var ErrUnknownArtifact = errors.New("hls: unknown artifact")

// ErrNoVideoStream is returned when ffprobe finds no usable video stream.
// Callers should surface it as HTTP 404, like any other unplayable source.
var ErrNoVideoStream = errors.New("hls: no video stream")

// Output contract. Every rendition we serve is exactly this, whether it got
// there by copying or by re-encoding, because it is the intersection of what
// current browsers and Safari play natively:
//
//	H.264 High, level 4.1, yuv420p, progressive
//	AAC-LC, stereo
//	fMP4/CMAF segments
//
// The copy path is allowed when the source already satisfies it; anything else
// is re-encoded to it. Same pipeline, one branch.
const (
	outputVideoCodec   = "avc1.640029" // H.264 High 4.1
	outputAudioCodec   = "mp4a.40.2"   // AAC-LC
	outputMaxAudioChan = 2
	outputPixFmt       = "yuv420p"
	outputH264Level    = 41
)

// Plan is the probed description of one source and the segmentation derived from
// it. It is cached per (checksum, type, PlanVersion) and is the single source of
// truth for the playlists and for every segment run: a playlist is rendered from
// it rather than from ffmpeg's own HLS muxer, so master.m3u8 is available as soon
// as the probe is done and the segment boundaries a player asks for are always
// the ones a segment run will produce.
type Plan struct {
	Version    int         `json:"version"`
	Duration   float64     `json:"duration"`
	Renditions []Rendition `json:"renditions"`
}

// Rendition is one entry of the multivariant playlist. Today there is exactly
// one, v0, at the source's own resolution — the URL scheme carries the rendition
// number so a ladder can be added later without moving any URLs.
type Rendition struct {
	Index     int  `json:"index"`
	VideoCopy bool `json:"video_copy"`
	AudioCopy bool `json:"audio_copy"`
	HasAudio  bool `json:"has_audio"`

	Width     int     `json:"width"`
	Height    int     `json:"height"`
	FrameRate float64 `json:"frame_rate"`
	Bandwidth int     `json:"bandwidth"`
	Codecs    string  `json:"codecs"`

	Segments []Segment `json:"segments"`
}

// Segment is one media segment's place on the timeline. Start and Duration are
// what the segment run is given and what EXTINF advertises, so the two cannot
// drift apart.
type Segment struct {
	Start    float64 `json:"start"`
	Duration float64 `json:"duration"`
}

// TargetDuration is the EXT-X-TARGETDURATION value: the ceiling of the longest
// segment, as RFC 8216 §4.3.3.1 requires.
func (r Rendition) TargetDuration() int {
	longest := 0.0
	for _, s := range r.Segments {
		longest = math.Max(longest, s.Duration)
	}

	return int(math.Ceil(longest))
}

// buildPlan turns a probe result into a Plan.
//
// keyframes is the video stream's keyframe timestamps and is only consulted on
// the copy path, where a segment must begin on a keyframe because the packets are
// passed through untouched. On the re-encode path we place the keyframes
// ourselves, so the grid is exactly targetDuration and no keyframe index is
// needed at all.
func buildPlan(probe *probeResult, targetDuration float64, keyframes []float64) (*Plan, error) {
	video := probe.videoStream()
	if video == nil {
		return nil, ErrNoVideoStream
	}

	duration := probe.duration()
	if duration <= 0 {
		return nil, fmt.Errorf("%w: source reports no duration", ErrNoVideoStream)
	}

	audio := probe.audioStream()
	rendition := Rendition{
		Index: 0,
		// Copying needs both a stream that already meets the output contract and
		// a keyframe index to cut it on. Without the index we cannot state honest
		// EXTINF values for passed-through packets, so the stream is re-encoded
		// and we place the boundaries ourselves.
		VideoCopy: videoIsPlayable(video) && len(keyframes) > 0,
		AudioCopy: audio != nil && audioIsPlayable(audio),
		HasAudio:  audio != nil,
		Width:     video.Width,
		Height:    video.Height,
		FrameRate: parseRational(video.AvgFrameRate),
	}

	rendition.Codecs = codecsAttribute(video, audio, rendition)
	rendition.Bandwidth = estimateBandwidth(probe, video, audio)

	if rendition.VideoCopy {
		rendition.Segments = segmentOnKeyframes(keyframes, duration, targetDuration)
	} else {
		rendition.Segments = segmentOnGrid(duration, targetDuration)
	}
	if len(rendition.Segments) == 0 {
		return nil, fmt.Errorf("%w: source yielded no segments", ErrNoVideoStream)
	}

	return &Plan{Version: PlanVersion, Duration: duration, Renditions: []Rendition{rendition}}, nil
}

// videoIsPlayable reports whether the video stream already satisfies the output
// contract and can be copied rather than re-encoded.
//
// It is deliberately conservative: an unknown profile, an unparsable level or an
// interlaced source answers false and gets re-encoded. Guessing wrong here does
// not produce a worse picture, it produces a stream the browser refuses, which
// looks like a broken file to the user.
func videoIsPlayable(s *probeStream) bool {
	if s.CodecName != "h264" {
		return false
	}
	switch s.PixFmt {
	case "yuv420p", "yuvj420p":
	default:
		// 10-bit, 4:2:2, 4:4:4 and the rest need re-encoding.
		return false
	}
	switch strings.ToLower(s.Profile) {
	case "constrained baseline", "baseline", "main", "high":
	default:
		// High 10, High 4:2:2, High 4:4:4 Predictive, ...
		return false
	}
	if s.Level <= 0 || s.Level > outputH264Level {
		return false
	}
	if s.FieldOrder != "" && s.FieldOrder != "progressive" {
		return false
	}

	return true
}

// audioIsPlayable reports whether the audio stream can be copied. AC-3, DTS,
// TrueHD, Vorbis, Opus-in-MP4 and anything above stereo are re-encoded to
// AAC-LC stereo.
func audioIsPlayable(s *probeStream) bool {
	if s.CodecName != "aac" {
		return false
	}
	if p := strings.ToLower(s.Profile); p != "lc" && p != "aac-lc" && p != "aac_low" {
		return false
	}

	return s.Channels >= 1 && s.Channels <= outputMaxAudioChan
}

// codecsAttribute is the RFC 6381 CODECS attribute for the master playlist.
//
// On the copy path it describes what the source actually carries, because
// over-stating the requirement can make a weaker device refuse a stream it could
// have played. When the profile or level cannot be mapped confidently the stream
// is not copied in the first place, so the output contract's own string is
// correct by construction.
func codecsAttribute(video, audio *probeStream, r Rendition) string {
	codecs := make([]string, 0, 2)

	if r.VideoCopy {
		if c := h264CodecString(video); c != "" {
			codecs = append(codecs, c)
		} else {
			codecs = append(codecs, outputVideoCodec)
		}
	} else {
		codecs = append(codecs, outputVideoCodec)
	}

	if r.HasAudio {
		// Both branches end at AAC-LC: copied AAC-LC and re-encoded AAC-LC
		// share the one string.
		_ = audio
		codecs = append(codecs, outputAudioCodec)
	}

	return strings.Join(codecs, ",")
}

// h264CodecString builds avc1.PPCCLL from the probed profile and level, e.g.
// High 4.1 -> avc1.640029. It returns "" when the profile is not one it maps.
func h264CodecString(s *probeStream) string {
	var profileIDC, constraints int
	switch strings.ToLower(s.Profile) {
	case "constrained baseline":
		profileIDC, constraints = 0x42, 0x40
	case "baseline":
		profileIDC, constraints = 0x42, 0x00
	case "main":
		profileIDC, constraints = 0x4D, 0x00
	case "high":
		profileIDC, constraints = 0x64, 0x00
	default:
		return ""
	}
	if s.Level <= 0 || s.Level > 0xFF {
		return ""
	}

	return fmt.Sprintf("avc1.%02X%02X%02X", profileIDC, constraints, s.Level)
}

// estimateBandwidth is the BANDWIDTH attribute: a peak-ish bitrate in bits per
// second. The attribute is required, and with a single rendition nothing selects
// on it, so an estimate from the source's own bitrate is enough.
func estimateBandwidth(probe *probeResult, video, audio *probeStream) int {
	if bps := atoiSafe(probe.Format.BitRate); bps > 0 {
		return bps
	}

	total := atoiSafe(video.BitRate)
	if audio != nil {
		total += atoiSafe(audio.BitRate)
	}
	if total > 0 {
		return total
	}

	// Nothing reported a bitrate. Derive something plausible from the frame size
	// rather than emitting a zero, which some players treat as unplayable.
	pixels := video.Width * video.Height
	if pixels <= 0 {
		return 2_000_000
	}

	return pixels / 2
}

// segmentOnGrid is the re-encode path's table: a fixed grid, because we force a
// keyframe at every boundary ourselves.
func segmentOnGrid(duration, target float64) []Segment {
	if target <= 0 {
		return nil
	}

	segments := make([]Segment, 0, int(duration/target)+1)
	for start := 0.0; start < duration; start += target {
		d := math.Min(target, duration-start)
		if d < minSegmentDuration && len(segments) > 0 {
			// Fold a sliver of a final segment into its predecessor rather than
			// advertising an EXTINF a player may balk at.
			segments[len(segments)-1].Duration += d
			break
		}
		segments = append(segments, Segment{Start: start, Duration: d})
	}

	return segments
}

// segmentOnKeyframes is the copy path's table. A copied segment must begin on a
// keyframe, so boundaries are taken from the source's own keyframes: walk them
// and cut at the first one at or past target seconds since the last cut. Segments
// are therefore at least target long and as much longer as the source's GOP
// structure forces.
func segmentOnKeyframes(keyframes []float64, duration, target float64) []Segment {
	if target <= 0 || len(keyframes) == 0 {
		return nil
	}

	// A source whose first keyframe is not at zero cannot be cut at zero.
	starts := []float64{keyframes[0]}
	for _, kf := range keyframes[1:] {
		if kf-starts[len(starts)-1] >= target && duration-kf >= minSegmentDuration {
			starts = append(starts, kf)
		}
	}

	segments := make([]Segment, 0, len(starts))
	for i, start := range starts {
		end := duration
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		if end-start <= 0 {
			continue
		}
		segments = append(segments, Segment{Start: start, Duration: end - start})
	}

	return segments
}

// minSegmentDuration is the shortest segment worth advertising on its own.
const minSegmentDuration = 0.5

// parseRational reads ffprobe's "30000/1001" style rate. It returns 0 when the
// rate is absent or degenerate, and callers omit FRAME-RATE in that case.
func parseRational(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		return 0
	}
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0
	}

	return n / d
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}

	return n
}

// rendition looks up one rendition of the plan by its index.
func (p *Plan) rendition(index int) (*Rendition, error) {
	for i := range p.Renditions {
		if p.Renditions[i].Index == index {
			return &p.Renditions[i], nil
		}
	}

	return nil, fmt.Errorf("%w: no rendition v%d", ErrUnknownArtifact, index)
}
