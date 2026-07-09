package quiz

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestValidateQuestion pins the docs/07 authoring rules: a correct answer
// must exist among the options, points > 0, choice types need 2-8 options.
func TestValidateQuestion(t *testing.T) {
	ptr := func(f float64) *float64 { return &f }
	opts := json.RawMessage(`[{"key":"a","text":"Red"},{"key":"b","text":"Blue"}]`)

	cases := []struct {
		name      string
		in        QuestionInput
		wantField string // "" means valid
	}{
		{"valid single", QuestionInput{
			Type: "single", Body: json.RawMessage(`{"text":"Pick one"}`),
			Options: opts, Correct: json.RawMessage(`"a"`)}, ""},
		{"valid multi", QuestionInput{
			Type: "multi", Body: json.RawMessage(`{"text":"Pick some"}`),
			Options: opts, Correct: json.RawMessage(`["a","b"]`)}, ""},
		{"valid truefalse", QuestionInput{
			Type: "truefalse", Body: json.RawMessage(`{"text":"Sky is blue"}`),
			Correct: json.RawMessage(`true`)}, ""},
		{"valid short", QuestionInput{
			Type: "short", Body: json.RawMessage(`{"text":"Capital of France"}`),
			Correct: json.RawMessage(`{"accepted":["Paris"]}`)}, ""},

		{"unknown type", QuestionInput{
			Type: "essay", Body: json.RawMessage(`{"text":"x"}`),
			Correct: json.RawMessage(`true`)}, "type"},
		{"empty body text", QuestionInput{
			Type: "truefalse", Body: json.RawMessage(`{"text":"  "}`),
			Correct: json.RawMessage(`true`)}, "body"},
		{"missing body", QuestionInput{
			Type: "truefalse", Correct: json.RawMessage(`true`)}, "body"},

		{"zero points", QuestionInput{
			Type: "truefalse", Body: json.RawMessage(`{"text":"x"}`),
			Correct: json.RawMessage(`true`), Points: ptr(0)}, "points"},
		{"negative points", QuestionInput{
			Type: "truefalse", Body: json.RawMessage(`{"text":"x"}`),
			Correct: json.RawMessage(`true`), Points: ptr(-2)}, "points"},
		{"absurd points", QuestionInput{
			Type: "truefalse", Body: json.RawMessage(`{"text":"x"}`),
			Correct: json.RawMessage(`true`), Points: ptr(5000)}, "points"},

		{"one option", QuestionInput{
			Type: "single", Body: json.RawMessage(`{"text":"x"}`),
			Options: json.RawMessage(`[{"key":"a","text":"only"}]`),
			Correct: json.RawMessage(`"a"`)}, "options"},
		{"nine options", QuestionInput{
			Type: "single", Body: json.RawMessage(`{"text":"x"}`),
			Options: json.RawMessage(`[{"key":"a","text":"1"},{"key":"b","text":"2"},
				{"key":"c","text":"3"},{"key":"d","text":"4"},{"key":"e","text":"5"},
				{"key":"f","text":"6"},{"key":"g","text":"7"},{"key":"h","text":"8"},
				{"key":"i","text":"9"}]`),
			Correct: json.RawMessage(`"a"`)}, "options"},
		{"duplicate option keys", QuestionInput{
			Type: "single", Body: json.RawMessage(`{"text":"x"}`),
			Options: json.RawMessage(`[{"key":"a","text":"1"},{"key":"a","text":"2"}]`),
			Correct: json.RawMessage(`"a"`)}, "options"},
		{"options on truefalse", QuestionInput{
			Type: "truefalse", Body: json.RawMessage(`{"text":"x"}`),
			Options: opts, Correct: json.RawMessage(`true`)}, "options"},

		{"correct not among options", QuestionInput{
			Type: "single", Body: json.RawMessage(`{"text":"x"}`),
			Options: opts, Correct: json.RawMessage(`"z"`)}, "correct"},
		{"multi correct with unknown key", QuestionInput{
			Type: "multi", Body: json.RawMessage(`{"text":"x"}`),
			Options: opts, Correct: json.RawMessage(`["a","z"]`)}, "correct"},
		{"multi correct empty", QuestionInput{
			Type: "multi", Body: json.RawMessage(`{"text":"x"}`),
			Options: opts, Correct: json.RawMessage(`[]`)}, "correct"},
		{"truefalse non-boolean correct", QuestionInput{
			Type: "truefalse", Body: json.RawMessage(`{"text":"x"}`),
			Correct: json.RawMessage(`"yes"`)}, "correct"},
		{"short with no accepted answers", QuestionInput{
			Type: "short", Body: json.RawMessage(`{"text":"x"}`),
			Correct: json.RawMessage(`{"accepted":[]}`)}, "correct"},
		{"short with blank accepted answer", QuestionInput{
			Type: "short", Body: json.RawMessage(`{"text":"x"}`),
			Correct: json.RawMessage(`{"accepted":["  "]}`)}, "correct"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := tc.in.Validate()
			if tc.wantField == "" {
				if len(fields) != 0 {
					t.Fatalf("Validate() = %v, want valid", fields)
				}
				return
			}
			if _, ok := fields[tc.wantField]; !ok {
				t.Fatalf("Validate() = %v, want error on field %q", fields, tc.wantField)
			}
		})
	}
}

