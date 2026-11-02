package hls

import (
	"mime"
	"slices"
	"strings"
)

// DefaultSourceTypes are the container types HLS playback is announced and
// attempted for. The list is about what ffmpeg can *demux*, which is almost
// everything: the codecs inside decide whether a stream is copied or re-encoded,
// not whether it is accepted at all.
//
// It is deliberately separate from thumbnail.DefaultSourceTypes. That list
// answers "can the image generator draw a preview of this", drives
// oc:has-preview, and must not grow video types unless something can actually
// extract a frame. This one answers "can we serve this as HLS", and nothing
// else reads it.
var DefaultSourceTypes = []string{
	"video/mp4",
	"video/x-m4v",
	"video/quicktime",
	"video/x-matroska",
	"video/webm",
	"video/x-msvideo",
	"video/avi",
	"video/x-ms-wmv",
	"video/x-ms-asf",
	"video/x-flv",
	"video/mpeg",
	"video/mp2t",
	"video/3gpp",
	"video/3gpp2",
	"video/ogg",
	"video/x-prores",
	"video/hevc",
	"video/x-theora",
	"application/vnd.rn-realmedia",
}

// SourceTypes are the types HLS playback is announced and attempted for: the
// defaults plus what a deployment configured.
//
// The list is a hint, not a verdict. ffprobe decides what it can really read,
// and a source it refuses answers 404.
type SourceTypes struct {
	byType map[string]struct{}
}

// NewSourceTypes merges the configured types onto the defaults.
func NewSourceTypes(entries []string) SourceTypes {
	t := SourceTypes{byType: make(map[string]struct{}, len(DefaultSourceTypes)+len(entries))}
	for _, entry := range slices.Concat(DefaultSourceTypes, entries) {
		if entry = strings.ToLower(strings.TrimSpace(entry)); entry != "" {
			t.byType[entry] = struct{}{}
		}
	}

	return t
}

// Supports reports whether HLS playback is attempted for this type.
func (t SourceTypes) Supports(contentType string) bool {
	parsed, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	_, supported := t.byType[parsed]

	return supported
}

// List is the types as a slice, for a configuration that takes one.
func (t SourceTypes) List() []string {
	out := make([]string, 0, len(t.byType))
	for mimeType := range t.byType {
		out = append(out, mimeType)
	}

	return out
}
