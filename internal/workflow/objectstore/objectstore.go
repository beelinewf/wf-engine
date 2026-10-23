// Package objectstore is the streaming port multipart input uploads write
// through: file parts flow part-by-part into S3-compatible storage while
// the workflow context receives only references. The MinIO implementation
// is built once at startup from global config; service tests substitute a
// fake Store.
package objectstore

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Store streams objects into S3-compatible storage. Implementations must
// stream Put's reader without buffering the whole object, and Delete must
// be safe to call for a key that was never committed (cleanup runs
// best-effort after failures).
type Store interface {
	// Put streams r into bucket/key with the given content type and user
	// metadata. A negative size streams an unknown length.
	Put(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string, userMetadata map[string]string) error
	// Delete removes bucket/key. Deleting a missing key succeeds.
	Delete(ctx context.Context, bucket, key string) error
}

// NewMinioStore builds the S3-compatible Store. The endpoint is a bare
// host:port; an http(s):// prefix is tolerated for operator convenience.
func NewMinioStore(endpoint, region, accessKey, secretKey string, useSSL bool) (Store, error) {
	endpoint = strings.TrimSpace(endpoint)
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	if endpoint == "" || accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("objectstore: endpoint, access key, and secret key are required")
	}
	if region == "" {
		region = "us-east-1"
	}
	c, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
		Region: region,
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: build client: %w", err)
	}
	return &minioStore{c: c, region: region}, nil
}

// minioAPI is the subset of *minio.Client the store needs. *minio.Client
// satisfies it; tests substitute a fake.
type minioAPI interface {
	PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	BucketExists(ctx context.Context, bucketName string) (bool, error)
	MakeBucket(ctx context.Context, bucketName string, opts minio.MakeBucketOptions) error
	RemoveObject(ctx context.Context, bucketName, objectName string, opts minio.RemoveObjectOptions) error
}

// minioStore adapts a minioAPI to Store.
type minioStore struct {
	c      minioAPI
	region string
}

func (m *minioStore) Put(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string, userMetadata map[string]string) error {
	if err := m.ensureBucket(ctx, bucket); err != nil {
		return err
	}
	_, err := m.c.PutObject(ctx, bucket, key, r, size, minio.PutObjectOptions{
		ContentType:  contentType,
		UserMetadata: userMetadata,
	})
	if err != nil {
		return fmt.Errorf("objectstore: put %s/%s: %w", bucket, key, err)
	}
	return nil
}

// ensureBucket creates the bucket when it does not exist, so input nodes
// can name a bucket without operator provisioning. A bucket created
// concurrently between the check and the create is success.
func (m *minioStore) ensureBucket(ctx context.Context, bucket string) error {
	exists, err := m.c.BucketExists(ctx, bucket)
	if err != nil {
		return fmt.Errorf("objectstore: stat bucket %s: %w", bucket, err)
	}
	if exists {
		return nil
	}
	if err := m.c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: m.region}); err != nil {
		if code := minio.ToErrorResponse(err).Code; code == minio.BucketAlreadyExists || code == minio.BucketAlreadyOwnedByYou {
			return nil
		}
		return fmt.Errorf("objectstore: create bucket %s: %w", bucket, err)
	}
	return nil
}

func (m *minioStore) Delete(ctx context.Context, bucket, key string) error {
	if err := m.c.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("objectstore: delete %s/%s: %w", bucket, key, err)
	}
	return nil
}
