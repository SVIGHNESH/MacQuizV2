package quiz_test

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
)

// TestValidateImportFlow pins the docs/07 section 2 step 3 worker brick:
// quiz.ValidateImport reads an imports row's uploaded file, parses it, and
// records row_count/status/error_report - a clean file goes 'ready', a file
// with row errors goes 'failed' with the per-row report, an unreadable file
// (missing from storage) also fails with a file-level error, and an import
// no longer in 'validating' is left untouched (idempotent re-run).
//
// It runs in its own database (macquiz_importtest) - see itest.FreshDatabase.
func TestValidateImportFlow(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_importtest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	provision(t, ctx, sqlDB, "teacher", "owner@school.test")

	quizID := seedQuiz(t, ctx, sqlDB, "owner@school.test", "Import target")
	storage := quiz.LocalImportStorage{Dir: t.TempDir()}

	const header = "type,question,option_a,option_b,option_c,option_d,option_e,option_f,correct,points\n"

	t.Run("clean file goes ready", func(t *testing.T) {
		csv := header +
			"single,Pick red,Red,Blue,,,,,a,2\n" +
			"truefalse,Sky is blue,,,,,,,true,\n"
		writeImportFile(t, storage.Dir, "clean.csv", csv)
		importID := seedImport(t, ctx, sqlDB, quizID, "owner@school.test", "clean.csv")

		if err := quiz.ValidateImport(ctx, sqlDB, storage, importID); err != nil {
			t.Fatalf("ValidateImport: %v", err)
		}

		status, rowCount, report := loadImport(t, ctx, sqlDB, importID)
		if status != "ready" {
			t.Errorf("status = %q, want ready", status)
		}
		if rowCount != 2 {
			t.Errorf("row_count = %d, want 2", rowCount)
		}
		if report != nil {
			t.Errorf("error_report = %s, want nil", report)
		}
	})

	t.Run("clean xlsx file goes ready", func(t *testing.T) {
		writeImportFileBytes(t, storage.Dir, "clean.xlsx", buildXLSXFixture(t))
		importID := seedImport(t, ctx, sqlDB, quizID, "owner@school.test", "clean.xlsx")

		if err := quiz.ValidateImport(ctx, sqlDB, storage, importID); err != nil {
			t.Fatalf("ValidateImport: %v", err)
		}

		status, rowCount, report := loadImport(t, ctx, sqlDB, importID)
		if status != "ready" {
			t.Errorf("status = %q, want ready", status)
		}
		if rowCount != 1 {
			t.Errorf("row_count = %d, want 1", rowCount)
		}
		if report != nil {
			t.Errorf("error_report = %s, want nil", report)
		}
	})

	t.Run("file with row errors goes failed with a report", func(t *testing.T) {
		csv := header + "essay,Write about x,,,,,,,,\n"
		writeImportFile(t, storage.Dir, "bad-rows.csv", csv)
		importID := seedImport(t, ctx, sqlDB, quizID, "owner@school.test", "bad-rows.csv")

		if err := quiz.ValidateImport(ctx, sqlDB, storage, importID); err != nil {
			t.Fatalf("ValidateImport: %v", err)
		}

		status, rowCount, report := loadImport(t, ctx, sqlDB, importID)
		if status != "failed" {
			t.Errorf("status = %q, want failed", status)
		}
		if rowCount != 0 {
			t.Errorf("row_count = %d, want 0", rowCount)
		}
		var errs []quiz.ImportRowError
		if err := json.Unmarshal(report, &errs); err != nil {
			t.Fatalf("unmarshal error_report: %v", err)
		}
		if len(errs) != 1 || errs[0].Column != "type" {
			t.Errorf("error_report = %+v, want a single type error", errs)
		}
	})

	t.Run("missing file fails with a file-level error", func(t *testing.T) {
		importID := seedImport(t, ctx, sqlDB, quizID, "owner@school.test", "does-not-exist.csv")

		if err := quiz.ValidateImport(ctx, sqlDB, storage, importID); err != nil {
			t.Fatalf("ValidateImport: %v", err)
		}

		status, rowCount, report := loadImport(t, ctx, sqlDB, importID)
		if status != "failed" {
			t.Errorf("status = %q, want failed", status)
		}
		if rowCount != 0 {
			t.Errorf("row_count = %d, want 0", rowCount)
		}
		var errs []quiz.ImportRowError
		if err := json.Unmarshal(report, &errs); err != nil {
			t.Fatalf("unmarshal error_report: %v", err)
		}
		if len(errs) != 1 || errs[0].Column != "file" {
			t.Errorf("error_report = %+v, want a single file-level error", errs)
		}
	})

	t.Run("already-resolved import is left untouched", func(t *testing.T) {
		csv := header + "truefalse,Sky is blue,,,,,,,true,\n"
		writeImportFile(t, storage.Dir, "resolved.csv", csv)
		importID := seedImport(t, ctx, sqlDB, quizID, "owner@school.test", "resolved.csv")
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE imports SET status = 'committed', row_count = 1 WHERE id = $1`, importID); err != nil {
			t.Fatalf("pre-resolve import: %v", err)
		}

		if err := quiz.ValidateImport(ctx, sqlDB, storage, importID); err != nil {
			t.Fatalf("ValidateImport: %v", err)
		}

		status, rowCount, report := loadImport(t, ctx, sqlDB, importID)
		if status != "committed" {
			t.Errorf("status = %q, want committed (untouched)", status)
		}
		if rowCount != 1 {
			t.Errorf("row_count = %d, want 1 (untouched)", rowCount)
		}
		if report != nil {
			t.Errorf("error_report = %s, want nil (untouched)", report)
		}
	})
}

func seedQuiz(t *testing.T, ctx context.Context, sqlDB *sql.DB, ownerEmail, title string) string {
	t.Helper()
	var quizID string
	err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO quizzes (owner_id, title)
		 SELECT id, $2 FROM users WHERE email = $1
		 RETURNING id`, ownerEmail, title).Scan(&quizID)
	if err != nil {
		t.Fatalf("seed quiz: %v", err)
	}
	return quizID
}

func seedImport(t *testing.T, ctx context.Context, sqlDB *sql.DB, quizID, uploaderEmail, fileRef string) string {
	t.Helper()
	var importID string
	err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO imports (quiz_id, uploaded_by, file_ref)
		 SELECT $1, id, $3 FROM users WHERE email = $2
		 RETURNING id`, quizID, uploaderEmail, fileRef).Scan(&importID)
	if err != nil {
		t.Fatalf("seed import: %v", err)
	}
	return importID
}

func loadImport(t *testing.T, ctx context.Context, sqlDB *sql.DB, importID string) (status string, rowCount int, report []byte) {
	t.Helper()
	var rc sql.NullInt64
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status, row_count, error_report FROM imports WHERE id = $1`, importID).
		Scan(&status, &rc, &report); err != nil {
		t.Fatalf("load import: %v", err)
	}
	return status, int(rc.Int64), report
}

