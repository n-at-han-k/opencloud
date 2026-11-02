package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// cacheKey builds the cache key for one artifact of one source:
// hash[:2]/hash[2:4]/hash[4:]/<canonical artifact>.
//
// The checksum is hashed rather than used as it comes, for the same reason the
// thumbnail cache hashes it: it is not this service's to trust, an OCM share
// takes it from a remote PROPFIND, and it ends up as a path on disk and as an
// object key. The source type is hashed with it because the same bytes under two
// types are not the same stream — the type picks the demuxer.
//
// PlanVersion is hashed in as well, so changing the segmenting rules, the output
// contract or the target duration retires every cached artifact instead of
// serving segments that disagree with a freshly rendered playlist. That last
// failure mode is silent and miserable to debug, which is why it is in the key.
func cacheKey(checksum, mimeType string, artifact Artifact) string {
	return keyPrefix(checksum, mimeType) + "/" + artifact.String()
}

// planCacheKey is where the probed plan for a source lives.
func planCacheKey(checksum, mimeType string) string {
	return keyPrefix(checksum, mimeType) + "/plan.json"
}

func keyPrefix(checksum, mimeType string) string {
	sum := sha256.Sum256([]byte(
		checksum + "\x00" + mimeType + "\x00" + strconv.Itoa(PlanVersion),
	))
	hash := hex.EncodeToString(sum[:])

	return hash[:2] + "/" + hash[2:4] + "/" + hash[4:]
}

// versionToken is a short stand-in for the source's identity, put on every URI a
// playlist generates.
//
// The artifacts are keyed on the file's checksum but the URLs do not carry it,
// so without this a client that cached v0/seg00001.m4s keeps serving it after
// the file is replaced -- and mixing segments of two versions of a film is the
// kind of failure nobody reports as a cache bug. Changing the file changes the
// token, which changes every segment URL.
func versionToken(checksum, mimeType string) string {
	sum := sha256.Sum256([]byte(
		checksum + "\x00" + mimeType + "\x00" + strconv.Itoa(PlanVersion),
	))

	return hex.EncodeToString(sum[:])[:8]
}
