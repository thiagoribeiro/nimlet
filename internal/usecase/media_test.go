package usecase_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/usecase"
	"github.com/thiagoribeiro/nimlet/testkit"
)

func newMediaFixture() (in.MediaService, *testkit.MockMediaIndex) {
	store := &testkit.MockMediaStore{
		SaveFn: func(_ context.Context, _, _, _ string, r io.Reader) error { _, err := io.ReadAll(r); return err },
		GetFn: func(_ context.Context, _ string) (io.ReadCloser, string, int64, error) {
			return io.NopCloser(strings.NewReader("bytes")), "text/plain", 5, nil
		},
	}
	index := testkit.NewMockMediaIndex()
	return usecase.NewMediaService(store, index, "nimlet"), index
}

func TestMedia_DownloadIsNamespaceScoped(t *testing.T) {
	svc, _ := newMediaFixture()
	alice := in.Actor{NamespaceID: "ns-a", UserID: "alice"}
	mallory := in.Actor{NamespaceID: "ns-b", UserID: "mallory"}
	colleague := in.Actor{NamespaceID: "ns-a", UserID: "bob"}

	up, err := svc.Upload(context.Background(), alice, "text/plain", strings.NewReader("bytes"))
	require.NoError(t, err)
	assert.Equal(t, "mxc://nimlet/"+up.MediaID, up.ContentURI)

	rc, _, _, err := svc.Download(context.Background(), colleague, up.MediaID)
	require.NoError(t, err)
	_ = rc.Close()

	_, _, _, err = svc.Download(context.Background(), mallory, up.MediaID)
	assert.ErrorIs(t, err, domain.ErrNotFound, "known media ID from another namespace must not be served")
}

// Media from before namespaces has no index row; it all belonged to "default".
func TestMedia_LegacyMediaOnlyForDefaultNamespace(t *testing.T) {
	svc, _ := newMediaFixture()
	legacyID := uuid.NewString()

	rc, _, _, err := svc.Download(context.Background(), in.Actor{NamespaceID: domain.DefaultNamespaceID}, legacyID)
	require.NoError(t, err)
	_ = rc.Close()

	_, _, _, err = svc.Download(context.Background(), in.Actor{NamespaceID: "ns-b"}, legacyID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMedia_RejectsNonUUIDIDs(t *testing.T) {
	svc, _ := newMediaFixture()
	for _, id := range []string{"../etc", "..", "abc", ""} {
		_, _, _, err := svc.Download(context.Background(), in.Actor{NamespaceID: domain.DefaultNamespaceID}, id)
		assert.ErrorIs(t, err, domain.ErrNotFound, id)
	}
}
