package quiz

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3API is the subset of *s3.Client R2ImportStorage depends on, so tests can
// substitute a fake without a real R2/S3 endpoint.
type s3API interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// R2ImportStorage is the production ImportFileStore (docs/02 section 3.5,
// docs/09 section 4): bulk-upload files live as objects in a Cloudflare R2
// bucket (S3-compatible) instead of on local disk, so serve and worker can
// run on separate hosts/containers without a shared volume. It stands in
// for LocalImportStorage exactly where docs/07 section 2 step 2 calls for
// object storage.
type R2ImportStorage struct {
	Client s3API
	Bucket string
}

// NewR2ImportStorage builds an R2ImportStorage from an R2 account endpoint
// (e.g. https://<account-id>.r2.cloudflarestorage.com) and an R2 API
// token's access key pair. R2 requires path-style bucket addressing and
// accepts any non-empty region name, so "auto" is used per Cloudflare's own
// documented SDK setup.
func NewR2ImportStorage(endpoint, bucket, accessKeyID, secretAccessKey string) *R2ImportStorage {
	client := s3.New(s3.Options{
		Region:       "auto",
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, ""),
	})
	return &R2ImportStorage{Client: client, Bucket: bucket}
}

// Open fetches fileRef as an object under Bucket. fileRef must be a bare
// name (no path separator or "..") - the same discipline LocalImportStorage
// applies, since it ultimately comes from a client-controlled upload
// registration and object keys have no filesystem sandboxing to fall back on.
func (s *R2ImportStorage) Open(ctx context.Context, fileRef string) (io.ReadCloser, error) {
	if fileRef == "" || filepath.Base(fileRef) != fileRef {
		return nil, fmt.Errorf("invalid import file ref %q", fileRef)
	}
	out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(fileRef),
	})
	if err != nil {
		return nil, fmt.Errorf("get import object %q: %w", fileRef, err)
	}
	return out.Body, nil
}

// Save uploads r's full contents as a freshly generated, random object key.
// The caller (the handler) is expected to have already capped r at
// MaxImportFileBytes via http.MaxBytesReader, so buffering the whole body
// (needed for S3's SigV4 payload signing, which requires a seekable body) is
// bounded and safe.
func (s *R2ImportStorage) Save(ctx context.Context, r io.Reader) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate import file ref: %w", err)
	}
	fileRef := hex.EncodeToString(raw) + ".csv"

	body, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("read import file: %w", err)
	}

	_, err = s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.Bucket),
		Key:           aws.String(fileRef),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String("text/csv"),
	})
	if err != nil {
		return "", fmt.Errorf("put import object %q: %w", fileRef, err)
	}
	return fileRef, nil
}

var _ ImportFileStore = (*R2ImportStorage)(nil)
