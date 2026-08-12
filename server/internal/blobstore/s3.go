package blobstore

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3API is the subset of *s3.Client S3 depends on, so tests can substitute
// a fake without a real bucket.
type s3API interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// S3 is the production Store (docs/02 section 3.5, docs/09 section 4):
// blobs live as objects in a bucket instead of on local disk, so serve and
// worker can run on separate hosts and containers without a shared volume.
type S3 struct {
	Client      s3API
	Bucket      string
	Prefix      string
	Ext         string
	ContentType string
}

// NewS3 builds an S3 store from an ObjectStore config.
//
// Two credential paths, chosen by whether AccessKeyID is set:
//
//   - empty (the AWS deployment's default): the SDK's standard credential
//     chain, which on EC2 resolves to the instance profile's role. No
//     long-lived key ever lands in .env.production or on the disk of a box
//     anyone can snapshot, and rotation is AWS's problem rather than ours.
//   - set: a static key pair, for an S3-compatible service that has no
//     instance-role equivalent (Cloudflare R2, MinIO) or for running the
//     stack outside AWS.
//
// Endpoint is likewise the non-AWS escape hatch: empty means the regional
// AWS endpoint with virtual-host-style addressing, and a non-empty value
// switches to that host with path-style addressing, which is what every
// S3-compatible implementation expects.
func NewS3(ctx context.Context, o ObjectStore, prefix, ext, contentType string) (*S3, error) {
	opts := s3.Options{Region: o.Region}
	if o.Endpoint != "" {
		opts.BaseEndpoint = aws.String(o.Endpoint)
		opts.UsePathStyle = true
	}
	if o.AccessKeyID != "" {
		opts.Credentials = credentials.NewStaticCredentialsProvider(o.AccessKeyID, o.SecretAccessKey, "")
	} else {
		// Resolving the chain can hit IMDS, so it needs the caller's ctx and
		// can fail; a bucket that was configured but cannot be reached is a
		// boot failure, not a silent fallback to local disk (that would put
		// serve and worker on two different stores).
		base, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(o.Region))
		if err != nil {
			return nil, fmt.Errorf("load aws credentials for bucket %q: %w", o.Bucket, err)
		}
		opts.Credentials = base.Credentials
	}
	return &S3{
		Client:      s3.New(opts),
		Bucket:      o.Bucket,
		Prefix:      prefix,
		Ext:         ext,
		ContentType: contentType,
	}, nil
}

// key maps a bare ref onto this store's namespaced object key.
func (s *S3) key(ref string) string {
	return s.Prefix + ref
}

// Open fetches ref as an object under Bucket.
func (s *S3) Open(ctx context.Context, ref string) (io.ReadCloser, error) {
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
func (s *S3) Save(ctx context.Context, r io.Reader) (string, error) {
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
func (s *S3) Put(ctx context.Context, ref string, r io.Reader) error {
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
func (s *S3) Delete(ctx context.Context, ref string) error {
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

var _ Store = (*S3)(nil)
