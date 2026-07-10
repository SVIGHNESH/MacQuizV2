package authusers_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"

	"macquiz/server/internal/authusers"
)

func TestParseUserImportFile_ValidCSV(t *testing.T) {
	csv := "role,email,full_name\n" +
		"student,priya@school.test,Priya Sharma\n" +
		"Teacher,ANITA@school.test,\"Desai, Anita\"\n"
	rows, errs, err := authusers.ParseUserImportFile(strings.NewReader(csv))
	if err != nil || len(errs) > 0 {
		t.Fatalf("ParseUserImportFile = (%v, %v), want clean parse", errs, err)
	}
	if len(rows) != 2 {
		t.Fatalf("parsed %d rows, want 2", len(rows))
	}
	if rows[0] != (authusers.UserImportRow{Row: 1, Role: "student", Email: "priya@school.test", FullName: "Priya Sharma"}) {
		t.Fatalf("row 1 = %+v", rows[0])
	}
	// Role is case-normalized; a quoted name keeps its comma.
	if rows[1].Role != "teacher" || rows[1].FullName != "Desai, Anita" {
		t.Fatalf("row 2 = %+v", rows[1])
	}
}

func TestParseUserImportFile_MissingColumn(t *testing.T) {
	csv := "role,email\nstudent,priya@school.test\n"
	_, _, err := authusers.ParseUserImportFile(strings.NewReader(csv))
	if err == nil || !strings.Contains(err.Error(), `missing required column "full_name"`) {
		t.Fatalf("err = %v, want missing full_name column", err)
	}
}

func TestParseUserImportFile_RowErrors(t *testing.T) {
	csv := "role,email,full_name\n" +
		"admin,root@school.test,Root\n" +
		"student,not-an-email,Someone\n" +
		"student,ok@school.test,\n" +
		"student,dupe@school.test,First\n" +
		"teacher,DUPE@school.test,Second\n"
	rows, errs, err := authusers.ParseUserImportFile(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseUserImportFile: %v", err)
	}
	if len(rows) != 1 || rows[0].Email != "dupe@school.test" {
		t.Fatalf("rows = %+v, want only the first dupe row to survive", rows)
	}
	want := map[int]string{1: "role", 2: "email", 3: "full_name", 5: "email"}
	if len(errs) != len(want) {
		t.Fatalf("errs = %+v, want %d errors", errs, len(want))
	}
	for _, e := range errs {
		if want[e.Row] != e.Column {
			t.Fatalf("unexpected error %+v", e)
		}
	}
	// The case-insensitive duplicate names the original row.
	if !strings.Contains(errs[3].Message, "duplicate of row 4") {
		t.Fatalf("dup error = %+v, want reference to row 4", errs[3])
	}
}

func TestParseUserImportFile_SkipsBlankRowsKeepsNumbering(t *testing.T) {
	csv := "role,email,full_name\n" +
		"student,a@school.test,A\n" +
		",,\n" +
		"student,broken,B\n"
	rows, errs, err := authusers.ParseUserImportFile(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseUserImportFile: %v", err)
	}
	if len(rows) != 1 || len(errs) != 1 {
		t.Fatalf("got %d rows / %d errs, want 1 / 1", len(rows), len(errs))
	}
	// The blank row is skipped but still counted, so the error points at the
	// row the admin sees in their spreadsheet.
	if errs[0].Row != 3 || errs[0].Column != "email" {
		t.Fatalf("err = %+v, want row 3 email", errs[0])
	}
}

func TestParseUserImportFile_UnquotedCommaShiftsColumns(t *testing.T) {
	csv := "role,email,full_name\n" +
		"student,priya@school.test,Sharma, Priya\n"
	rows, errs, err := authusers.ParseUserImportFile(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseUserImportFile: %v", err)
	}
	if len(rows) != 0 || len(errs) != 1 {
		t.Fatalf("got %d rows / %d errs, want 0 / 1", len(rows), len(errs))
	}
	if errs[0].Column != "row" || !strings.Contains(errs[0].Message, "double quotes") {
		t.Fatalf("err = %+v, want a row-level quote-the-comma message", errs[0])
	}
}

func TestParseUserImportFile_RowLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("role,email,full_name\n")
	for i := 0; i <= authusers.MaxUserImportRows; i++ {
		fmt.Fprintf(&b, "student,s%d@school.test,Student %d\n", i, i)
	}
	rows, errs, err := authusers.ParseUserImportFile(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("ParseUserImportFile: %v", err)
	}
	if len(rows) != authusers.MaxUserImportRows {
		t.Fatalf("parsed %d rows, want the %d-row cap", len(rows), authusers.MaxUserImportRows)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "row limit") {
		t.Fatalf("errs = %+v, want a single row-limit error", errs)
	}
}

func TestParseUserImportFile_XLSX(t *testing.T) {
	data := rosterXLSX(t, [][]string{
		{"role", "email", "full_name"},
		{"student", "priya@school.test", "Priya Sharma"},
		{"teacher", "anita@school.test", "Anita Desai"},
	})
	rows, errs, err := authusers.ParseUserImportFile(bytes.NewReader(data))
	if err != nil || len(errs) > 0 {
		t.Fatalf("ParseUserImportFile(xlsx) = (%v, %v), want clean parse", errs, err)
	}
	if len(rows) != 2 || rows[1].Role != "teacher" || rows[1].Email != "anita@school.test" {
		t.Fatalf("rows = %+v", rows)
	}
}

// rosterXLSX builds a minimal single-sheet workbook using inline strings -
// just the two XML parts the XLSX reader consumes - so the roster test
// exercises the real sniff-and-dispatch path without a spreadsheet library.
func rosterXLSX(t *testing.T, grid [][]string) []byte {
	t.Helper()
	var sheet strings.Builder
	sheet.WriteString(`<?xml version="1.0"?><worksheet><sheetData>`)
	for r, row := range grid {
		fmt.Fprintf(&sheet, `<row r="%d">`, r+1)
		for c, val := range row {
			fmt.Fprintf(&sheet, `<c r="%c%d" t="inlineStr"><is><t>%s</t></is></c>`, 'A'+c, r+1, val)
		}
		sheet.WriteString(`</row>`)
	}
	sheet.WriteString(`</sheetData></worksheet>`)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		t.Fatalf("create sheet entry: %v", err)
	}
	if _, err := f.Write([]byte(sheet.String())); err != nil {
		t.Fatalf("write sheet: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close xlsx zip: %v", err)
	}
	return buf.Bytes()
}
