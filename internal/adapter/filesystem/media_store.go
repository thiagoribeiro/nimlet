package filesystem

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

type mediaStore struct {
	basePath string
}

func NewMediaStore(basePath string) out.MediaStore {
	return &mediaStore{basePath: basePath}
}

// Save streams r directly to disk — no intermediate buffer.
// Files are sharded into 2-char prefix directories to avoid fs inode limits.
func (s *mediaStore) Save(_ context.Context, mediaID, _, contentType string, r io.Reader) error {
	dir := filepath.Join(s.basePath, mediaID[:2])
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("filesystem: mkdir: %w", err)
	}

	// Write content file.
	contentPath := filepath.Join(dir, mediaID)
	f, err := os.Create(contentPath)
	if err != nil {
		return fmt.Errorf("filesystem: create file: %w", err)
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(contentPath)
		return fmt.Errorf("filesystem: write file: %w", err)
	}

	// Write sidecar for content-type.
	metaPath := contentPath + ".meta"
	if err := os.WriteFile(metaPath, []byte(contentType), 0o640); err != nil {
		return fmt.Errorf("filesystem: write meta: %w", err)
	}
	return nil
}

// Get returns an *os.File as io.ReadCloser so io.Copy in the HTTP handler
// can use the sendfile(2) syscall on Linux — true zero-copy path.
func (s *mediaStore) Get(_ context.Context, mediaID string) (io.ReadCloser, string, int64, error) {
	if len(mediaID) < 2 {
		return nil, "", 0, domain.ErrNotFound
	}
	contentPath := filepath.Join(s.basePath, mediaID[:2], mediaID)

	f, err := os.Open(contentPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", 0, domain.ErrNotFound
		}
		return nil, "", 0, fmt.Errorf("filesystem: open file: %w", err)
	}

	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, "", 0, fmt.Errorf("filesystem: stat file: %w", err)
	}

	contentType := "application/octet-stream"
	metaBytes, err := os.ReadFile(contentPath + ".meta")
	if err == nil {
		contentType = string(metaBytes)
	}

	return f, contentType, stat.Size(), nil
}
