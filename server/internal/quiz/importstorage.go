package quiz

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ImportStorage retrieves an uploaded bulk-import file by the opaque
// file_ref an imports row carries, so the validation worker (docs/07
// section 2 step 3) never depends on a specific object-storage backend.
// LocalImportStorage is the only implementation today; a production R2
// client slots in behind the same interface without touching the worker.
type ImportStorage interface {
	Open(ctx context.Context, fileRef string) (io.ReadCloser, error)
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
