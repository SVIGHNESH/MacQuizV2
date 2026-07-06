package attempt

import (
	"encoding/json"
	"testing"
)

// TestGradeQuestion pins the docs/04 grading rules per question type - key
// comparison for choices (all-or-nothing set equality for multi), boolean
// comparison for truefalse, normalized exact/numeric matching for short - and
// that malformed responses grade as wrong rather than erroring.
func TestGradeQuestion(t *testing.T) {
	q := func(typ, correct string, points float64) Question {
		return Question{ID: "q", Type: typ, Correct: json.RawMessage(correct), Points: points}
	}
	cases := []struct {
		name     string
		question Question
		response string
		correct  bool
	}{
		{"single right key", q("single", `"b"`, 2), `"b"`, true},
		{"single wrong key", q("single", `"b"`, 2), `"a"`, false},
		{"single garbage response", q("single", `"b"`, 2), `{"x":1}`, false},

		{"multi exact set any order", q("multi", `["a","c"]`, 3), `["c","a"]`, true},
		{"multi subset earns nothing", q("multi", `["a","c"]`, 3), `["a"]`, false},
		{"multi superset earns nothing", q("multi", `["a","c"]`, 3), `["a","c","d"]`, false},
		{"multi duplicate keys collapse", q("multi", `["a","c"]`, 3), `["a","a","c"]`, true},
		{"multi garbage response", q("multi", `["a","c"]`, 3), `"a"`, false},

		{"truefalse match", q("truefalse", `true`, 1), `true`, true},
		{"truefalse mismatch", q("truefalse", `true`, 1), `false`, false},
		{"truefalse garbage response", q("truefalse", `true`, 1), `"yes"`, false},

		{"short exact", q("short", `{"accepted":["Paris"]}`, 1), `"Paris"`, true},
		{"short case and whitespace normalize", q("short", `{"accepted":["New   Delhi"]}`, 1), `"  new delhi "`, true},
		{"short second accepted answer", q("short", `{"accepted":["four","4"]}`, 1), `"FOUR"`, true},
		{"short numeric equivalence", q("short", `{"accepted":["5"]}`, 1), `"5.0"`, true},
		{"short bare number response", q("short", `{"accepted":["5"]}`, 1), `5.0`, true},
		{"short numeric mismatch", q("short", `{"accepted":["5"]}`, 1), `"5.01"`, false},
		{"short non-answer", q("short", `{"accepted":["Paris"]}`, 1), `"London"`, false},
		{"short garbage response", q("short", `{"accepted":["Paris"]}`, 1), `["Paris"]`, false},

		{"unknown type earns nothing", q("essay", `"x"`, 1), `"x"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			correct, awarded := gradeQuestion(tc.question, json.RawMessage(tc.response))
			if correct != tc.correct {
				t.Fatalf("correct = %v, want %v", correct, tc.correct)
			}
			wantPoints := 0.0
			if tc.correct {
				wantPoints = tc.question.Points
			}
			if awarded != wantPoints {
				t.Fatalf("awarded = %v, want %v", awarded, wantPoints)
			}
		})
	}
}
