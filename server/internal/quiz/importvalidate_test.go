package quiz

import (
	"strings"
	"testing"
)

const importHeader = "type,question,option_a,option_b,option_c,option_d,option_e,option_f,correct,points\n"

// An unquoted comma inside a cell splits it and shifts every later column
// right; the report must name that, not the shifted cells' confusing
// downstream failures (a real support case: "points must be a number" on a
// truefalse row whose question carried a comma).
func TestParseImportCSV_UnquotedCommaShiftsColumns(t *testing.T) {
	csv := importHeader +
		"truefalse,In Big-O notation, O(1) is constant time.,,,,,,,true,1\n"
	rows, errs, err := ParseImportCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseImportCSV: %v", err)
	}
	if len(rows) != 0 || len(errs) != 1 {
		t.Fatalf("got %d rows / %d errs, want 0 / 1", len(rows), len(errs))
	}
	if errs[0].Row != 1 || errs[0].Column != "row" ||
		!strings.Contains(errs[0].Message, "double quotes") {
		t.Fatalf("err = %+v, want a row-level quote-the-comma message", errs[0])
	}
}

// A bare trailing comma (one empty extra cell) is harmless and must not trip
// the overflow check.
func TestParseImportCSV_TrailingEmptyCellIsLegal(t *testing.T) {
	csv := importHeader + "truefalse,Sky is blue,,,,,,,true,1,\n"
	rows, errs, err := ParseImportCSV(strings.NewReader(csv))
	if err != nil || len(errs) != 0 || len(rows) != 1 {
		t.Fatalf("ParseImportCSV = (%d rows, %+v, %v), want one clean row", len(rows), errs, err)
	}
}

func TestParseImportCSV_ValidFile(t *testing.T) {
	csv := importHeader +
		"single,Pick red,Red,Blue,,,,,a,2\n" +
		"multi,Pick primaries,Red,Blue,Green,,,,\"a,c\",1\n" +
		"truefalse,Sky is blue,,,,,,,true,\n" +
		"short,Capital of France,,,,,,,Paris|paris,1\n"

	rows, errs, err := ParseImportCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseImportCSV: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("want no row errors, got %+v", errs)
	}
	if len(rows) != 4 {
		t.Fatalf("want 4 parsed rows, got %d", len(rows))
	}
	if string(rows[0].Input.Correct) != `"a"` {
		t.Errorf("single correct = %s, want \"a\"", rows[0].Input.Correct)
	}
	if string(rows[1].Input.Correct) != `["a","c"]` {
		t.Errorf("multi correct = %s, want [\"a\",\"c\"]", rows[1].Input.Correct)
	}
	if got := *rows[0].Input.Points; got != 2 {
		t.Errorf("points = %v, want 2", got)
	}
	if got := *rows[3].Input.Points; got != 1 {
		t.Errorf("default points = %v, want 1", got)
	}
}

func TestParseImportCSV_UnknownType(t *testing.T) {
	csv := importHeader + "essay,Write about x,,,,,,,,\n"
	rows, errs, err := ParseImportCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseImportCSV: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("want 0 parsed rows, got %d", len(rows))
	}
	if !hasColumnError(errs, 1, "type") {
		t.Errorf("want a row-1 type error, got %+v", errs)
	}
}

func TestParseImportCSV_MissingCorrect(t *testing.T) {
	csv := importHeader + "single,Pick one,Red,Blue,,,,,,\n"
	_, errs, err := ParseImportCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseImportCSV: %v", err)
	}
	if !hasColumnError(errs, 1, "correct") {
		t.Errorf("want a row-1 correct error, got %+v", errs)
	}
}

func TestParseImportCSV_CorrectNotAmongOptions(t *testing.T) {
	csv := importHeader + "single,Pick one,Red,Blue,,,,,z,\n"
	_, errs, err := ParseImportCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseImportCSV: %v", err)
	}
	if !hasColumnError(errs, 1, "correct") {
		t.Errorf("want a row-1 correct error, got %+v", errs)
	}
}

