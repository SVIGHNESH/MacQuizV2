package quiz

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// xlsxSignature is the leading four bytes of every .xlsx file - it is a ZIP
// archive under the hood - so ParseImportFile can tell the two supported
// template formats (docs/07 section 2 step 1: "CSV/XLSX template") apart
// from the file's own bytes instead of a client-supplied content type.
var xlsxSignature = []byte("PK\x03\x04")

// ParseImportFile parses a bulk-upload template in either format the
// template doc names, dispatching on the file's own leading bytes rather
// than a client-supplied content type or filename extension - a client
// cannot make the parser misread a file just by lying about what it is.
func ParseImportFile(r io.Reader) ([]ImportRow, []ImportRowError, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, fmt.Errorf("read file: %w", err)
	}
	if bytes.HasPrefix(data, xlsxSignature) {
		return ParseImportXLSX(bytes.NewReader(data))
	}
	return ParseImportCSV(bytes.NewReader(data))
}

// ParseImportXLSX parses a bulk-upload template saved as .xlsx into the
// same normalized rows and error report ParseImportCSV produces -
// parseImportRecords holds every validation rule, shared by both formats.
//
// Only the first worksheet is read: xl/worksheets/sheet1.xml if present,
// otherwise the lexicographically first xl/worksheets/*.xml entry.
// Resolving sheet order properly needs the xl/workbook.xml relationship
// graph; a teacher filling in the single-sheet template never has more than
// one sheet to read from, so that graph is unneeded complexity here.
func ParseImportXLSX(r io.Reader) ([]ImportRow, []ImportRowError, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, fmt.Errorf("read file: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, fmt.Errorf("open xlsx: %w", err)
	}

	sst, err := readXLSXSharedStrings(zr)
	if err != nil {
		return nil, nil, err
	}
	sheet, err := findXLSXFirstSheet(zr)
	if err != nil {
		return nil, nil, err
	}
	records, err := readXLSXRows(sheet, sst)
	if err != nil {
		return nil, nil, err
	}
	if len(records) == 0 {
		return nil, nil, fmt.Errorf("read header: empty sheet")
	}
	return parseImportRecords(records[0], records[1:])
}

func findXLSXFirstSheet(zr *zip.Reader) (*zip.File, error) {
	var candidates []*zip.File
	for _, f := range zr.File {
		if f.Name == "xl/worksheets/sheet1.xml" {
			return f, nil
		}
		if strings.HasPrefix(f.Name, "xl/worksheets/") && strings.HasSuffix(f.Name, ".xml") {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("xlsx has no worksheet")
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	return candidates[0], nil
}

// readXLSXSharedStrings loads xl/sharedStrings.xml, the table every text
// cell's <c t="s"> indexes into. A workbook with no text cells at all (or
// one written with inline strings only) may omit the part entirely, which
// is not an error - it just means no cell will resolve type "s".
func readXLSXSharedStrings(zr *zip.Reader) ([]string, error) {
	f := findZipFile(zr, "xl/sharedStrings.xml")
	if f == nil {
		return nil, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open shared strings: %w", err)
	}
	defer rc.Close()

	var sst struct {
		Items []xlsxRichText `xml:"si"`
	}
	if err := xml.NewDecoder(rc).Decode(&sst); err != nil {
		return nil, fmt.Errorf("parse shared strings: %w", err)
	}
	out := make([]string, len(sst.Items))
	for i, it := range sst.Items {
		out[i] = it.text()
	}
	return out, nil
}

func findZipFile(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// xlsxRichText matches both a shared-string table entry (<si>) and an
// inline cell value (<is>): either a direct <t> or one or more rich-text
// <r><t> runs, which text() flattens into one plain string.
type xlsxRichText struct {
	T    string            `xml:"t"`
	Runs []xlsxRichTextRun `xml:"r"`
}

type xlsxRichTextRun struct {
	T string `xml:"t"`
}

func (rt xlsxRichText) text() string {
	if rt.T != "" || len(rt.Runs) == 0 {
		return rt.T
	}
	var b strings.Builder
	for _, r := range rt.Runs {
		b.WriteString(r.T)
	}
	return b.String()
}

type xlsxWorksheet struct {
	SheetData struct {
		Rows []xlsxRow `xml:"row"`
	} `xml:"sheetData"`
}

type xlsxRow struct {
	Cells []xlsxCell `xml:"c"`
}

type xlsxCell struct {
	Ref    string        `xml:"r,attr"`
	Type   string        `xml:"t,attr"`
	Value  string        `xml:"v"`
	Inline *xlsxRichText `xml:"is"`
}

// readXLSXRows turns a worksheet's <sheetData> into the same [][]string
// shape ParseImportCSV works from, placing each cell at its real column
// index (from its "r" ref, e.g. "C7") rather than its position in the XML -
// spreadsheet software omits empty trailing cells from a row entirely, so
// positional indexing alone would silently shift later columns left.
func readXLSXRows(f *zip.File, sst []string) ([][]string, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open worksheet: %w", err)
	}
	defer rc.Close()

	var ws xlsxWorksheet
	if err := xml.NewDecoder(rc).Decode(&ws); err != nil {
		return nil, fmt.Errorf("parse worksheet: %w", err)
	}

	rows := make([][]string, 0, len(ws.SheetData.Rows))
	for _, row := range ws.SheetData.Rows {
		width := 0
		cols := make(map[int]string, len(row.Cells))
		for _, c := range row.Cells {
			col, cerr := xlsxColumnIndex(c.Ref)
			if cerr != nil {
				continue
			}
			cols[col] = xlsxCellText(c, sst)
			if col+1 > width {
				width = col + 1
			}
		}
		rec := make([]string, width)
		for col, val := range cols {
			rec[col] = val
		}
		rows = append(rows, rec)
	}
	return rows, nil
}

func xlsxCellText(c xlsxCell, sst []string) string {
	switch c.Type {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(c.Value))
		if err != nil || i < 0 || i >= len(sst) {
			return ""
		}
		return sst[i]
	case "inlineStr":
		if c.Inline != nil {
			return c.Inline.text()
		}
		return ""
	case "b":
		if strings.TrimSpace(c.Value) == "1" {
			return "TRUE"
		}
		return "FALSE"
	default: // "n" (number), "str" (formula string result), or unset
		return c.Value
	}
}

// xlsxColumnIndex turns a cell reference like "AC12" into a 0-based column
// index (A=0), ignoring the row-number suffix.
func xlsxColumnIndex(ref string) (int, error) {
	i := 0
	for i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("cell %q has no column letters", ref)
	}
	col := 0
	for _, ch := range ref[:i] {
		col = col*26 + int(ch-'A'+1)
	}
	return col - 1, nil
}
