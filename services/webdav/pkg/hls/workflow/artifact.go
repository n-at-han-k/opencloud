package workflow

import (
	"fmt"
	"strconv"
	"strings"
)

// Kind is the sort of HLS artifact a request names.
type Kind int

const (
	// KindMaster is the multivariant playlist, master.m3u8.
	KindMaster Kind = iota
	// KindIndex is a rendition's media playlist, v{n}/index.m3u8.
	KindIndex
	// KindInit is a rendition's initialization segment, v{n}/init.mp4.
	KindInit
	// KindSegment is one media segment, v{n}/seg{nnnnn}.m4s.
	KindSegment
)

// SegmentDigits is the zero-padded width of a segment number. It is fixed so a
// segment has exactly one spelling, which keeps the cache key one-to-one with
// the artifact.
const SegmentDigits = 5

// Artifact is a parsed, validated HLS artifact selector.
//
// It exists so nothing downstream ever handles the raw query value. The value
// reaches a cache key, and the file cache turns a key into a path with
// filepath.Join, so a selector that could carry ".." would read and write
// outside the cache directory. Parsing to these three integers makes that
// unrepresentable: String rebuilds the path from the fields, never from input.
type Artifact struct {
	Kind      Kind
	Rendition int
	Segment   int
}

// ParseArtifact parses an hls query value. Anything that is not exactly one of
// the four known shapes is rejected; there is no prefix or suffix matching.
func ParseArtifact(s string) (Artifact, error) {
	if s == "master.m3u8" {
		return Artifact{Kind: KindMaster}, nil
	}

	rendition, rest, ok := strings.Cut(s, "/")
	if !ok {
		return Artifact{}, fmt.Errorf("%w: %q", ErrUnknownArtifact, s)
	}

	n, err := parseRendition(rendition)
	if err != nil {
		return Artifact{}, fmt.Errorf("%w: %q", ErrUnknownArtifact, s)
	}

	switch {
	case rest == "index.m3u8":
		return Artifact{Kind: KindIndex, Rendition: n}, nil
	case rest == "init.mp4":
		return Artifact{Kind: KindInit, Rendition: n}, nil
	}

	seg, err := parseSegment(rest)
	if err != nil {
		return Artifact{}, fmt.Errorf("%w: %q", ErrUnknownArtifact, s)
	}

	return Artifact{Kind: KindSegment, Rendition: n, Segment: seg}, nil
}

// parseRendition reads "v{n}" with n a plain decimal, no sign and no padding.
func parseRendition(s string) (int, error) {
	digits, ok := strings.CutPrefix(s, "v")
	if !ok || digits == "" || len(digits) > 2 || !isDigits(digits) {
		return 0, fmt.Errorf("bad rendition %q", s)
	}

	return strconv.Atoi(digits)
}

// parseSegment reads "seg{nnnnn}.m4s" with exactly SegmentDigits digits, so
// seg42 and seg000042 are not alternative spellings of seg00042.
func parseSegment(s string) (int, error) {
	digits, ok := strings.CutPrefix(s, "seg")
	if !ok {
		return 0, fmt.Errorf("bad segment %q", s)
	}
	digits, ok = strings.CutSuffix(digits, ".m4s")
	if !ok || len(digits) != SegmentDigits || !isDigits(digits) {
		return 0, fmt.Errorf("bad segment %q", s)
	}

	return strconv.Atoi(digits)
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return s != ""
}

// String is the canonical spelling of the artifact, and the only thing that is
// ever used to build a cache key or a playlist line.
func (a Artifact) String() string {
	switch a.Kind {
	case KindMaster:
		return "master.m3u8"
	case KindIndex:
		return fmt.Sprintf("v%d/index.m3u8", a.Rendition)
	case KindInit:
		return fmt.Sprintf("v%d/init.mp4", a.Rendition)
	case KindSegment:
		return fmt.Sprintf("v%d/seg%0*d.m4s", a.Rendition, SegmentDigits, a.Segment)
	default:
		return ""
	}
}

// ContentType is the media type the artifact is served as.
func (a Artifact) ContentType() string {
	switch a.Kind {
	case KindMaster, KindIndex:
		// RFC 8216 §4. Safari insists on this one; hls.js does not care.
		return "application/vnd.apple.mpegurl"
	case KindInit:
		return "video/mp4"
	case KindSegment:
		return "video/iso.segment"
	default:
		return "application/octet-stream"
	}
}
