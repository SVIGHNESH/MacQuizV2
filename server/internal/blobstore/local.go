package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Local is the dev/single-VM Store: blobs live as plain files under Dir,
// named by their ref. It stands in for the R2 backend (docs/02 section 3.5,
// docs/09 section 4) whenever no bucket is configured.
type Local struct {
	Dir string
	Ext string
}

// Open reads ref from Dir.
func (s Local) Open(_ context.Context, ref string) (io.ReadCloser, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(s.Dir, ref))
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Save writes r to a freshly generated, random ref under Dir. The caller
// (the handler) is expected to have already size-capped r via
// http.MaxBytesReader; Save does not re-check, so its io.Copy error surfaces
// whatever *http.MaxBytesError that reader produces.
func (s Local) Save(_ context.Context, r io.Reader) (string, error) {
	ref, err := randomRef(s.Ext)
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.Dir, ref)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("create blob file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("write blob file: %w", err)
	}
	return ref, nil
}

// Put writes r under the caller's ref via a temp-file-plus-rename, so a
// concurrent Open never observes a torn half-written blob.
func (s Local) Put(_ context.Context, ref string, r io.Reader) error {
	if err := validRef(ref); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".put-*")
	if err != nil {
		return fmt.Errorf("create blob temp file: %w", err)
	}
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write blob file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("close blob temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(s.Dir, ref)); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("finalize blob file: %w", err)
	}
	return nil
}

// Delete removes ref from Dir; a ref that is already gone is not an error.
func (s Local) Delete(_ context.Context, ref string) error {
	if err := validRef(ref); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.Dir, ref)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete blob file: %w", err)
	}
	return nil
}

var _ Store = Local{}
