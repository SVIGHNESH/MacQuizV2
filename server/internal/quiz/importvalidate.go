package quiz

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// MaxImportRows bounds a single bulk-upload file (docs/07-authoring-imports-
// analytics.md section 2: "Limits: 10 MB, 500 rows").
const MaxImportRows = 500

// importTemplateColumns is the fixed template header (docs/07 section 2 step
// 1): "type, question, option_a..option_f, correct, points".
var importTemplateColumns = []string{
	"type", "question",
	"option_a", "option_b", "option_c", "option_d", "option_e", "option_f",
	"correct", "points",
}

// topicColumn is the one OPTIONAL template column: a file written against the
// original fixed header still parses, and one that carries topic tags feeds
// student_stats.topic_strengths. It is not in importTemplateColumns because a
// missing topic column is not a malformed file.
const topicColumn = "topic"

// ImportRowError is one validation failure against a specific row/column of
// a bulk-import file, shaped to serialize directly into imports.error_report.
type ImportRowError struct {
	Row     int    `json:"row"` // 1-based, header excluded
	Column  string `json:"column"`
	Message string `json:"message"`
}

// ImportRow is one parsed template row, normalized into the same
// QuestionInput shape one-by-one authoring uses.
type ImportRow struct {
	Row   int
	Input QuestionInput
}

// ParseImportCSV parses a bulk-upload template saved as .csv (docs/07
// section 2 step 1) into normalized question inputs plus a per-row/column
// error report; parseImportRecords holds every validation rule shared with
// ParseImportXLSX. Nothing is written to the database: a non-empty error
// report means the import cannot commit and the teacher must fix and
// re-upload.
func ParseImportCSV(r io.Reader) ([]ImportRow, []ImportRowError, error) {
	reader := csv.NewReader(r)
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1 // trailing option columns may be short

	header, err := reader.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("read header: %w", err)
	}
	var records [][]string
	for {
		rec, rerr := reader.Read()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, nil, fmt.Errorf("row %d: %w", len(records)+1, rerr)
		}
		records = append(records, rec)
	}
	return parseImportRecords(header, records)
}

// parseImportRecords holds every per-row/column validation rule from docs/07
// section 2 step 3, shared by ParseImportCSV and ParseImportXLSX so the two
// formats can never silently diverge on what counts as a valid row.
//
// The returned error is non-nil only when the file is too malformed to
// review at all (missing a required column) - every per-row problem
// (unknown type, missing correct answer, correct answer not among the
// options, duplicate question text, malformed points) surfaces as an
// ImportRowError instead, matching the "review" step's contract of showing
// failing rows inline rather than rejecting the upload.
func parseImportRecords(header []string, records [][]string) ([]ImportRow, []ImportRowError, error) {
	col := make(map[string]int, len(header))
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, want := range importTemplateColumns {
		if _, ok := col[want]; !ok {
			return nil, nil, fmt.Errorf("missing required column %q", want)
		}
	}
	get := func(rec []string, name string) string {
		i := col[name]
		if i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var rows []ImportRow
	var errs []ImportRowError
	seenQuestions := map[string]int{} // normalized question text -> first row

	rowNum := 0
	for _, rec := range records {
		rowNum++
		if rowNum > MaxImportRows {
			errs = append(errs, ImportRowError{
				Row:     rowNum,
				Message: fmt.Sprintf("file exceeds the %d row limit", MaxImportRows),
			})
			break
		}

		typ := strings.ToLower(get(rec, "type"))
		question := get(rec, "question")

		var opts []option
		for _, letter := range []string{"a", "b", "c", "d", "e", "f"} {
			if text := get(rec, "option_"+letter); text != "" {
				opts = append(opts, option{Key: letter, Text: text})
			}
		}

		in := QuestionInput{Type: typ}
		bodyJSON, _ := json.Marshal(questionBody{Text: question})
		in.Body = bodyJSON

		var rowErrs []ImportRowError
		if question == "" {
			rowErrs = append(rowErrs, ImportRowError{Row: rowNum, Column: "question", Message: "question text is required"})
		} else {
			norm := strings.ToLower(question)
			if first, dup := seenQuestions[norm]; dup {
				rowErrs = append(rowErrs, ImportRowError{Row: rowNum, Column: "question", Message: fmt.Sprintf("duplicate of row %d", first)})
			} else {
				seenQuestions[norm] = rowNum
			}
		}

		correctRaw := get(rec, "correct")
		switch typ {
		case "single", "multi":
			optsJSON, _ := json.Marshal(opts)
			in.Options = optsJSON
			in.Correct = buildChoiceCorrect(typ, correctRaw)
		case "truefalse":
			in.Correct = buildBoolCorrect(correctRaw)
		case "short":
			in.Correct = buildShortCorrect(correctRaw)
		}

		// get() would resolve a missing column to index 0 (the zero value of
		// the col map), so the optional topic column is read only when the
		// header actually declared it.
		if _, ok := col[topicColumn]; ok {
			if topicRaw := get(rec, topicColumn); topicRaw != "" {
				in.Topic = &topicRaw
			}
		}

		if pointsRaw := get(rec, "points"); pointsRaw != "" {
			p, perr := strconv.ParseFloat(pointsRaw, 64)
			if perr != nil {
				rowErrs = append(rowErrs, ImportRowError{Row: rowNum, Column: "points", Message: "points must be a number"})
			} else {
				in.Points = &p
			}
		}

		fields := in.Validate()
		for _, c := range []string{"type", "body", "options", "correct", "points", "topic"} {
			if msg, ok := fields[c]; ok {
				rowErrs = append(rowErrs, ImportRowError{Row: rowNum, Column: c, Message: msg})
			}
		}

		if len(rowErrs) > 0 {
			errs = append(errs, rowErrs...)
			continue
		}
		rows = append(rows, ImportRow{Row: rowNum, Input: in})
	}

	return rows, errs, nil
}

// buildChoiceCorrect turns the template's option-letter cell into the same
// JSON shape one-by-one authoring sends: a single key for "single", an array
// of keys for "multi". An empty or out-of-range letter still marshals to
// valid JSON so QuestionInput.Validate reports it via the existing
// "must be the key of one of the options" check rather than a separate path.
func buildChoiceCorrect(typ, raw string) json.RawMessage {
	switch typ {
	case "single":
		b, _ := json.Marshal(strings.ToLower(strings.TrimSpace(raw)))
		return b
	case "multi":
		var keys []string
		for _, part := range strings.Split(raw, ",") {
			if part = strings.ToLower(strings.TrimSpace(part)); part != "" {
				keys = append(keys, part)
			}
		}
		b, _ := json.Marshal(keys)
		return b
	}
	return nil
}

// buildBoolCorrect accepts "true"/"false" case-insensitively; anything else
// (including empty) marshals to a JSON string, which fails the bool
// unmarshal in Validate and surfaces as "must be true or false".
func buildBoolCorrect(raw string) json.RawMessage {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true":
		return json.RawMessage("true")
	case "false":
		return json.RawMessage("false")
	default:
		b, _ := json.Marshal(raw)
		return b
	}
}

// buildShortCorrect splits accepted short answers on "|" since a comma is a
// plausible character inside an answer itself. An empty cell returns nil so
// Validate's own "must be {...}" message fires instead of a duplicate here.
func buildShortCorrect(raw string) json.RawMessage {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var accepted []string
	for _, part := range strings.Split(raw, "|") {
		if part = strings.TrimSpace(part); part != "" {
			accepted = append(accepted, part)
		}
	}
	b, _ := json.Marshal(shortCorrect{Accepted: accepted})
	return b
}
