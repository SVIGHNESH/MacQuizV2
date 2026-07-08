package quiz

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MaxImportFileBytes bounds a single bulk-upload request body (docs/07
// section 2: "Limits: 10 MB, 500 rows"); the row-count half of that limit is
// MaxImportRows, enforced later by ParseImportCSV.
const MaxImportFileBytes = 10 << 20

// ImportStorage retrieves an uploaded bulk-import file by the opaque
// file_ref an imports row carries, so the validation worker (docs/07
// section 2 step 3) never depends on a specific object-storage backend.
// LocalImportStorage is the only implementation today; a production R2
// client slots in behind the same interface without touching the worker.
type ImportStorage interface {
	Open(ctx context.Context, fileRef string) (io.ReadCloser, error)
}

// ImportUploadStore accepts a registered bulk-upload file and returns the
// opaque file_ref later stored on the imports row and passed to
// ImportStorage.Open. It stands in for the docs/07 pre-signed-upload flow
// (the teacher's browser would otherwise PUT straight to object storage) on
// the single-VM deployment, same as LocalImportStorage stands in for R2.
type ImportUploadStore interface {
	Save(ctx context.Context, r io.Reader) (fileRef string, err error)
}

// LocalImportStorage is the dev/single-VM ImportStorage: files live as
// plain files under Dir, named by their file_ref. It stands in for the R2
// pre-signed-upload flow (docs/02 section 3.5, docs/09 section 4) until
// that brick is built.
type LocalImportStorage struct {
	Dir string
}

// Open reads fileRef from Dir. fileRef must be a bare filename - it is
// never allowed to escape Dir via a path separator or "..", since it
// ultimately comes from a client-controlled upload registration.
func (s LocalImportStorage) Open(_ context.Context, fileRef string) (io.ReadCloser, error) {
	if fileRef == "" || filepath.Base(fileRef) != fileRef {
		return nil, fmt.Errorf("invalid import file ref %q", fileRef)
	}
	f, err := os.Open(filepath.Join(s.Dir, fileRef))
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Save writes r to a freshly generated, random file_ref under Dir. The
// caller (the handler) is expected to have already capped r at
// MaxImportFileBytes via http.MaxBytesReader; Save does not re-check the
// size, so its own io.Copy error surfaces whatever *http.MaxBytesError that
// reader produces.
func (s LocalImportStorage) Save(_ context.Context, r io.Reader) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate import file ref: %w", err)
	}
	fileRef := hex.EncodeToString(raw) + ".csv"

	path := filepath.Join(s.Dir, fileRef)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("create import file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("write import file: %w", err)
	}
	return fileRef, nil
}
