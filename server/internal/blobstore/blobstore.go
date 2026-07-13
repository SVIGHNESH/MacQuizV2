// Package blobstore persists opaque blobs behind one Store interface with
// two interchangeable backends: a Cloudflare R2 bucket (S3-compatible API,
// docs/02 section 3.5, docs/09 section 4) in production and a local-disk
// directory on the dev/single-VM stack. Which backend New returns is decided
// by whether the R2 bucket is configured; an unset bucket falling back to
// disk - rather than a boot failure - matches the "unconfigured optional
// backend degrades gracefully" contract used throughout this codebase
// (Redis publisher/cache, email sender).
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

// Options selects and parameterizes a Store backend.
type Options struct {
	// LocalDir is the disk fallback root, used when R2Bucket is unset.
	LocalDir string
	// Ext is appended to every Save-generated ref, e.g. ".csv".
	Ext string
	// ContentType is stored as the Content-Type of every R2 object.
	ContentType string
	// R2Prefix namespaces this store's object keys inside a shared bucket,
	// e.g. "avatars/". Refs stay bare; the prefix is applied internally.
	R2Prefix string

	R2Bucket          string
	R2Endpoint        string
	R2AccessKeyID     string
	R2SecretAccessKey string
}

// New selects the Store backend: R2 when Options.R2Bucket is set, otherwise
// local disk under Options.LocalDir (the dev/single-VM default).
func New(o Options) Store {
	if o.R2Bucket != "" {
		return NewR2(o)
	}
	return Local{Dir: o.LocalDir, Ext: o.Ext}
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