func TestParseImportCSV_DuplicateQuestionText(t *testing.T) {
	csv := importHeader +
		"truefalse,Sky is blue,,,,,,,true,\n" +
		"truefalse,sky is BLUE ,,,,,,,false,\n"
	_, errs, err := ParseImportCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseImportCSV: %v", err)
	}
	if !hasColumnError(errs, 2, "question") {
		t.Errorf("want a row-2 question (duplicate) error, got %+v", errs)
	}
}

func TestParseImportCSV_MalformedPoints(t *testing.T) {
	csv := importHeader + "truefalse,Sky is blue,,,,,,,true,not-a-number\n"
	_, errs, err := ParseImportCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseImportCSV: %v", err)
	}
	if !hasColumnError(errs, 1, "points") {
		t.Errorf("want a row-1 points error, got %+v", errs)
	}
}

func TestParseImportCSV_RowLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString(importHeader)
	for i := 0; i < MaxImportRows+1; i++ {
		b.WriteString("truefalse,Q,,,,,,,true,\n")
	}
	rows, errs, err := ParseImportCSV(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("ParseImportCSV: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("only the first row is a non-duplicate within the 500-row window, got %d valid rows", len(rows))
	}
	foundLimitErr := false
	for _, e := range errs {
		if strings.Contains(e.Message, "row limit") {
			foundLimitErr = true
		}
	}
	if !foundLimitErr {
		t.Errorf("want a row-limit error, got %+v", errs)
	}
}

func TestParseImportCSV_MissingColumn(t *testing.T) {
	csv := "type,question,correct\nsingle,x,a\n"
	_, _, err := ParseImportCSV(strings.NewReader(csv))
	if err == nil {
		t.Fatal("want an error for a missing required column")
	}
}

func hasColumnError(errs []ImportRowError, row int, column string) bool {
	for _, e := range errs {
		if e.Row == row && e.Column == column {
			return true
		}
	}
	return false
}

// topicHeader puts the optional topic column FIRST, not appended last, so a
// parser that assumed a fixed column order rather than reading the header map
// would mis-file the cell.
const topicHeader = "topic,type,question,option_a,option_b,option_c,option_d,option_e,option_f,correct,points\n"

func TestParseImportCSV_TopicColumn(t *testing.T) {
	csv := topicHeader +
		"Data privacy,single,Pick red,Red,Blue,,,,,a,1\n" +
		"   ,truefalse,Sky is blue,,,,,,,true,\n" +
		strings.Repeat("x", 61) + ",short,Capital of France,,,,,,,Paris,1\n"

	rows, errs, err := ParseImportCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ParseImportCSV: %v", err)
	}
	if len(errs) != 1 || errs[0].Row != 3 || errs[0].Column != "topic" {
		t.Fatalf("want one row-3 topic error, got %+v", errs)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 parsed rows, got %d", len(rows))
	}
	if rows[0].Input.topic == nil || *rows[0].Input.topic != "Data privacy" {
		t.Errorf("row 1 topic = %v, want Data privacy", rows[0].Input.topic)
	}
	// A whitespace-only cell is untagged, not a topic named " ".
	if rows[1].Input.topic != nil {
		t.Errorf("row 2 topic = %v, want nil", *rows[1].Input.topic)
	}
}

func TestParseImportCSV_TopicColumnIsOptional(t *testing.T) {
	// The original fixed template has no topic column. get() resolves a missing
	// column to index 0 - "type" - so an unguarded read would tag every
	// question with its own type.
	csv := importHeader + "single,Pick red,Red,Blue,,,,,a,1\n"
	rows, errs, err := ParseImportCSV(strings.NewReader(csv))
	if err != nil || len(errs) != 0 || len(rows) != 1 {
		t.Fatalf("ParseImportCSV = (%d rows, %+v, %v), want one clean row", len(rows), errs, err)
	}
	if rows[0].Input.topic != nil {
		t.Errorf("topic = %q, want nil for a file with no topic column", *rows[0].Input.topic)
	}
}
