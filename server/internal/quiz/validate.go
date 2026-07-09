package quiz

import (
	"encoding/json"
	"strings"
)

// QuestionInput is the wire shape of question create/update bodies. Validate
// must pass before the input reaches the database.
type QuestionInput struct {
	Type    string          `json:"type"`
	Body    json.RawMessage `json:"body"`
	Options json.RawMessage `json:"options"`
	Correct json.RawMessage `json:"correct"`
	Points  *float64        `json:"points"`
	Topic   *string         `json:"topic"`

	// points is the normalized value (default 1) set by Validate.
	points float64
	// topic is the normalized tag (trimmed, nil when blank) set by Validate.
	// A blank tag and an absent one are the same thing - untagged; storing ""
	// would make the empty string a topic the rollup could aggregate under.
	topic *string
}

// questionBody is the minimal required body shape: rich text plus an
// optional image reference (docs/03-data-model.md).
type questionBody struct {
	Text     string `json:"text"`
	ImageRef string `json:"image_ref"`
}

// option is one choice of a single/multi question: [{key, text}].
type option struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

// shortCorrect is the answer key for short-answer questions; grading
// (Milestone 4) matches responses against accepted using normalized
// exact/numeric comparison (docs/04-api.md section 4).
type shortCorrect struct {
	Accepted []string `json:"accepted"`
}

// maxPoints bounds a single question's weight; beyond this a value is
// almost certainly a typo, and scores stay readable.
const maxPoints = 1000

// maxTopicLength bounds a topic tag, matching the questions.topic CHECK. A
// topic is a label on an analytics axis, not prose: past this it stops being
// a tag several questions can share.
const maxTopicLength = 60

// Validate enforces the docs/07 authoring rules per question type: a correct
// answer must exist among the options, points > 0, choice types need 2-8
// options. It returns per-field messages for the 422 VALIDATION_FAILED
// envelope; an empty map means the input is valid and normalized.
func (in *QuestionInput) Validate() map[string]string {
	fields := map[string]string{}

	var body questionBody
	if len(in.Body) == 0 || json.Unmarshal(in.Body, &body) != nil {
		fields["body"] = "must be an object like {\"text\": \"...\"}"
	} else if strings.TrimSpace(body.Text) == "" {
		fields["body"] = "text is required"
	}

	in.topic = nil
	if in.Topic != nil {
		if trimmed := strings.TrimSpace(*in.Topic); trimmed == "" {
			in.topic = nil
		} else if len([]rune(trimmed)) > maxTopicLength {
			fields["topic"] = "must be at most 60 characters"
		} else {
			in.topic = &trimmed
		}
	}

	in.points = 1
	if in.Points != nil {
		if *in.Points <= 0 {
			fields["points"] = "must be greater than zero"
		} else if *in.Points > maxPoints {
			fields["points"] = "must be at most 1000"
		} else {
			in.points = *in.Points
		}
	}

	switch in.Type {
	case "single", "multi":
		keys := validateOptions(in.Options, fields)
		if _, ok := fields["options"]; !ok {
			validateChoiceCorrect(in.Type, in.Correct, keys, fields)
		}
	case "truefalse":
		if len(in.Options) != 0 && string(in.Options) != "null" {
			fields["options"] = "true/false questions take no options"
		}
		var v bool
		if len(in.Correct) == 0 || json.Unmarshal(in.Correct, &v) != nil {
			fields["correct"] = "must be true or false"
		}
	case "short":
		if len(in.Options) != 0 && string(in.Options) != "null" {
			fields["options"] = "short-answer questions take no options"
		}
		var sc shortCorrect
		if len(in.Correct) == 0 || json.Unmarshal(in.Correct, &sc) != nil || len(sc.Accepted) == 0 {
			fields["correct"] = "must be {\"accepted\": [\"...\"]} with at least one answer"
		} else {
			for _, a := range sc.Accepted {
				if strings.TrimSpace(a) == "" {
					fields["correct"] = "accepted answers must not be empty"
					break
				}
			}
		}
	default:
		fields["type"] = "must be single, multi, truefalse, or short"
	}
	return fields
}

// validateOptions checks the 2-8 option rule for choice questions and
// returns the set of option keys for the correct-answer check.
func validateOptions(raw json.RawMessage, fields map[string]string) map[string]bool {
	var opts []option
	if len(raw) == 0 || json.Unmarshal(raw, &opts) != nil {
		fields["options"] = "must be an array like [{\"key\": \"a\", \"text\": \"...\"}]"
		return nil
	}
	if len(opts) < 2 || len(opts) > 8 {
		fields["options"] = "choice questions need between 2 and 8 options"
		return nil
	}
	keys := map[string]bool{}
	for _, o := range opts {
		if strings.TrimSpace(o.Key) == "" || strings.TrimSpace(o.Text) == "" {
			fields["options"] = "every option needs a key and a text"
			return nil
		}
		if keys[o.Key] {
			fields["options"] = "option keys must be unique"
			return nil
		}
		keys[o.Key] = true
	}
	return keys
}

// validateChoiceCorrect enforces "a correct answer must exist among the
// options": a single option key for type single, one or more distinct keys
// for type multi.
func validateChoiceCorrect(typ string, raw json.RawMessage, keys map[string]bool, fields map[string]string) {
	switch typ {
	case "single":
		var key string
		if len(raw) == 0 || json.Unmarshal(raw, &key) != nil || !keys[key] {
			fields["correct"] = "must be the key of one of the options"
		}
	case "multi":
		var correct []string
		if len(raw) == 0 || json.Unmarshal(raw, &correct) != nil || len(correct) == 0 {
			fields["correct"] = "must be a non-empty array of option keys"
			return
		}
		seen := map[string]bool{}
		for _, k := range correct {
			if !keys[k] || seen[k] {
				fields["correct"] = "every entry must be a distinct option key"
				return
			}
			seen[k] = true
		}
	}
}
