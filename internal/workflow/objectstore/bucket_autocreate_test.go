package objectstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/minio/minio-go/v7"
)

// fakeMinio is an in-memory minioAPI: buckets must be created before Put,
// mirroring S3's NoSuchBucket behavior.
type fakeMinio struct {
	buckets    map[string]bool
	makeCalls  int
	putCalls   int
	existsErr  error
	makeErr    error
	putErr     error
	makeRegion string
}

func newFakeMinio() *fakeMinio {
	return &fakeMinio{buckets: map[string]bool{}}
}

func (f *fakeMinio) BucketExists(_ context.Context, bucket string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	return f.buckets[bucket], nil
}

func (f *fakeMinio) MakeBucket(_ context.Context, bucket string, opts minio.MakeBucketOptions) error {
	f.makeCalls++
	f.makeRegion = opts.Region
	if f.makeErr != nil {
		// A racing writer won: the bucket exists even though we lost.
		if resp := minio.ToErrorResponse(f.makeErr); resp.Code == minio.BucketAlreadyExists || resp.Code == minio.BucketAlreadyOwnedByYou {
			f.buckets[bucket] = true
		}
		return f.makeErr
	}
	f.buckets[bucket] = true
	return nil
}

func (f *fakeMinio) PutObject(_ context.Context, bucket, _ string, r io.Reader, _ int64, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
	f.putCalls++
	if f.putErr != nil {
		return minio.UploadInfo{}, f.putErr
	}
	if !f.buckets[bucket] {
		return minio.UploadInfo{}, minio.ErrorResponse{Code: minio.NoSuchBucket, Message: "The specified bucket does not exist"}
	}
	_, _ = io.Copy(io.Discard, r)
	return minio.UploadInfo{}, nil
}

func (f *fakeMinio) RemoveObject(_ context.Context, _, _ string, _ minio.RemoveObjectOptions) error {
	return nil
}

// A Put to a missing bucket creates the bucket first, then uploads.
func TestPutCreatesMissingBucket(t *testing.T) {
	f := newFakeMinio()
	st := &minioStore{c: f, region: "us-east-1"}

	err := st.Put(context.Background(), "uploads", "posts/a.pdf", bytes.NewReader([]byte("pdf")), 3, "application/pdf", nil)
	if err != nil {
		t.Fatalf("Put() error = %v, want nil (bucket should be auto-created)", err)
	}
	if f.makeCalls != 1 {
		t.Fatalf("MakeBucket calls = %d, want 1", f.makeCalls)
	}
	if f.makeRegion != "us-east-1" {
		t.Errorf("MakeBucket region = %q, want %q", f.makeRegion, "us-east-1")
	}
	if f.putCalls != 1 {
		t.Fatalf("PutObject calls = %d, want 1", f.putCalls)
	}
}

// A Put to an existing bucket uploads without recreating it.
func TestPutSkipsCreateWhenBucketExists(t *testing.T) {
	f := newFakeMinio()
	f.buckets["uploads"] = true
	st := &minioStore{c: f, region: "us-east-1"}

	if err := st.Put(context.Background(), "uploads", "posts/a.pdf", bytes.NewReader([]byte("pdf")), 3, "application/pdf", nil); err != nil {
		t.Fatalf("Put() error = %v, want nil", err)
	}
	if f.makeCalls != 0 {
		t.Errorf("MakeBucket calls = %d, want 0 (bucket already exists)", f.makeCalls)
	}
}

// A bucket created concurrently between the existence check and the create
// still uploads: BucketAlreadyExists/BucketAlreadyOwnedByYou are success.
func TestPutToleratesConcurrentBucketCreate(t *testing.T) {
	for _, code := range []string{"BucketAlreadyExists", "BucketAlreadyOwnedByYou"} {
		f := newFakeMinio()
		f.makeErr = minio.ErrorResponse{Code: code, Message: "already exists"}
		st := &minioStore{c: f, region: "us-east-1"}

		if err := st.Put(context.Background(), "uploads", "posts/a.pdf", bytes.NewReader([]byte("pdf")), 3, "application/pdf", nil); err != nil {
			t.Errorf("Put() with MakeBucket %s error = %v, want nil", code, err)
		}
	}
}

// Bucket lookup and creation failures fail the Put without uploading.
func TestPutFailsWhenBucketCannotBeEnsured(t *testing.T) {
	t.Run("exists check fails", func(t *testing.T) {
		f := newFakeMinio()
		f.existsErr = errors.New("boom")
		st := &minioStore{c: f, region: "us-east-1"}

		if err := st.Put(context.Background(), "uploads", "posts/a.pdf", bytes.NewReader([]byte("x")), 1, "application/pdf", nil); err == nil {
			t.Fatal("Put() error = nil, want the bucket check failure")
		}
		if f.putCalls != 0 {
			t.Errorf("PutObject calls = %d, want 0", f.putCalls)
		}
	})

	t.Run("create fails", func(t *testing.T) {
		f := newFakeMinio()
		f.makeErr = errors.New("access denied")
		st := &minioStore{c: f, region: "us-east-1"}

		if err := st.Put(context.Background(), "uploads", "posts/a.pdf", bytes.NewReader([]byte("x")), 1, "application/pdf", nil); err == nil {
			t.Fatal("Put() error = nil, want the bucket create failure")
		}
		if f.putCalls != 0 {
			t.Errorf("PutObject calls = %d, want 0", f.putCalls)
		}
	})
}
