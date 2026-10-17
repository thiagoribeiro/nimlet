//go:build integration

package s3_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	s3adapter "github.com/thiagoribeiro/nimlet/internal/adapter/s3"
	"github.com/thiagoribeiro/nimlet/internal/domain"
)

// Runs against any S3-compatible endpoint (MinIO in bench/infra.sh): TEST_S3_ENDPOINT,
// TEST_S3_ACCESS_KEY, TEST_S3_SECRET_KEY.
func testClient(t *testing.T) (*awss3.Client, string) {
	t.Helper()
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_S3_ENDPOINT not set")
	}
	client := awss3.New(awss3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(os.Getenv("TEST_S3_ACCESS_KEY"), os.Getenv("TEST_S3_SECRET_KEY"), ""),
	})
	bucket := "nimlet-test-" + uuid.NewString()[:8]
	_, err := client.CreateBucket(context.Background(), &awss3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	return client, bucket
}

func TestMediaStore_S3(t *testing.T) {
	client, bucket := testClient(t)
	store := s3adapter.NewMediaStore(client, bucket)
	ctx := context.Background()

	// An upload arrives as a stream of unknown length (multipart part body).
	body := strings.Repeat("x", 6<<20) // larger than one S3 multipart part
	id := uuid.NewString()
	require.NoError(t, store.Save(ctx, id, "owner", "application/octet-stream", io.MultiReader(strings.NewReader(body))))

	rc, ct, size, err := store.Get(ctx, id)
	require.NoError(t, err)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	assert.Equal(t, len(body), len(got))
	assert.Equal(t, int64(len(body)), size)
	assert.Equal(t, "application/octet-stream", ct)

	_, _, _, err = store.Get(ctx, uuid.NewString())
	assert.True(t, errors.Is(err, domain.ErrNotFound), "missing object must be ErrNotFound, got %v", err)
}
