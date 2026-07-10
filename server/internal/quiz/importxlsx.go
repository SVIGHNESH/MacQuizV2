package quiz

import (
	"io"

	"macquiz/server/internal/tabular"
)

// ParseImportFile parses a bulk-upload template in either format the
// template doc names (docs/07 section 2 step 1: "CSV/XLSX template");
// tabular.Records dispatches on the file's own leading bytes, and
// parseImportRecords holds every validation rule so the two formats can
// never silently diverge on what counts as a valid row.
func ParseImportFile(r io.Reader) ([]ImportRow, []ImportRowError, error) {
	records, err := tabular.Records(r)
	if err != nil {
		return nil, nil, err
	}
	return parseImportRecords(records[0], records[1:])
}

// ParseImportXLSX parses a bulk-upload template saved as .xlsx into the
// same normalized rows and error report ParseImportCSV produces.
func ParseImportXLSX(r io.Reader) ([]ImportRow, []ImportRowError, error) {
	records, err := tabular.XLSX(r)
	if err != nil {
		return nil, nil, err
	}
	return parseImportRecords(records[0], records[1:])
}
