package quiz

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The Milestone 2 serializer invariant (docs/12): questions.correct never
// reaches a student client. These tests pin it from day one, before any
// student can even see a quiz.

func sampleQuestion() Question {
	return Question{
		ID: "q1", QuizID: "z1", Position: 1, Type: "single",
		Body:    json.RawMessage(`{"text":"Pick one"}`),
		Options: json.RawMessage(`[{"key":"a","text":"Red"},{"key":"b","text":"Blue"}]`),
		Correct: json.RawMessage(`"a"`),
		Points:  2, Source: "manual",
	}
}

// TestQuestionNeverSerializesCorrect proves the structural guarantee: the
// base Question type cannot leak the answer key even if a handler marshals
// it directly, because Correct is tagged `json:"-"`.
func TestQuestionNeverSerializesCorrect(t *testing.T) {
	out, err := json.Marshal(sampleQuestion())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(out, []byte("correct")) {
		t.Fatalf("default Question serialization leaks the answer key: %s", out)
	}
}

// TestStudentQuestionsStripCorrect checks the student view drops the key
// from the value itself, not just from the JSON tags.
func TestStudentQuestionsStripCorrect(t *testing.T) {
	views := StudentQuestions([]Question{sampleQuestion()})
	if views[0].Correct != nil {
		t.Fatal("StudentQuestions kept the answer key in the value")
	}
	out, err := json.Marshal(views)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(out, []byte("correct")) {
		t.Fatalf("student serialization contains the answer key: %s", out)
	}
	if !bytes.Contains(out, []byte(`"options"`)) || !bytes.Contains(out, []byte(`"points"`)) {
		t.Fatalf("student serialization is missing expected fields: %s", out)
	}
}

// TestTeacherViewCarriesCorrect checks the owner-facing view is the one and
// only serialization that includes the answer key.
func TestTeacherViewCarriesCorrect(t *testing.T) {
	out, err := json.Marshal(TeacherView(sampleQuestion()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["correct"] != "a" {
		t.Fatalf("teacher view correct = %v, want \"a\"", decoded["correct"])
	}
}