func writeImportFile(t *testing.T, dir, name, content string) {
	t.Helper()
	writeImportFileBytes(t, dir, name, []byte(content))
}

func writeImportFileBytes(t *testing.T, dir, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
		t.Fatalf("write import file %s: %v", name, err)
	}
}

// buildXLSXFixture builds a minimal real .xlsx (the shared-strings table
// plus a single-sheet worksheet, the two XML parts quiz.ParseImportXLSX
// reads) holding one valid truefalse row, to prove the worker's real
// dispatch path (ValidateImport -> storage.Open -> quiz.ParseImportFile ->
// quiz.ParseImportXLSX) end to end - the more detailed cell-type/format
// coverage lives in importxlsx_test.go's unit tests.
func buildXLSXFixture(t *testing.T) []byte {
	t.Helper()
	strs := []string{
		"type", "question", "option_a", "option_b", "option_c", "option_d",
		"option_e", "option_f", "correct", "points", "truefalse", "Sky is blue", "true",
	}
	var sst strings.Builder
	fmt.Fprintf(&sst, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="%d" uniqueCount="%d">`, len(strs), len(strs))
	for _, s := range strs {
		fmt.Fprintf(&sst, `<si><t>%s</t></si>`, s)
	}
	sst.WriteString(`</sst>`)

	worksheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` +
		`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c>` +
		`<c r="C1" t="s"><v>2</v></c><c r="D1" t="s"><v>3</v></c><c r="E1" t="s"><v>4</v></c>` +
		`<c r="F1" t="s"><v>5</v></c><c r="G1" t="s"><v>6</v></c><c r="H1" t="s"><v>7</v></c>` +
		`<c r="I1" t="s"><v>8</v></c><c r="J1" t="s"><v>9</v></c></row>` +
		`<row r="2"><c r="A2" t="s"><v>10</v></c><c r="B2" t="s"><v>11</v></c><c r="I2" t="s"><v>12</v></c></row>` +
		`</sheetData></worksheet>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"xl/worksheets/sheet1.xml": worksheet,
		"xl/sharedStrings.xml":     sst.String(),
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}
