// Package blobstore persists opaque blobs behind one Store interface with
// two interchangeable backends: an S3 bucket (docs/02 section 3.5, docs/09
// section 4) in production and a local-disk directory on the dev/single-VM
// stack. Which backend New returns is decided by whether the bucket is
// configured; an unset bucket falling back to disk - rather than a boot
// failure - matches the "unconfigured optional backend degrades gracefully"
// contract used throughout this codebase (Redis publisher/cache, email
// sender).
package blobstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
)

// Store is the union of every blob operation the callers need. Bulk imports
// use Save (store-generated random ref) plus Open; avatars use Put
// (caller-chosen, content-derived ref) plus Open and Delete.
type Store interface {
	// Save writes r under a freshly generated random ref and returns it.
	Save(ctx context.Context, r io.Reader) (ref string, err error)
	// Put writes r under the caller's ref, overwriting any previous blob.
	// Callers key Put by content-derived refs, so an overwrite is idempotent.
	Put(ctx context.Context, ref string, r io.Reader) error
	// Open reads the blob stored under ref.
	Open(ctx context.Context, ref string) (io.ReadCloser, error)
	// Delete removes the blob stored under ref; deleting a ref that does not
	// exist is not an error, so callers can fire it best-effort.
	Delete(ctx context.Context, ref string) error
}

// ObjectStore is the bucket half of a Store's configuration, shared by every
// blobstore backed by object storage. It is one value rather than four loose
// strings because every caller passes the same set straight through from
// config, and because Bucket == "" is the single flag that selects the
// local-disk backend instead.
type ObjectStore struct {
	// Bucket empty selects the local-disk backend; everything else here is
	// ignored in that case.
	Bucket string
	// Region is the bucket's AWS region, e.g. "ap-south-1". Required when
	// Bucket is set - the SDK has no usable default.
	Region string
	// Endpoint overrides the AWS regional endpoint, for an S3-compatible
	// service (Cloudflare R2, MinIO) rather than S3 itself. See NewS3.
	Endpoint string
	// AccessKeyID and SecretAccessKey are optional static credentials; empty
	// means the default AWS credential chain (on EC2, the instance role).
	AccessKeyID     string
	SecretAccessKey string
}

// Options selects and parameterizes a Store backend.
type Options struct {
	// LocalDir is the disk fallback root, used when Object.Bucket is unset.
	LocalDir string
	// Ext is appended to every Save-generated ref, e.g. ".csv".
	Ext string
	// ContentType is stored as the Content-Type of every uploaded object.
	ContentType string
	// Prefix namespaces this store's object keys inside a shared bucket,
	// e.g. "avatars/". Refs stay bare; the prefix is applied internally.
	Prefix string

	Object ObjectStore
}

// New selects the Store backend: the bucket when Options.Object.Bucket is
// set, otherwise local disk under Options.LocalDir (the dev/single-VM
// default). It only returns an error for a bucket that is configured but
// whose credentials cannot be resolved; the local-disk path cannot fail.
func New(ctx context.Context, o Options) (Store, error) {
	if o.Object.Bucket != "" {
		return NewS3(ctx, o.Object, o.Prefix, o.Ext, o.ContentType)
	}
	return Local{Dir: o.LocalDir, Ext: o.Ext}, nil
}

// randomRef generates the random hex ref Save keys a new blob by.
func randomRef(ext string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate blob ref: %w", err)
	}
	return hex.EncodeToString(raw) + ext, nil
}

// validRef enforces the bare-name discipline both backends share: a ref must
// never escape the store via a path separator or "..", since some refs
// ultimately come from client-controlled registrations and object keys have
// no filesystem sandboxing to fall back on.
func validRef(ref string) error {
	if ref == "" || ref == "." || ref == ".." || filepath.Base(ref) != ref {
		return fmt.Errorf("invalid blob ref %q", ref)
	}
	return nil
}
