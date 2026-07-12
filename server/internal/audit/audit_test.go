package audit

import (
	"encoding/json"
	"testing"
	"time"
)

// The diff helpers are the whole of docs/08 section 7's "changes" convention:
// every module writes its before/after through them, so their equality rules -
// what counts as a change and what is merely a different spelling of the same
// value - are what the audit log means by "diff".
func TestDiffRecordsOnlyWhatMoved(t *testing.T) {
	changes := map[string]Change{}
	Diff(changes, "title", "Old", "New")
	Diff(changes, "max_attempts", 2, 2)

	if len(changes) != 1 {
		t.Fatalf("changes = %v, want only the field that moved", changes)
	}
	if got := changes["title"]; got.From != "Old" || got.To != "New" {
		t.Fatalf("title change = %+v, want Old -> New", got)
	}
}

func TestDiffPointerTreatsNilAsAValue(t *testing.T) {
	two, three := 2.0, 3.0
	changes := map[string]Change{}
	DiffPointer(changes, "points", &two, &three)      // set -> set
	DiffPointer(changes, "penalty", nil, &two)        // unset -> set
	DiffPointer(changes, "topic", &three, nil)        // set -> unset
	DiffPointer(changes, "untouched", &two, &two)     // same value
	DiffPointer[float64](changes, "absent", nil, nil) // never set

	if len(changes) != 3 {
		t.Fatalf("changes = %v, want points, penalty, and topic only", changes)
	}
	if got := changes["points"]; got.From != 2.0 || got.To != 3.0 {
		t.Fatalf("points change = %+v, want 2 -> 3 (dereferenced, not addresses)", got)
	}
	if got := changes["penalty"]; got.From != nil {
		t.Fatalf("penalty from = %v, want nil (the field was unset before)", got.From)
	}
	if got := changes["topic"]; got.To != nil {
		t.Fatalf("topic to = %v, want nil (the field was cleared)", got.To)
	}
}

// The stored jsonb comes back from Postgres normalized while the incoming
// patch is raw client JSON, so a byte-wise compare would call every autosave a
// change to the answer key.
func TestDiffJSONComparesMeaningNotBytes(t *testing.T) {
	changes := map[string]Change{}
	DiffJSON(changes, "body",
		json.RawMessage(`{"text": "Q1", "image_ref": ""}`),
		json.RawMessage(`{"image_ref":"","text":"Q1"}`))
	DiffJSON(changes, "options", nil, nil)
	DiffJSON(changes, "correct", json.RawMessage(`[0]`), json.RawMessage(`[1]`))

	if len(changes) != 1 {
		t.Fatalf("changes = %v, want only the answer key", changes)
	}
	if got := string(changes["correct"].From.(json.RawMessage)); got != "[0]" {
		t.Fatalf("correct from = %s, want [0]", got)
	}
}

func TestDiffTimeComparesInstantsNotLocations(t *testing.T) {
	utc := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	sameMoment := utc.In(time.FixedZone("IST", 5*3600+1800))
	later := utc.Add(time.Hour)

	changes := map[string]Change{}
	DiffTime(changes, "starts_at", &utc, &sameMoment)
	DiffTime(changes, "ends_at", &utc, &later)

	if len(changes) != 1 {
		t.Fatalf("changes = %v, want only ends_at; the same instant in another zone is not a change", changes)
	}
	// Recorded in UTC, so two rows written from different zones read alike.
	to, ok := changes["ends_at"].To.(time.Time)
	if !ok || to.Location() != time.UTC {
		t.Fatalf("ends_at to = %v, want a UTC time", changes["ends_at"].To)
	}
}

func TestChangeMarshalsAsFromTo(t *testing.T) {
	b, err := json.Marshal(map[string]Change{"status": {From: "closed", To: "archived"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"status":{"from":"closed","to":"archived"}}`; string(b) != want {
		t.Fatalf("detail = %s, want %s", b, want)
	}
}
