package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

type mediaService struct {
	store      out.MediaStore
	index      out.MediaIndex
	serverName string
}

// NewMediaService builds the media use case; serverName is the authority in returned
// content URIs (mxc://<serverName>/<mediaID>).
func NewMediaService(store out.MediaStore, index out.MediaIndex, serverName string) in.MediaService {
	return &mediaService{store: store, index: index, serverName: serverName}
}

// Upload streams r into the store without buffering, then records which namespace and
// user the file belongs to. If recording fails the bytes are orphaned but unreachable:
// Download only serves indexed media (or pre-namespace media to the default namespace).
func (s *mediaService) Upload(ctx context.Context, actor in.Actor, contentType string, r io.Reader) (*in.UploadResult, error) {
	mediaID := uuid.NewString()
	if err := s.store.Save(ctx, mediaID, actor.UserID, contentType, r); err != nil {
		return nil, fmt.Errorf("media: upload: %w", err)
	}
	if err := s.index.Create(ctx, &domain.Media{
		ID:          mediaID,
		NamespaceID: actor.NamespaceID,
		OwnerUserID: actor.UserID,
		ContentType: contentType,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		return nil, fmt.Errorf("media: upload: index: %w", err)
	}
	return &in.UploadResult{
		MediaID:    mediaID,
		ContentURI: "mxc://" + s.serverName + "/" + mediaID,
	}, nil
}

// Download serves a file only inside its own namespace. Media uploaded before namespaces
// existed has no index row; all of it was created in the default namespace, so only
// default-namespace actors may read it. A file from another namespace is reported as not
// found, exactly like a missing one.
func (s *mediaService) Download(ctx context.Context, actor in.Actor, mediaID string) (io.ReadCloser, string, int64, error) {
	// Every media ID is a UUID we generated. Rejecting anything else up front keeps
	// client-controlled strings out of storage paths/keys (e.g. "../" on the filesystem driver).
	if _, err := uuid.Parse(mediaID); err != nil {
		return nil, "", 0, domain.ErrNotFound
	}
	m, err := s.index.Get(ctx, mediaID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		if actor.NamespaceID != domain.DefaultNamespaceID {
			return nil, "", 0, domain.ErrNotFound
		}
	case err != nil:
		return nil, "", 0, fmt.Errorf("media: download: %w", err)
	case m.NamespaceID != actor.NamespaceID:
		return nil, "", 0, domain.ErrNotFound
	}
	return s.store.Get(ctx, mediaID)
}
