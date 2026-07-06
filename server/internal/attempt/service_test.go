package attempt

import (
	"encoding/json"
	"testing"
)

func TestDecodeSnapshotKeepsTheKeyInternal(t *testing.T) {
	raw := []byte(`[
	  {"id":"q1","position":1,"type":"single","body":{"text":"?"},
	   "options":[{"key":"a","text":"A"}],"correct":"a","points":2},
	  {"id":"q2","position":2,"type":"short","body":{"text":"?"},
	   "options":null,"correct":{"accepted":["x"]},"points":1}
	]`)
	questions, err := decodeSnapshot(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(questions) != 2 {
		t.Fatalf("questions = %d, want 2", len(questions))
	}
	if string(questions[0].Correct) != `"a"` {
		t.Fatalf("internal answer key lost: %q", questions[0].Correct)
	}
	if questions[1].Options != nil {
		t.Fatalf("null options should decode to nil, got %q", questions[1].Options)
	}

	// The serializer boundary: a marshaled player question carries no key.
	out, err := json.Marshal(questions[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var onWire map[string]any
	if err := json.Unmarshal(out, &onWire); err != nil {
		t.Fatalf("unmarshal wire form: %v", err)
	}
	if _, leaked := onWire["correct"]; leaked {
		t.Fatalf("serialized question leaks the answer key: %s", out)
	}
	if _, present := onWire["options"]; !present {
		t.Fatalf("choice question lost its options: %s", out)
	}
}

func TestShuffleForAttemptIsDeterministicPerAttempt(t *testing.T) {
	build := func() []Question {
		qs := make([]Question, 8)
		for i := range qs {
			qs[i] = Question{ID: string(rune('a' + i)), Position: i + 1}
		}
		return qs
	}

	first, again := build(), build()
	shuffleForAttempt(first, "attempt-1")
	shuffleForAttempt(again, "attempt-1")
	for i := range first {
		if first[i].ID != again[i].ID {
			t.Fatalf("same attempt shuffled differently at %d: %s vs %s", i, first[i].ID, again[i].ID)
		}
		if first[i].Position != i+1 {
			t.Fatalf("positions not re-densified: %v", first[i])
		}
	}

	other := build()
	shuffleForAttempt(other, "attempt-2")
	same := true
	for i := range first {
		if first[i].ID != other[i].ID {
			same = false
			break
		}
	}
	if same {
		t.Fatal("two attempts got the identical order; the shuffle key ignores the attempt id")
	}
}
