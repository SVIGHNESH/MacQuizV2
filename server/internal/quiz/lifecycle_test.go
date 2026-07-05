package quiz

import (
	"testing"
	"time"
)

// TestEffectiveStatus pins the lazy state derivation from docs/06 section 1:
// readers see a scheduled quiz as live once starts_at passes and as closed
// once ends_at passes, without waiting for the scheduler job to flip the row.
func TestEffectiveStatus(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	later := now.Add(2 * time.Hour)

	cases := []struct {
		name     string
		status   string
		startsAt *time.Time
		endsAt   *time.Time
		want     string
	}{
		{"draft stays draft", "draft", nil, nil, "draft"},
		{"scheduled before window", "scheduled", &future, &later, "scheduled"},
		{"scheduled inside window reads live", "scheduled", &past, &future, "live"},
		{"scheduled at exact starts_at reads live", "scheduled", &now, &future, "live"},
		{"scheduled past window reads closed", "scheduled", &past, &past, "closed"},
		{"live inside window stays live", "live", &past, &future, "live"},
		{"live past ends_at reads closed", "live", &past, &past, "closed"},
		{"closed stays closed", "closed", &past, &past, "closed"},
		{"archived stays archived", "archived", &past, &past, "archived"},
	}
	for _, tc := range cases {
		if got := effectiveStatus(tc.status, tc.startsAt, tc.endsAt, now); got != tc.want {
			t.Errorf("%s: effectiveStatus(%s) = %s, want %s", tc.name, tc.status, got, tc.want)
		}
	}
}

// TestGuardrailsValidate pins the docs/06 section 3 config vocabulary.
func TestGuardrailsValidate(t *testing.T) {
	if fields := DefaultGuardrails().Validate(); len(fields) != 0 {
		t.Fatalf("default guardrails invalid: %v", fields)
	}
	good := Guardrails{
		Fullscreen:      "count",
		FocusTracking:   "warn",
		BlockClipboard:  true,
		MaxViolations:   3,
		ViolationAction: "auto_submit",
	}
	if fields := good.Validate(); len(fields) != 0 {
		t.Fatalf("valid guardrails rejected: %v", fields)
	}

	bad := Guardrails{
		Fullscreen:      "always",
		FocusTracking:   "sometimes",
		MaxViolations:   0,
		ViolationAction: "expel",
	}
	fields := bad.Validate()
	for _, field := range []string{
		"guardrails.fullscreen",
		"guardrails.focus_tracking",
		"guardrails.max_violations",
		"guardrails.violation_action",
	} {
		if fields[field] == "" {
			t.Errorf("expected a message for %s, got %v", field, fields)
		}
	}
}
