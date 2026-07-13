package quiz

import (
	"context"
	"io"

	"macquiz/server/internal/blobstore"
)

// MaxImportFileBytes bounds a single bulk-upload request body (docs/07
// section 2: "Limits: 10 MB, 500 rows"); the row-count half of that limit is
// MaxImportRows, enforced later by ParseImportFile.
const MaxImportFileBytes = 10 << 20

// ImportStorage retrieves an uploaded bulk-import file by the opaque
// file_ref an imports row carries, so the validation worker (docs/07
// section 2 step 3) never depends on a specific object-storage backend.
type ImportStorage interface {
	Open(ctx context.Context, fileRef string) (io.ReadCloser, error)
}

// ImportUploadStore accepts a registered bulk-upload file and returns the
// opaque file_ref later stored on the imports row and passed to
// ImportStorage.Open. It stands in for the docs/07 pre-signed-upload flow
// (the teacher's browser would otherwise PUT straight to object storage) on
// the single-VM deployment, same as the blobstore's local-disk backend
// stands in for R2.
type ImportUploadStore interface {
	Save(ctx context.Context, r io.Reader) (fileRef string, err error)
}

// ImportFileStore is the union of ImportUploadStore and ImportStorage.
// RegisterImport writes a file through it and CommitImport later reads the
// same file back through the same handle to recover the parsed rows
// (docs/07 section 2 step 5); every implementation so far is one physical
// store, so the service depends on a single combined interface rather than
// two independently-configurable ones.
type ImportFileStore interface {
	ImportUploadStore
	ImportStorage
}

// LocalImportStorage is the blobstore's local-disk backend under its
// pre-extraction name, kept as an alias because the DB-backed flow tests
// across four packages construct it directly. New code should use
// blobstore.Local (or NewImportFileStore) instead.
type LocalImportStorage = blobstore.Local

// NewImportFileStore builds the bulk-import blob store: an R2 bucket when
// r2Bucket is set (docs/02 section 3.5, docs/09 section 4), otherwise local
// disk under dir (the dev/single-VM default). Backend selection and the
// degrade-not-fail contract live in blobstore.New.
func NewImportFileStore(dir, r2Bucket, r2Endpoint, r2AccessKeyID, r2SecretAccessKey string) ImportFileStore {
	return blobstore.New(blobstore.Options{
		LocalDir:          dir,
		Ext:               ".csv",
		ContentType:       "text/csv",
		R2Bucket:          r2Bucket,
		R2Endpoint:        r2Endpoint,
		R2AccessKeyID:     r2AccessKeyID,
		R2SecretAccessKey: r2SecretAccessKey,
	})
}
