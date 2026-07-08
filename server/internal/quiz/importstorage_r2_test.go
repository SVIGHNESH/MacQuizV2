package quiz

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 is an in-memory s3API double keyed by object key, used to test
// R2ImportStorage's Save/Open logic without a real R2/S3 endpoint.
type fakeS3 struct {
	objects map[string][]byte
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}}
}

func (f *fakeS3) PutObject(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if params.Bucket == nil || *params.Bucket != "test-imports" {
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

func TestR2ImportStorageSaveAndOpenRoundTrip(t *testing.T) {
	fake := newFakeS3()
	store := &R2ImportStorage{Client: fake, Bucket: "test-imports"}

	fileRef, err := store.Save(context.Background(), strings.NewReader("type,question\nmcq,2+2?\n"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.HasSuffix(fileRef, ".csv") || len(fileRef) != len(".csv")+32 {
		t.Fatalf("unexpected fileRef shape: %q", fileRef)
	}

	rc, err := store.Open(context.Background(), fileRef)
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

func TestR2ImportStorageOpenRejectsPathTraversal(t *testing.T) {
	store := &R2ImportStorage{Client: newFakeS3(), Bucket: "test-imports"}

	for _, bad := range []string{"", "../secret.csv", "sub/dir.csv", "."} {
		if _, err := store.Open(context.Background(), bad); err == nil {
			t.Fatalf("Open(%q) = nil error, want rejection", bad)
		}
	}
}

func TestR2ImportStorageOpenMissingKey(t *testing.T) {
	store := &R2ImportStorage{Client: newFakeS3(), Bucket: "test-imports"}
	if _, err := store.Open(context.Background(), "does-not-exist.csv"); err == nil {
		t.Fatal("expected error for missing object")
	}
}
