package blobstore

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3API is the subset of *s3.Client R2 depends on, so tests can substitute
// a fake without a real R2/S3 endpoint.
type s3API interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// R2 is the production Store (docs/02 section 3.5, docs/09 section 4):
// blobs live as objects in a Cloudflare R2 bucket (S3-compatible) instead
// of on local disk, so serve and worker can run on separate hosts and
// containers without a shared volume.
type R2 struct {
	Client      s3API
	Bucket      string
	Prefix      string
	Ext         string
	ContentType string
}

// NewR2 builds an R2 store from an R2 account endpoint (e.g.
// https://<account-id>.r2.cloudflarestorage.com) and an R2 API token's
// access key pair. R2 requires path-style bucket addressing and accepts any
// non-empty region name, so "auto" is used per Cloudflare's own documented
// SDK setup.
func NewR2(o Options) *R2 {
	client := s3.New(s3.Options{
		Region:       "auto",
		BaseEndpoint: aws.String(o.R2Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(o.R2AccessKeyID, o.R2SecretAccessKey, ""),
	})
	return &R2{Client: client, Bucket: o.R2Bucket, Prefix: o.R2Prefix, Ext: o.Ext, ContentType: o.ContentType}
}

// key maps a bare ref onto this store's namespaced object key.
func (s *R2) key(ref string) string {
	return s.Prefix + ref
}

// Open fetches ref as an object under Bucket.
func (s *R2) Open(ctx context.Context, ref string) (io.ReadCloser, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.key(ref)),
	})
	if err != nil {
		return nil, fmt.Errorf("get blob object %q: %w", ref, err)
	}
	return out.Body, nil
}

// Save uploads r's full contents under a freshly generated, random object
// key. The caller (the handler) is expected to have already size-capped r
// via http.MaxBytesReader, so buffering the whole body (needed for S3's
// SigV4 payload signing, which requires a seekable body) is bounded and safe.
func (s *R2) Save(ctx context.Context, r io.Reader) (string, error) {
	ref, err := randomRef(s.Ext)
	if err != nil {
		return "", err
	}
	if err := s.Put(ctx, ref, r); err != nil {
		return "", err
	}
	return ref, nil
}

// Put uploads r's full contents under the caller's ref, overwriting any
// previous object. The same bounded-buffering caveat as Save applies.
func (s *R2) Put(ctx context.Context, ref string, r io.Reader) error {
	if err := validRef(ref); err != nil {
		return err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read blob: %w", err)
	}
	_, err = s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.Bucket),
		Key:           aws.String(s.key(ref)),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String(s.ContentType),
	})
	if err != nil {
		return fmt.Errorf("put blob object %q: %w", ref, err)
	}
	return nil
}

// Delete removes ref's object; S3-style deletes are idempotent, so a ref
// that is already gone is not an error.
func (s *R2) Delete(ctx context.Context, ref string) error {
	if err := validRef(ref); err != nil {
		return err
	}
	if _, err := s.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.key(ref)),
	}); err != nil {
		return fmt.Errorf("delete blob object %q: %w", ref, err)
	}
	return nil
}

var _ Store = (*R2)(nil)
