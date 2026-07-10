// Package tabular reads spreadsheet-shaped upload files - CSV or XLSX - into
// plain rows of cells. It is a leaf package like httpapi: business modules
// (quiz question imports, authusers roster imports) import it for the
// file-format mechanics and keep every validation rule to themselves, so the
// two import features can never diverge on how a file's bytes become records.
package tabular

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
)

// xlsxSignature is the leading four bytes of every .xlsx file - it is a ZIP
// archive under the hood - so Records can tell the two supported formats
// apart from the file's own bytes instead of a client-supplied content type.
var xlsxSignature = []byte("PK\x03\x04")

// Records reads an upload in either supported format, dispatching on the
// file's own leading bytes rather than a client-supplied content type or
// filename extension - a client cannot make the parser misread a file just
// by lying about what it is. The first record is the header row; a file
// without one is an error, so callers may index records[0] directly.
func Records(r io.Reader) ([][]string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if bytes.HasPrefix(data, xlsxSignature) {
		return XLSX(bytes.NewReader(data))
	}
	return CSV(bytes.NewReader(data))
}

// CSV reads a .csv upload into records, header first. Leading whitespace is
// trimmed and short rows are allowed (spreadsheet software drops empty
// trailing cells), matching what XLSX produces for the same sheet.
func CSV(r io.Reader) ([][]string, error) {
	reader := csv.NewReader(r)
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	records := [][]string{header}
	for {
		rec, rerr := reader.Read()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("row %d: %w", len(records), rerr)
		}
		records = append(records, rec)
	}
	return records, nil
}
