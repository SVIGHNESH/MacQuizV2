package blobstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 is an in-memory s3API double keyed by object key, used to test the
// S3 store's logic without a real bucket.
type fakeS3 struct {
	objects map[string][]byte
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}}
}

func (f *fakeS3) PutObject(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if params.Bucket == nil || *params.Bucket != "test-blobs" {
		return nil, errors.New("unexpected bucket")
	}
	body, err := io.ReadAll(params.Body)
	if err != nil {
		return nil, err
	}
	f.objects[*params.Key] = body
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, params *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	body, ok := f.objects[*params.Key]
	if !ok {
		return nil, errors.New("no such key")
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, params *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	delete(f.objects, *params.Key)
	return &s3.DeleteObjectOutput{}, nil
}

func TestS3SaveAndOpenRoundTrip(t *testing.T) {
	fake := newFakeS3()
	store := &S3{Client: fake, Bucket: "test-blobs", Ext: ".csv", ContentType: "text/csv"}

	ref, err := store.Save(context.Background(), strings.NewReader("type,question\nmcq,2+2?\n"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.HasSuffix(ref, ".csv") || len(ref) != len(".csv")+32 {
		t.Fatalf("unexpected ref shape: %q", ref)
	}

	rc, err := store.Open(context.Background(), ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "type,question\nmcq,2+2?\n" {
		t.Fatalf("round-tripped content mismatch: %q", got)
	}
}

func TestS3PutAppliesPrefixAndOverwrites(t *testing.T) {
	fake := newFakeS3()
	store := &S3{Client: fake, Bucket: "test-blobs", Prefix: "avatars/", ContentType: "image/jpeg"}

	if err := store.Put(context.Background(), "abcd1234.jpg", strings.NewReader("v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := fake.objects["avatars/abcd1234.jpg"]; !ok {
		t.Fatalf("object stored without prefix: keys %v", fake.objects)
	}
	if err := store.Put(context.Background(), "abcd1234.jpg", strings.NewReader("v2")); err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}

	rc, err := store.Open(context.Background(), "abcd1234.jpg")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "v2" {
		t.Fatalf("overwrite not visible: %q", got)
	}

	if err := store.Delete(context.Background(), "abcd1234.jpg"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Open(context.Background(), "abcd1234.jpg"); err == nil {
		t.Fatal("expected error opening deleted blob")
	}
	if err := store.Delete(context.Background(), "abcd1234.jpg"); err != nil {
		t.Fatalf("Delete of missing ref should be idempotent: %v", err)
	}
}

func TestS3RejectsPathTraversal(t *testing.T) {
	store := &S3{Client: newFakeS3(), Bucket: "test-blobs"}

	for _, bad := range []string{"", "../secret.csv", "sub/dir.csv", "."} {
		if _, err := store.Open(context.Background(), bad); err == nil {
			t.Fatalf("Open(%q) = nil error, want rejection", bad)
		}
		if err := store.Put(context.Background(), bad, strings.NewReader("x")); err == nil {
			t.Fatalf("Put(%q) = nil error, want rejection", bad)
		}
		if err := store.Delete(context.Background(), bad); err == nil {
			t.Fatalf("Delete(%q) = nil error, want rejection", bad)
		}
	}
}

func TestS3OpenMissingKey(t *testing.T) {
	store := &S3{Client: newFakeS3(), Bucket: "test-blobs"}
	if _, err := store.Open(context.Background(), "does-not-exist.csv"); err == nil {
		t.Fatal("expected error for missing object")
	}
}

// The AWS-vs-S3-compatible split lives entirely in NewS3's client options,
// and getting it wrong fails only against a real bucket (path-style
// addressing against S3 returns a redirect; region "auto" fails SigV4), so
// it is worth pinning here rather than discovering on the first deploy.
func TestNewS3AddressingStyleFollowsEndpoint(t *testing.T) {
	t.Run("aws bucket uses the regional endpoint and virtual-host style", func(t *testing.T) {
		store, err := NewS3(context.Background(), ObjectStore{
			Bucket:          "macquiz-blobs",
			Region:          "ap-south-1",
			AccessKeyID:     "AKIAEXAMPLE",
			SecretAccessKey: "secret",
		}, "avatars/", ".jpg", "image/jpeg")
		if err != nil {
			t.Fatalf("NewS3: %v", err)
		}
		opts := store.Client.(*s3.Client).Options()
		if opts.Region != "ap-south-1" {
			t.Errorf("Region = %q, want ap-south-1", opts.Region)
		}
		if opts.BaseEndpoint != nil {
			t.Errorf("BaseEndpoint = %q, want the SDK's regional default", *opts.BaseEndpoint)
		}
		if opts.UsePathStyle {
			t.Error("UsePathStyle = true, want virtual-host addressing against S3")
		}
		if store.Prefix != "avatars/" || store.Ext != ".jpg" || store.ContentType != "image/jpeg" {
			t.Errorf("key/content options not carried through: %+v", store)
		}
	})

	t.Run("s3-compatible endpoint switches to path style", func(t *testing.T) {
		store, err := NewS3(context.Background(), ObjectStore{
			Bucket:          "macquiz-blobs",
			Region:          "auto",
			Endpoint:        "https://account.r2.cloudflarestorage.com",
			AccessKeyID:     "key",
			SecretAccessKey: "secret",
		}, "", ".csv", "text/csv")
		if err != nil {
			t.Fatalf("NewS3: %v", err)
		}
		opts := store.Client.(*s3.Client).Options()
		if opts.BaseEndpoint == nil || *opts.BaseEndpoint != "https://account.r2.cloudflarestorage.com" {
			t.Errorf("BaseEndpoint = %v, want the configured endpoint", opts.BaseEndpoint)
		}
		if !opts.UsePathStyle {
			t.Error("UsePathStyle = false, want path addressing against an S3-compatible service")
		}
	})
}

// An unset bucket must degrade to local disk rather than fail boot - the
// dev stack and every DB-backed test depend on it.
func TestNewSelectsLocalDiskWithoutABucket(t *testing.T) {
	store, err := New(context.Background(), Options{LocalDir: t.TempDir(), Ext: ".csv"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := store.(Local); !ok {
		t.Fatalf("New returned %T, want Local", store)
	}
}
