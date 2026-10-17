package s3

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

type mediaStore struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
}

func NewMediaStore(client *s3.Client, bucket string) out.MediaStore {
	return &mediaStore{
		client: client,
		// Uploads arrive as a stream of unknown length (a multipart part body). PutObject
		// cannot take that — it must hash or size a seekable body — so the upload manager
		// sends it as a multipart upload, buffering one part at a time: memory per upload is
		// PartSize × Concurrency (5 MiB × 2), whatever the file size.
		uploader: manager.NewUploader(client, func(u *manager.Uploader) {
			u.PartSize = manager.MinUploadPartSize
			u.Concurrency = 2
		}),
		bucket: bucket,
	}
}

// Save streams r to S3 without buffering the whole body (see NewMediaStore).
func (s *mediaStore) Save(ctx context.Context, mediaID, _, contentType string, r io.Reader) error {
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         aws.String(mediaID),
		Body:        r,
		ContentType: aws.String(contentType),
		ACL:         types.ObjectCannedACLPrivate,
	})
	if err != nil {
		return fmt.Errorf("s3: put object %q: %w", mediaID, err)
	}
	return nil
}

// Get returns the S3 object body as io.ReadCloser.
// The HTTP handler's io.Copy streams this directly to the response writer —
// the full object never lands in application memory.
func (s *mediaStore) Get(ctx context.Context, mediaID string) (io.ReadCloser, string, int64, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    aws.String(mediaID),
	})
	if err != nil {
		// A missing key is a not-found, not an internal error.
		return nil, "", 0, fmt.Errorf("s3: get object %q: %w", mediaID, checkNotFound(err))
	}
	contentType := aws.ToString(out.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	size := aws.ToInt64(out.ContentLength)
	return out.Body, contentType, size, nil
}

func checkNotFound(err error) error {
	if err == nil {
		return nil
	}
	if isNoSuchKey(err) {
		return domain.ErrNotFound
	}
	return err
}

// isNoSuchKey looks through the SDK's error wrapping (operation → response → API error):
// a plain type assertion never matched, so a missing object surfaced as a 500.
func isNoSuchKey(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var api smithy.APIError
	return errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound")
}
