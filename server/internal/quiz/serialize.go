package quiz

import "encoding/json"

// The serializer boundary (docs/12 Milestone 2): questions.correct never
// reaches a student client. The guarantee is structural - Question tags
// Correct with `json:"-"`, so the default serialization of a question has no
// answer key at all. Handlers must opt IN to exposing it by building the
// TeacherQuestion view; there is no way to leak it by forgetting to strip.

// TeacherQuestion is the owner-facing view: the question plus its answer
// key. Only authoring endpoints (which already proved ownership through the
// policy) may serialize this type.
type TeacherQuestion struct {
	Question
	Correct json.RawMessage `json:"correct"`
}

// TeacherView exposes the answer key to the quiz owner.
func TeacherView(q Question) TeacherQuestion {
	return TeacherQuestion{Question: q, Correct: q.Correct}
}

// TeacherViews maps a question list to the owner-facing view.
func TeacherViews(qs []Question) []TeacherQuestion {
	out := make([]TeacherQuestion, len(qs))
	for i, q := range qs {
		out[i] = TeacherView(q)
	}
	return out
}

// StudentQuestions returns the question set as the attempt player may see
// it (docs/04-api.md: "returns question set WITHOUT correct"). The answer
// key is dropped from the value itself, not just hidden by a tag, so even a
// reflective serializer downstream could not recover it.
func StudentQuestions(qs []Question) []Question {
	out := make([]Question, len(qs))
	for i, q := range qs {
		q.Correct = nil
		out[i] = q
	}
	return out
}
