package filesystem_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/adapter/filesystem"
	"github.com/thiagoribeiro/nimlet/internal/domain"
)

const mediaID = "3f2b8c1e-0000-4000-8000-000000000001"

func TestMediaStore_SaveAndGet(t *testing.T) {
	dir := t.TempDir()
	store := filesystem.NewMediaStore(dir)
	ctx := context.Background()

	require.NoError(t, store.Save(ctx, mediaID, "owner", "image/png", strings.NewReader("png-bytes")))
	assert.FileExists(t, filepath.Join(dir, mediaID[:2], mediaID))

	rc, contentType, size, err := store.Get(ctx, mediaID)
	require.NoError(t, err)
	defer rc.Close()
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "png-bytes", string(body))
	assert.Equal(t, "image/png", contentType)
	assert.Equal(t, int64(len("png-bytes")), size)
}

func TestMediaStore_GetMissing(t *testing.T) {
	store := filesystem.NewMediaStore(t.TempDir())
	for _, id := range []string{mediaID, "x", ""} {
		_, _, _, err := store.Get(context.Background(), id)
		assert.ErrorIs(t, err, domain.ErrNotFound, "id %q", id)
	}
}

func TestMediaStore_MissingMetaDefaultsContentType(t *testing.T) {
	dir := t.TempDir()
	store := filesystem.NewMediaStore(dir)
	ctx := context.Background()
	require.NoError(t, store.Save(ctx, mediaID, "owner", "text/plain", strings.NewReader("hi")))
	require.NoError(t, os.Remove(filepath.Join(dir, mediaID[:2], mediaID+".meta")))

	rc, contentType, _, err := store.Get(ctx, mediaID)
	require.NoError(t, err)
	rc.Close()
	assert.Equal(t, "application/octet-stream", contentType)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestMediaStore_SaveFailureLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	store := filesystem.NewMediaStore(dir)

	err := store.Save(context.Background(), mediaID, "owner", "image/png", failingReader{})
	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(dir, mediaID[:2], mediaID))
	assert.NoFileExists(t, filepath.Join(dir, mediaID[:2], mediaID+".meta"))
}
