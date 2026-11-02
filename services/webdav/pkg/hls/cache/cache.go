package cache

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/opencloud-eu/opencloud/pkg/config/defaults"
)

var ErrCacheMiss = errors.New("hls cache miss")

// DefaultFileCacheDir is the on-disk location of file-backend HLS artifacts:
// $OC_BASE_DATA_PATH/hls/cache. It is deliberately not the thumbnail cache
// directory: a two-hour film is some gigabytes of segments against a
// thumbnail's tens of kilobytes, so the two want separate eviction and separate
// volumes, and this one can be deleted as a whole without losing previews.
func DefaultFileCacheDir() string {
	return filepath.Join(defaults.BaseDataPath(), "hls", "cache")
}

// Cache provides cached storage for generated HLS artifacts: plans, playlists,
// initialization segments and media segments.
type Cache interface {
	Get(key string) ([]byte, error)
	Put(key string, data []byte) error
	// Purge removes everything the cache holds. What it drops is derived data:
	// the cost of purging is regenerating it, never losing it.
	Purge() error
}

// New creates a cache instance based on the configured backend. A backend
// nobody can serve is an error, not a silently disabled cache.
//
// The "memory" backend is accepted but a poor fit for HLS: segments are megabytes
// each and a single film fills it, so it is only useful for tests.
func New(backend string, cacheDir string, s3cfg *S3CacheConfig) (Cache, error) {
	switch backend {
	case "none":
		return NoopCache{}, nil
	case "memory":
		return NewInMemoryCache(), nil
	case "file":
		dir := cacheDir
		if dir == "" {
			dir = DefaultFileCacheDir()
		}
		return NewFileCache(dir), nil
	case "s3":
		if s3cfg == nil || !s3cfg.IsComplete() {
			return nil, errors.New("s3 hls cache needs a bucket, a region and credentials")
		}
		return NewS3Cache(*s3cfg), nil
	default:
		return nil, fmt.Errorf("unknown hls cache backend %q, expected none, memory, file or s3", backend)
	}
}

// BuildS3CacheConfig creates an S3CacheConfig from raw config fields.
func BuildS3CacheConfig(bucket, region, endpoint, accessKey, secretKey string) *S3CacheConfig {
	cfg := &S3CacheConfig{
		Bucket:   bucket,
		Region:   region,
		Endpoint: endpoint,
		Secure:   true,
	}
	if accessKey != "" && secretKey != "" {
		cfg.Credentials = credentials.NewStaticV4(accessKey, secretKey, "")
	}
	return cfg
}
