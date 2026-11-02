package workflow

import (
	"fmt"
	"strings"
)

// renderMaster renders the multivariant playlist from the plan.
func renderMaster(plan *Plan, version string) []byte {
	var b strings.Builder

	b.WriteString("#EXTM3U\n")
	// Version 7 is the floor for fMP4 segments (EXT-X-MAP with a non-TS payload).
	b.WriteString("#EXT-X-VERSION:7\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	for _, r := range plan.Renditions {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d", r.Bandwidth)
		if r.Codecs != "" {
			fmt.Fprintf(&b, ",CODECS=%q", r.Codecs)
		}
		if r.Width > 0 && r.Height > 0 {
			fmt.Fprintf(&b, ",RESOLUTION=%dx%d", r.Width, r.Height)
		}
		if r.FrameRate > 0 {
			fmt.Fprintf(&b, ",FRAME-RATE=%.3f", r.FrameRate)
		}
		b.WriteString("\n")
		b.WriteString(artifactURI(Artifact{Kind: KindIndex, Rendition: r.Index}, version) + "\n")
	}

	return []byte(b.String())
}

// renderIndex renders one rendition's media playlist from the plan. It is a
// complete VOD playlist: every segment is listed, so a player can seek anywhere
// immediately, whether or not those segments have been generated yet.
func renderIndex(r Rendition, version string) []byte {
	var b strings.Builder

	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", r.TargetDuration())
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	fmt.Fprintf(&b, "#EXT-X-MAP:URI=%q\n",
		artifactURI(Artifact{Kind: KindInit, Rendition: r.Index}, version))

	for i, s := range r.Segments {
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n", s.Duration)
		b.WriteString(artifactURI(Artifact{
			Kind:      KindSegment,
			Rendition: r.Index,
			Segment:   i,
		}, version) + "\n")
	}

	b.WriteString("#EXT-X-ENDLIST\n")

	return []byte(b.String())
}

// artifactURI is how a playlist points at another artifact.
//
// The artifacts live on the source file's own DAV URL, told apart by a query
// parameter, so a playlist line cannot be a plain relative path: resolving
// "seg00000.m4s" against /dav/spaces/{id}/movies/a.mkv?hls=v0/index.m3u8 would
// drop the filename and ask for /dav/spaces/{id}/movies/seg00000.m4s.
//
// A query-only reference is the one relative form that works. RFC 3986 §5.3
// keeps the base path and replaces only the query, so "?hls=v0/seg00000.m4s"
// resolves to the same file with a different artifact — and it stays correct
// behind any proxy, rename or public-link prefix, with nothing to configure.
func artifactURI(a Artifact, version string) string {
	uri := "?hls=" + a.String()
	if version != "" {
		// Ties the URL to the file's content; see versionToken.
		uri += "&v=" + version
	}

	return uri
}
