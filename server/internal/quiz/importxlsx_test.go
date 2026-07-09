package quiz

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

type xlsxCellSpec struct {
	ref string
	typ string // "s" (shared string), "n" (number), "b" (boolean), "inlineStr"
	val string
}

// buildTestXLSX assembles a minimal but real .xlsx (a ZIP archive of the two
// XML parts ParseImportXLSX actually reads) from a grid of cell specs, one
// row of specs per worksheet row - it exercises the same shared-strings and
// worksheet XML shapes a real spreadsheet application writes.
func buildTestXLSX(t *testing.T, rows [][]xlsxCellSpec) []byte {
	t.Helper()

	sstIndex := map[string]int{}
	var sstList []string
	sharedIndex := func(s string) int {
		if i, ok := sstIndex[s]; ok {
			return i
		}
		i := len(sstList)
		sstList = append(sstList, s)
		sstIndex[s] = i
		return i
	}

	var sheetData strings.Builder
	sheetData.WriteString(`<sheetData>`)
	for rowNum, row := range rows {
		fmt.Fprintf(&sheetData, `<row r="%d">`, rowNum+1)
		for _, c := range row {
			switch c.typ {
			case "s":
				fmt.Fprintf(&sheetData, `<c r="%s" t="s"><v>%d</v></c>`, c.ref, sharedIndex(c.val))
			case "n":
				fmt.Fprintf(&sheetData, `<c r="%s"><v>%s</v></c>`, c.ref, c.val)
			case "b":
				fmt.Fprintf(&sheetData, `<c r="%s" t="b"><v>%s</v></c>`, c.ref, c.val)
			case "inlineStr":
				fmt.Fprintf(&sheetData, `<c r="%s" t="inlineStr"><is><t>%s</t></is></c>`, c.ref, c.val)
			default:
				t.Fatalf("unknown cell spec type %q", c.typ)
			}
		}
		sheetData.WriteString(`</row>`)
	}
	sheetData.WriteString(`</sheetData>`)

	worksheetXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		sheetData.String() + `</worksheet>`

	var sst strings.Builder
	fmt.Fprintf(&sst, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="%d" uniqueCount="%d">`, len(sstList), len(sstList))
	for _, s := range sstList {
		fmt.Fprintf(&sst, `<si><t>%s</t></si>`, s)
	}
	sst.WriteString(`</sst>`)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeZipEntry(t, zw, "xl/worksheets/sheet1.xml", worksheetXML)
	writeZipEntry(t, zw, "xl/sharedStrings.xml", sst.String())
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func writeZipEntry(t *testing.T, zw *zip.Writer, name, content string) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// header row column letters, matching importTemplateColumns' order:
// A=type B=question C=option_a D=option_b E=option_c F=option_d
// G=option_e H=option_f I=correct J=points
func xlsxHeaderRow() []xlsxCellSpec {
	cols := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}
	var row []xlsxCellSpec
	for i, name := range importTemplateColumns {
		row = append(row, xlsxCellSpec{ref: cols[i] + "1", typ: "s", val: name})
	}
	return row
}

func TestParseImportXLSX_ValidFile(t *testing.T) {
	rows := [][]xlsxCellSpec{
		xlsxHeaderRow(),
		{ // single choice, sparse option_c..f cells entirely omitted
			{ref: "A2", typ: "s", val: "single"},
			{ref: "B2", typ: "s", val: "Pick red"},
			{ref: "C2", typ: "s", val: "Red"},
			{ref: "D2", typ: "s", val: "Blue"},
			{ref: "I2", typ: "s", val: "a"},
			{ref: "J2", typ: "n", val: "2"},
		},
		{ // multi choice, question as an inline string instead of shared
			{ref: "A3", typ: "s", val: "multi"},
			{ref: "B3", typ: "inlineStr", val: "Pick primaries"},
			{ref: "C3", typ: "s", val: "Red"},
			{ref: "D3", typ: "s", val: "Blue"},
			{ref: "E3", typ: "s", val: "Green"},
			{ref: "I3", typ: "s", val: "a,c"},
			{ref: "J3", typ: "n", val: "1"},
		},
		{ // true/false, correct as a real XLSX boolean cell (t="b")
			{ref: "A4", typ: "s", val: "truefalse"},
			{ref: "B4", typ: "s", val: "Sky is blue"},
			{ref: "I4", typ: "b", val: "1"},
		},
	}

	data := buildTestXLSX(t, rows)
	parsed, errs, err := ParseImportXLSX(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseImportXLSX: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("want no row errors, got %+v", errs)
	}
	if len(parsed) != 3 {
		t.Fatalf("want 3 parsed rows, got %d", len(parsed))
	}
	if string(parsed[0].Input.Correct) != `"a"` {
		t.Errorf("single correct = %s, want \"a\"", parsed[0].Input.Correct)
	}
	if got := *parsed[0].Input.Points; got != 2 {
		t.Errorf("points = %v, want 2", got)
	}
	if string(parsed[1].Input.Correct) != `["a","c"]` {
		t.Errorf("multi correct = %s, want [\"a\",\"c\"]", parsed[1].Input.Correct)
	}
	if string(parsed[2].Input.Correct) != `true` {
		t.Errorf("boolean-cell correct = %s, want true (from XLSX t=\"b\")", parsed[2].Input.Correct)
	}
}

func TestParseImportXLSX_MissingColumn(t *testing.T) {
	rows := [][]xlsxCellSpec{
		{
			{ref: "A1", typ: "s", val: "type"},
			{ref: "B1", typ: "s", val: "question"},
			{ref: "C1", typ: "s", val: "correct"},
		},
		{
			{ref: "A2", typ: "s", val: "single"},
			{ref: "B2", typ: "s", val: "x"},
			{ref: "C2", typ: "s", val: "a"},
		},
	}
	data := buildTestXLSX(t, rows)
	_, _, err := ParseImportXLSX(bytes.NewReader(data))
	if err == nil {
		t.Fatal("want an error for a missing required column")
	}
}

func TestParseImportFile_DispatchesByContent(t *testing.T) {
	csvData := importHeader + "truefalse,Sky is blue,,,,,,,true,\n"
	rows, errs, err := ParseImportFile(strings.NewReader(csvData))
	if err != nil {
		t.Fatalf("ParseImportFile(csv): %v", err)
	}
	if len(errs) != 0 || len(rows) != 1 {
		t.Fatalf("ParseImportFile(csv) = %d rows, %d errs, want 1 row, 0 errs", len(rows), len(errs))
	}

	xlsxData := buildTestXLSX(t, [][]xlsxCellSpec{
		xlsxHeaderRow(),
		{
			{ref: "A2", typ: "s", val: "truefalse"},
			{ref: "B2", typ: "s", val: "Sky is blue"},
			{ref: "I2", typ: "s", val: "true"},
		},
	})
	rows, errs, err = ParseImportFile(bytes.NewReader(xlsxData))
	if err != nil {
		t.Fatalf("ParseImportFile(xlsx): %v", err)
	}
	if len(errs) != 0 || len(rows) != 1 {
		t.Fatalf("ParseImportFile(xlsx) = %d rows, %d errs, want 1 row, 0 errs", len(rows), len(errs))
	}
}
