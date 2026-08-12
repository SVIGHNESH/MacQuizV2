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
// R2 store's logic without a real R2/S3 endpoint.
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

func TestR2SaveAndOpenRoundTrip(t *testing.T) {
	fake := newFakeS3()
	store := &R2{Client: fake, Bucket: "test-blobs", Ext: ".csv", ContentType: "text/csv"}

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

func TestR2PutAppliesPrefixAndOverwrites(t *testing.T) {
	fake := newFakeS3()
	store := &R2{Client: fake, Bucket: "test-blobs", Prefix: "avatars/", ContentType: "image/jpeg"}

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

func TestR2RejectsPathTraversal(t *testing.T) {
	store := &R2{Client: newFakeS3(), Bucket: "test-blobs"}

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

func TestR2OpenMissingKey(t *testing.T) {
	store := &R2{Client: newFakeS3(), Bucket: "test-blobs"}
	if _, err := store.Open(context.Background(), "does-not-exist.csv"); err == nil {
		t.Fatal("expected error for missing object")
	}
}
