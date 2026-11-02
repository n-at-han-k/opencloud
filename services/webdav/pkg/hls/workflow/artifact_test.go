package workflow

import (
	"errors"
	"strings"
	"testing"
)

func TestParseArtifact(t *testing.T) {
	ok := []struct {
		in   string
		want Artifact
	}{
		{"master.m3u8", Artifact{Kind: KindMaster}},
		{"v0/index.m3u8", Artifact{Kind: KindIndex}},
		{"v0/init.mp4", Artifact{Kind: KindInit}},
		{"v0/seg00000.m4s", Artifact{Kind: KindSegment}},
		{"v0/seg00042.m4s", Artifact{Kind: KindSegment, Segment: 42}},
		{"v0/seg99999.m4s", Artifact{Kind: KindSegment, Segment: 99999}},
		{"v3/seg00007.m4s", Artifact{Kind: KindSegment, Rendition: 3, Segment: 7}},
		{"v12/index.m3u8", Artifact{Kind: KindIndex, Rendition: 12}},
	}
	for _, tc := range ok {
		got, err := ParseArtifact(tc.in)
		if err != nil {
			t.Errorf("ParseArtifact(%q) returned %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseArtifact(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		// the canonical spelling must round-trip, because it is what builds
		// cache keys and playlist lines
		if got.String() != tc.in {
			t.Errorf("ParseArtifact(%q).String() = %q", tc.in, got.String())
		}
	}

	bad := []string{
		"",
		"master.m3u",
		"index.m3u8",
		"init.mp4",
		"seg00000.m4s",
		"v0/master.m3u8",
		"v0/seg42.m4s",     // must be zero-padded to exactly five digits
		"v0/seg000042.m4s", // ... and not over-padded
		"v0/seg0004a.m4s",
		"v0/seg00042.ts",
		"v/index.m3u8",
		"v-1/index.m3u8",
		"v000/index.m3u8",
		"V0/index.m3u8",
		"v0/index.m3u8/",
		"v0//index.m3u8",

		// the ones that matter: the selector reaches a cache key, and the file
		// cache turns a key into a path
		"../../../etc/passwd",
		"v0/../../../etc/seg00001.m4s",
		"v0/x/../../../../seg00001.m4s",
		"/v0/seg00001.m4s",
		"v0/seg00001.m4s/../../x",
	}
	for _, in := range bad {
		if got, err := ParseArtifact(in); err == nil {
			t.Errorf("ParseArtifact(%q) accepted it as %+v, want an error", in, got)
		} else if !errors.Is(err, ErrUnknownArtifact) {
			t.Errorf("ParseArtifact(%q) returned %v, want ErrUnknownArtifact", in, err)
		}
	}
}

// A parsed artifact's cache key must never escape its prefix, whatever came in.
func TestCacheKeyStaysInsideItsPrefix(t *testing.T) {
	for _, in := range []string{"master.m3u8", "v0/index.m3u8", "v0/init.mp4", "v0/seg00042.m4s"} {
		artifact, err := ParseArtifact(in)
		if err != nil {
			t.Fatalf("ParseArtifact(%q): %v", in, err)
		}
		key := cacheKey("sum", "video/mp4", artifact)
		if strings.Contains(key, "..") {
			t.Errorf("key %q contains ..", key)
		}
		if strings.HasPrefix(key, "/") {
			t.Errorf("key %q is absolute", key)
		}
		if !strings.HasSuffix(key, in) {
			t.Errorf("key %q does not end in the canonical artifact %q", key, in)
		}
	}
}

func TestContentTypes(t *testing.T) {
	want := map[string]string{
		"master.m3u8":     "application/vnd.apple.mpegurl",
		"v0/index.m3u8":   "application/vnd.apple.mpegurl",
		"v0/init.mp4":     "video/mp4",
		"v0/seg00001.m4s": "video/iso.segment",
	}
	for in, ct := range want {
		artifact, err := ParseArtifact(in)
		if err != nil {
			t.Fatalf("ParseArtifact(%q): %v", in, err)
		}
		if got := artifact.ContentType(); got != ct {
			t.Errorf("%q content type = %q, want %q", in, got, ct)
		}
	}
}