// TestValidateDefaultsPoints checks that omitted points normalize to the
// schema default of 1.
func TestValidateDefaultsPoints(t *testing.T) {
	in := QuestionInput{
		Type: "truefalse", Body: json.RawMessage(`{"text":"x"}`),
		Correct: json.RawMessage(`true`),
	}
	if fields := in.Validate(); len(fields) != 0 {
		t.Fatalf("Validate() = %v, want valid", fields)
	}
	if in.points != 1 {
		t.Fatalf("normalized points = %v, want 1", in.points)
	}
}

// TestValidateNormalizesTopic pins the topic tag's normalization: it is
// trimmed, a blank tag means untagged rather than a topic named "", and an
// over-long tag is a field error rather than a truncation. The rollup keys
// student_stats.topic_strengths on this value, so "Data privacy " and
// "Data privacy" must not become two topics.
func TestValidateNormalizesTopic(t *testing.T) {
	topic := func(s string) *string { return &s }
	base := func() QuestionInput {
		return QuestionInput{
			Type:    "truefalse",
			Body:    json.RawMessage(`{"text":"Sky is blue"}`),
			Correct: json.RawMessage(`true`),
		}
	}

	t.Run("absent stays untagged", func(t *testing.T) {
		in := base()
		if fields := in.Validate(); len(fields) != 0 {
			t.Fatalf("Validate = %v, want valid", fields)
		}
		if in.topic != nil {
			t.Fatalf("topic = %q, want nil", *in.topic)
		}
	})

	t.Run("surrounding whitespace is trimmed", func(t *testing.T) {
		in := base()
		in.Topic = topic("  Data privacy\t")
		if fields := in.Validate(); len(fields) != 0 {
			t.Fatalf("Validate = %v, want valid", fields)
		}
		if in.topic == nil || *in.topic != "Data privacy" {
			t.Fatalf("topic = %v, want Data privacy", in.topic)
		}
	})

	t.Run("a blank tag is untagged, not an empty topic", func(t *testing.T) {
		in := base()
		in.Topic = topic("   ")
		if fields := in.Validate(); len(fields) != 0 {
			t.Fatalf("Validate = %v, want valid", fields)
		}
		if in.topic != nil {
			t.Fatalf("topic = %q, want nil", *in.topic)
		}
	})

	t.Run("60 characters pass, 61 fail", func(t *testing.T) {
		in := base()
		in.Topic = topic(strings.Repeat("x", 60))
		if fields := in.Validate(); len(fields) != 0 {
			t.Fatalf("Validate(60 chars) = %v, want valid", fields)
		}
		in = base()
		in.Topic = topic(strings.Repeat("x", 61))
		if fields := in.Validate(); fields["topic"] == "" {
			t.Fatalf("Validate(61 chars) = %v, want a topic field error", fields)
		}
	})

	t.Run("length counts runes, matching the database CHECK", func(t *testing.T) {
		// 60 multi-byte runes: 180 bytes, but a legal tag.
		in := base()
		in.Topic = topic(strings.Repeat("é", 60))
		if fields := in.Validate(); len(fields) != 0 {
			t.Fatalf("Validate = %v, want valid", fields)
		}
	})
}
