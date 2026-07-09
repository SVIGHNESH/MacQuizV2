package quiz

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// ImportValidateArgs is the job enqueued once a bulk-upload file is
// registered (docs/07 section 2 step 2) to run the worker-side validation
// pass (step 3). It carries no state beyond the import id: the worker
// re-reads the row, so a job that fires twice or against an import already
// resolved by a prior run is a harmless no-op.
type ImportValidateArgs struct {
	ImportID string `json:"import_id"`
}

// Kind names the job type in the queue ("import_validate job" in docs/07).
func (ImportValidateArgs) Kind() string { return "import_validate" }

// ValidateImport parses one import's uploaded file and records the outcome
// on the imports row: row_count and, on a clean file, status 'ready'; on
// any row error or an unreadable file, status 'failed' with error_report
// populated (docs/07 section 2 steps 3-4). Nothing is written to
// questions - commit is a separate, later step.
//
// It only acts on an import still in 'validating': a row already resolved
// by a prior run of this same job is left untouched, so ValidateImport is
// safe to re-run from a retry, the worker's boot re-scan, or a duplicate
// enqueue.
func ValidateImport(ctx context.Context, db *sql.DB, storage ImportStorage, importID string) error {
	var fileRef, status string
	err := db.QueryRowContext(ctx,
		`SELECT file_ref, status FROM imports WHERE id = $1`, importID).Scan(&fileRef, &status)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load import: %w", err)
	}
	if status != "validating" {
		return nil
	}

	f, err := storage.Open(ctx, fileRef)
	if err != nil {
		return failImport(ctx, db, importID, fmt.Sprintf("could not open uploaded file: %v", err))
	}
	defer f.Close()

	rows, rowErrors, err := ParseImportFile(f)
	if err != nil {
		return failImport(ctx, db, importID, err.Error())
	}

	newStatus := "ready"
	var report json.RawMessage
	if len(rowErrors) > 0 {
		newStatus = "failed"
		report, err = json.Marshal(rowErrors)
		if err != nil {
			return fmt.Errorf("marshal error report: %w", err)
		}
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE imports SET status = $1::import_status, row_count = $2, error_report = $3::jsonb WHERE id = $4`,
		newStatus, len(rows), nullableJSON(report), importID); err != nil {
		return fmt.Errorf("update import: %w", err)
	}
	return nil
}

// failImport marks an import 'failed' when the file itself could not be
// parsed at all (unreadable upload, missing required column) rather than
// producing per-row errors - a single file-level ImportRowError still shows
// the teacher why in the same error_report shape the review step expects.
func failImport(ctx context.Context, db *sql.DB, importID, message string) error {
	report, err := json.Marshal([]ImportRowError{{Row: 0, Column: "file", Message: message}})
	if err != nil {
		return fmt.Errorf("marshal file-level error report: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE imports SET status = 'failed'::import_status, row_count = 0, error_report = $1::jsonb WHERE id = $2`,
		report, importID); err != nil {
		return fmt.Errorf("record import failure: %w", err)
	}
	return nil
}
