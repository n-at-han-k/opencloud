package cache

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"sync"

	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3CacheConfig holds the configuration for an S3 cache backend.
type S3CacheConfig struct {
	Bucket      string
	Region      string
	Endpoint    string
	Secure      bool
	Credentials *credentials.Credentials
}

// IsComplete returns true if all required fields are set.
func (c S3CacheConfig) IsComplete() bool {
	return c.Bucket != "" && c.Region != "" && c.Credentials != nil
}

// s3Object is the subset of minio.Object used by S3Cache.
type s3Object interface {
	io.ReadCloser
}

// s3Client is the subset of minio.Client used by S3Cache.
type s3Client interface {
	GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (s3Object, error)
	PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64, opts minio.PutObjectOptions) (info minio.UploadInfo, err error)
	ListObjects(ctx context.Context, bucketName string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo
	RemoveObject(ctx context.Context, bucketName, objectName string, opts minio.RemoveObjectOptions) error
}

// realS3Client wraps *minio.Client to satisfy s3Client interface.
type realS3Client struct {
	*minio.Client
}

func (r *realS3Client) GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (s3Object, error) {
	return r.Client.GetObject(ctx, bucketName, objectName, opts)
}

// hlsContentTypes are the media types of the artifacts this cache holds.
//
// They are stated here rather than left to mime.TypeByExtension because that
// reads the host's mime.types: .m4s is in nobody's, and .m3u8 is in some and not
// others, so the same object would be stored under different types on different
// machines. The type served to a player comes from the workflow either way; this
// is so what lands in the bucket is labelled correctly for anything else looking
// at it.
var hlsContentTypes = map[string]string{
	".m3u8": "application/vnd.apple.mpegurl",
	".m4s":  "video/iso.segment",
	".mp4":  "video/mp4",
	".json": "application/json",
}

// S3Cache stores HLS artifacts in an S3-compatible bucket.
type S3Cache struct {
	config  S3CacheConfig
	client  s3Client
	once    sync.Once
	initErr error
	newFunc func(cfg S3CacheConfig) (s3Client, error)
}

func NewS3Cache(cfg S3CacheConfig) *S3Cache {
	c := &S3Cache{config: cfg}
	c.newFunc = func(cfg S3CacheConfig) (s3Client, error) {
		cl, err := minio.New(cfg.Endpoint, &minio.Options{
			Creds:  cfg.Credentials,
			Secure: cfg.Secure,
			Region: cfg.Region,
		})
		if err != nil {
			return nil, err
		}
		return &realS3Client{Client: cl}, nil
	}
	return c
}

// initClient lazily initializes the S3 client on first access.
func (c *S3Cache) initClient() error {
	c.once.Do(func() {
		cl, err := c.newFunc(c.config)
		if err != nil {
			c.initErr = err
			return
		}
		c.client = cl
	})
	return c.initErr
}

func (c *S3Cache) Get(key string) ([]byte, error) {
	if err := c.initClient(); err != nil {
		return nil, err
	}

	obj, err := c.client.GetObject(context.Background(), c.config.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, asCacheError(err)
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, asCacheError(err)
	}

	return data, nil
}

// asCacheError reports a missing object as a cache miss. minio's GetObject only
// validates its arguments and sends the request on the first read, so a key that
// is not there surfaces from the read, not from GetObject. A missing bucket
// answers 404 as well and must not read as a miss: it is a misconfiguration that
// would otherwise hide behind every lookup.
func asCacheError(err error) error {
	rsp := minio.ToErrorResponse(err)
	if rsp.Code == minio.NoSuchKey {
		return ErrCacheMiss
	}
	if rsp.Code == "" && rsp.StatusCode == http.StatusNotFound {
		return ErrCacheMiss
	}

	return err
}

func (c *S3Cache) Put(key string, data []byte) error {
	if err := c.initClient(); err != nil {
		return err
	}

	ext := filepath.Ext(key)
	contentType, known := hlsContentTypes[ext]
	if !known {
		contentType = mime.TypeByExtension(ext)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	_, err := c.client.PutObject(
		context.Background(),
		c.config.Bucket,
		key,
		bytes.NewReader(data),
		int64(len(data)),
		minio.PutObjectOptions{
			ContentType: contentType,
		},
	)
	return err
}

// Purge removes every object in the bucket.
//
// The bucket is the cache, so this is a full listing and delete rather than a
// prefix walk; a deployment that shares the bucket with anything else wants a
// bucket of its own instead.
func (c *S3Cache) Purge() error {
	if err := c.initClient(); err != nil {
		return err
	}

	ctx := context.Background()
	for object := range c.client.ListObjects(ctx, c.config.Bucket, minio.ListObjectsOptions{Recursive: true}) {
		if object.Err != nil {
			return object.Err
		}
		if err := c.client.RemoveObject(ctx, c.config.Bucket, object.Key, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}

	return nil
}
