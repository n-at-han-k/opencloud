package cache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// FileCache stores HLS artifacts on the local filesystem using hierarchical directories.
type FileCache struct {
	dir string
}

func NewFileCache(dir string) *FileCache {
	return &FileCache{dir: dir}
}

// The cache key is already a hierarchical relative path, so the on-disk
// location is the cache dir joined with it.
func (c *FileCache) Get(key string) ([]byte, error) {
	path := filepath.Join(c.dir, key)

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrCacheMiss
		}
		return nil, err
	}

	return data, nil
}

func (c *FileCache) Put(key string, data []byte) error {
	imgPath := filepath.Join(c.dir, key)
	dir := filepath.Dir(imgPath)

	if _, err := os.Stat(imgPath); err == nil {
		return nil
	}

	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	f, err := os.CreateTemp(dir, "tmpthumb")
	if err != nil {
		return err
	}
	tempPath := f.Name()

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tempPath)
		return err
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tempPath)
		return err
	}

	if err := f.Close(); err != nil {
		os.Remove(tempPath)
		return err
	}

	if err := os.Rename(tempPath, imgPath); err != nil {
		os.Remove(tempPath)
		return err
	}

	return nil
}

// Purge empties the cache directory, leaving the directory itself in place so
// the running service can keep writing to it.
func (c *FileCache) Purge() error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return err
	}

	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(c.dir, entry.Name())); err != nil {
			return err
		}
	}

	return nil
}
