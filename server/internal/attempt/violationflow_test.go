package attempt_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"macquiz/server/internal/attempt"
	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
)

// TestViolationFlowE2E pins the Milestone 6 guardrail-violation reporting brick
// (docs/06 section 3, docs/04:72): the student's own client reports a guardrail
// violation over POST /attempts/:id/events (the REST fallback for the attempt
// socket). It proves the counting rule that is the whole design:
//
//   - Only a guardrail whose snapshotted policy is "count" increments
//     violation_count (the tally the ladder and the roster badge read). A
//     "warn" report and a clipboard "on/logged" report still append and publish
//     their attempt.violation row - evidence the teacher sees on hover - but
//     leave the count untouched. A report for a guardrail switched off answers
//     409 GUARDRAIL_OFF.
//   - There is no dedup: one POST is one violation, additive monotonic evidence,
//     so two counted reports advance the count by two.
//   - Owner-only (another student is 404), student-only (a teacher is 403 via
//     requireStudent), in_progress-only (a submitted attempt answers its
//     terminal error), and a bad type is 422.
//   - Persist first / publish second: the count and the event row commit in one
//     transaction, then the same delta relays to Redis exactly once.
//   - The live roster's violation_count reflects the counted tally.
//
// The quiz under test is published with fullscreen=count, focus_tracking=warn,
// block_clipboard=false so one attempt exercises all three policy branches.
func TestViolationFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_violationtest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	pub := &capturePublisher{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:      sqlDB,
		Auth:    authusers.NewHandler(authSvc, false),
		Quiz:    quiz.NewHandler(quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: t.TempDir()}), authSvc),
		Attempt: attempt.NewHandler(attempt.NewService(sqlDB, log, pub), authSvc),
	})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	provision(t, ctx, sqlDB, "teacher", "owner@school.test")
	provision(t, ctx, sqlDB, "student", "reporter@school.test")
	provision(t, ctx, sqlDB, "student", "bystander@school.test")
	provision(t, ctx, sqlDB, "student", "submitter@school.test")

	teacher := login(t, server, "owner@school.test", "account-password")
	reporter := login(t, server, "reporter@school.test", "account-password")
	bystander := login(t, server, "bystander@school.test", "account-password")
	submitter := login(t, server, "submitter@school.test", "account-password")
	reporterID := userID(t, ctx, sqlDB, "reporter@school.test")
	submitterID := userID(t, ctx, sqlDB, "submitter@school.test")

	// A one-question quiz, guardrails spanning count/warn/off, all three assigned.
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Guardrails Under Test"}, teacher)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	quizID := body["quiz"].(map[string]any)["id"].(string)
	status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", map[string]any{
		"type": "single", "body": map[string]string{"text": "1 + 1 = ?"},
		"options": []map[string]string{{"key": "a", "text": "2"}, {"key": "b", "text": "3"}},
		"correct": "a", "points": 1,
	}, teacher)
	if status != 201 {
		t.Fatalf("add question = %d %v", status, body)
	}
	q1 := body["question"].(map[string]any)["id"].(string)
	if status, _, _ = itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{reporterID, submitterID}}, teacher); status != 200 {
		t.Fatalf("assign = %d", status)
	}
	if status, b, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
		"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"duration_sec": 120,
		"guardrails": map[string]any{
			"fullscreen": "count", "focus_tracking": "warn", "block_clipboard": false,
			"max_violations": 3, "violation_action": "flag",
		},
	}, teacher); status != 200 {
		t.Fatalf("publish = %d %v", status, b)
	}
	// Backdate starts_at so the quiz reads live lazily.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
		t.Fatalf("backdate starts_at: %v", err)
	}

	reporterAttempt := start(t, server, quizID, reporter)

	report := func(attemptID string, req map[string]any, cookies map[string]string) (int, map[string]any) {
		t.Helper()
		s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+attemptID+"/events", req, cookies)
		return s, b
	}

	t.Run("a report is refused without a valid type, the student role, ownership, or a live attempt", func(t *testing.T) {
		// A bad type is a 422, not a recorded event.
		if s, b := report(reporterAttempt, map[string]any{"type": "telepathy"}, reporter); s != 422 {
			t.Fatalf("bad type = %d %v, want 422", s, b)
		}
		// A teacher is not a player: requireStudent -> 403.
		if s, _ := report(reporterAttempt, map[string]any{"type": "fullscreen"}, teacher); s != 403 {
			t.Fatalf("teacher report = %d, want 403", s)
		}
		// Another student may not learn the attempt exists (404, not 403).
		if s, b := report(reporterAttempt, map[string]any{"type": "fullscreen"}, bystander); s != 404 {
			t.Fatalf("non-owner report = %d %v, want 404", s, b)
		}
		// An unknown attempt is a leak-free 404 for the owner too.
		if s, _ := report("00000000-0000-0000-0000-000000000000", map[string]any{"type": "fullscreen"}, reporter); s != 404 {
			t.Fatalf("unknown attempt report = %d, want 404", s)
		}
		// None of the refusals touched the count or wrote an event.
		if c := violationCount(t, ctx, sqlDB, reporterAttempt); c != 0 {
			t.Fatalf("violation_count after refused reports = %d, want 0", c)
		}
		if v := filter(events(t, ctx, sqlDB, reporterAttempt), "attempt.violation"); len(v) != 0 {
			t.Fatalf("refused reports wrote %d events, want 0", len(v))
		}
	})

	t.Run("a count-policy guardrail increments the tally, once per report (no dedup)", func(t *testing.T) {
		s, b := report(reporterAttempt, map[string]any{"type": "fullscreen"}, reporter)
		if s != 200 || b["counted"] != true {
			t.Fatalf("first fullscreen report = %d counted=%v, want 200 true", s, b["counted"])
		}
		if got := b["attempt"].(map[string]any)["violation_count"]; got != float64(1) {
			t.Fatalf("violation_count in response = %v, want 1", got)
		}
		// No dedup: a second identical report counts again.
		if s, b := report(reporterAttempt, map[string]any{"type": "fullscreen"}, reporter); s != 200 || b["attempt"].(map[string]any)["violation_count"] != float64(2) {
			t.Fatalf("second fullscreen report = %d count=%v, want 200 count 2", s, b["attempt"].(map[string]any)["violation_count"])
		}
		if c := violationCount(t, ctx, sqlDB, reporterAttempt); c != 2 {
			t.Fatalf("violation_count column = %d, want 2", c)
		}
		v := filter(events(t, ctx, sqlDB, reporterAttempt), "attempt.violation")
		if len(v) != 2 || v[1].payload["type"] != "fullscreen" || v[1].payload["violation_count"] != float64(2) {
			t.Fatalf("violation events = %v, want two fullscreen ending at count 2", v)
		}
		pv := filterCaptured(pub.forAttempt(reporterAttempt), "attempt.violation")
		if len(pv) != 2 || pv[1].quizID != quizID || pv[1].payload["violation_count"] != float64(2) {
			t.Fatalf("published violations = %v, want two on quiz %s ending at count 2", pv, quizID)
		}
	})

	t.Run("a warn-policy guardrail logs and publishes but does not count", func(t *testing.T) {
		dur := 40000
		s, b := report(reporterAttempt, map[string]any{"type": "focus", "duration_ms": dur}, reporter)
		if s != 200 || b["counted"] != false {
			t.Fatalf("focus report = %d counted=%v, want 200 false", s, b["counted"])
		}
		// The count is unchanged (still 2 from the two fullscreen reports).
		if got := b["attempt"].(map[string]any)["violation_count"]; got != float64(2) {
			t.Fatalf("violation_count after warn report = %v, want 2 (unchanged)", got)
		}
		if c := violationCount(t, ctx, sqlDB, reporterAttempt); c != 2 {
			t.Fatalf("violation_count column after warn report = %d, want 2", c)
		}
		// The warn report is still recorded as evidence, carrying its type,
		// duration, and the (unchanged) count.
		v := filter(events(t, ctx, sqlDB, reporterAttempt), "attempt.violation")
		last := v[len(v)-1]
		if last.payload["type"] != "focus" || last.payload["duration_ms"] != float64(dur) || last.payload["violation_count"] != float64(2) {
			t.Fatalf("warn event = %v, want focus/40000/count 2", last.payload)
		}
		if pv := filterCaptured(pub.forAttempt(reporterAttempt), "attempt.violation"); len(pv) != 3 {
			t.Fatalf("published violations after warn = %d, want 3", len(pv))
		}
	})

	t.Run("a report for a guardrail switched off answers 409 GUARDRAIL_OFF", func(t *testing.T) {
		// block_clipboard is false in this quiz's snapshot.
		s, b := report(reporterAttempt, map[string]any{"type": "clipboard"}, reporter)
		if s != 409 || b["code"] != "GUARDRAIL_OFF" {
			t.Fatalf("clipboard report = %d %v, want 409 GUARDRAIL_OFF", s, b)
		}
		// The off report neither counted nor wrote an event.
		if c := violationCount(t, ctx, sqlDB, reporterAttempt); c != 2 {
			t.Fatalf("violation_count after off report = %d, want 2 (untouched)", c)
		}
		if v := filter(events(t, ctx, sqlDB, reporterAttempt), "attempt.violation"); len(v) != 3 {
			t.Fatalf("off report wrote an event: now %d, want 3", len(v))
		}
	})

	t.Run("the live roster reflects the counted violation tally", func(t *testing.T) {
		row := rosterRow(t, server, quizID, teacher, reporterID)
		if row["violation_count"] != float64(2) {
			t.Fatalf("roster violation_count = %v, want 2", row["violation_count"])
		}
	})

	t.Run("a terminated attempt accrues no violations", func(t *testing.T) {
		submitterAttempt := start(t, server, quizID, submitter)
		save(t, server, submitterAttempt, q1, "a", submitter)
		if s, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+submitterAttempt+"/submit", nil, submitter); s != 200 {
			t.Fatalf("submit = %d, want 200", s)
		}
		if s, b := report(submitterAttempt, map[string]any{"type": "fullscreen"}, submitter); s != 409 || b["code"] != "ATTEMPT_ALREADY_SUBMITTED" {
			t.Fatalf("report on submitted attempt = %d %v, want 409 ATTEMPT_ALREADY_SUBMITTED", s, b)
		}
		if c := violationCount(t, ctx, sqlDB, submitterAttempt); c != 0 {
			t.Fatalf("submitted attempt violation_count = %d, want 0", c)
		}
		// A kicked attempt is the other terminal branch: the reporter is kicked
		// (its roster/tally assertions already ran), so its next report answers
		// the kicked terminal error rather than accruing a violation.
		if s, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+reporterAttempt+"/kick",
			map[string]any{"reason": "eyes elsewhere"}, teacher); s != 200 {
			t.Fatalf("kick reporter = %d, want 200", s)
		}
		if s, b := report(reporterAttempt, map[string]any{"type": "fullscreen"}, reporter); s != 409 || b["code"] != "ATTEMPT_KICKED" {
			t.Fatalf("report on kicked attempt = %d %v, want 409 ATTEMPT_KICKED", s, b)
		}
	})

	t.Run("the flag ladder notifies nobody: the tally is the whole record", func(t *testing.T) {
		if n := pub.notifyCount(); n != 0 {
			t.Fatalf("flag ladder sent %d notifications, want 0", n)
		}
	})
}

// TestViolationAutoSubmitLadderE2E pins the Milestone 6 violation-ladder
// terminal action (docs/06 section 3: "After max_violations counted violations,
// the configured action fires ... flag / auto_submit / notify"). It proves the
// one active action, auto_submit:
//
//   - Counted violations below the snapshotted max_violations only tally; the
//     attempt stays in_progress.
//   - The counted report that reaches max_violations auto-submits the attempt in
//     the SAME transaction as the violation - status flips to 'submitted' with
//     submit_kind='auto' (it rides the shared submit funnel, so it terminates
//     and grades exactly like a deadline expiry), appends the attempt.submitted
//     event AFTER the attempt.violation that triggered it, publishes both deltas
//     in that causal order, and enqueues the grade job (the trap: submit() never
//     enqueues grading - the caller must).
//   - The ladder fires exactly once: the auto-submitted attempt is terminal, so
//     a further report answers 409 ATTEMPT_ALREADY_SUBMITTED.
//   - The auto-submitted work grades to 'graded' with submit_kind='auto' kept,
//     so the results read (which gates on status='graded') is reachable.
//
// flag (the default) is covered by TestViolationFlowE2E, whose count reaches 2
// under max_violations=3 and stays live; only auto_submit is exercised here.
func TestViolationAutoSubmitLadderE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_laddertest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	pub := &capturePublisher{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:      sqlDB,
		Auth:    authusers.NewHandler(authSvc, false),
		Quiz:    quiz.NewHandler(quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: t.TempDir()}), authSvc),
		Attempt: attempt.NewHandler(attempt.NewService(sqlDB, log, pub), authSvc),
	})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	provision(t, ctx, sqlDB, "teacher", "ladderowner@school.test")
	provision(t, ctx, sqlDB, "student", "ladder@school.test")
	teacher := login(t, server, "ladderowner@school.test", "account-password")
	student := login(t, server, "ladder@school.test", "account-password")
	studentID := userID(t, ctx, sqlDB, "ladder@school.test")

	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Auto-Submit Ladder"}, teacher)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	quizID := body["quiz"].(map[string]any)["id"].(string)
	if status, b, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", map[string]any{
		"type": "single", "body": map[string]string{"text": "1 + 1 = ?"},
		"options": []map[string]string{{"key": "a", "text": "2"}, {"key": "b", "text": "3"}},
		"correct": "a", "points": 1,
	}, teacher); status != 201 {
		t.Fatalf("add question = %d %v", status, b)
	}
	if status, _, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{studentID}}, teacher); status != 200 {
		t.Fatalf("assign = %d", status)
	}
	// fullscreen=count with a low ladder (max 2, auto_submit) so the second
	// counted report trips the terminal action.
	if status, b, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
		"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"duration_sec": 120,
		"guardrails": map[string]any{
			"fullscreen": "count", "focus_tracking": "off", "block_clipboard": false,
			"max_violations": 2, "violation_action": "auto_submit",
		},
	}, teacher); status != 200 {
		t.Fatalf("publish = %d %v", status, b)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
		t.Fatalf("backdate starts_at: %v", err)
	}

	attemptID := start(t, server, quizID, student)
	report := func(req map[string]any) (int, map[string]any) {
		t.Helper()
		s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+attemptID+"/events", req, student)
		return s, b
	}
	gradeJobs := func() int {
		t.Helper()
		var jobs int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM river_job
			 WHERE kind = 'grade_attempt' AND args->>'attempt_id' = $1`, attemptID).Scan(&jobs); err != nil {
			t.Fatalf("count grade jobs: %v", err)
		}
		return jobs
	}

	t.Run("a counted report below the threshold only tallies; the attempt stays live", func(t *testing.T) {
		s, b := report(map[string]any{"type": "fullscreen"})
		att := b["attempt"].(map[string]any)
		if s != 200 || b["counted"] != true || att["violation_count"] != float64(1) || att["status"] != "in_progress" {
			t.Fatalf("first report = %d counted=%v count=%v status=%v, want 200/true/1/in_progress", s, b["counted"], att["violation_count"], att["status"])
		}
		if sub := filter(events(t, ctx, sqlDB, attemptID), "attempt.submitted"); len(sub) != 0 {
			t.Fatalf("attempt submitted below threshold: %v", sub)
		}
		if n := gradeJobs(); n != 0 {
			t.Fatalf("grade job enqueued below threshold: %d", n)
		}
	})

	t.Run("the report that reaches max_violations auto-submits, enqueues grading, and orders the log", func(t *testing.T) {
		s, b := report(map[string]any{"type": "fullscreen"})
		att := b["attempt"].(map[string]any)
		// Counted to 2, and the same request flipped the attempt via submit(kind='auto').
		if s != 200 || b["counted"] != true || att["violation_count"] != float64(2) {
			t.Fatalf("threshold report = %d counted=%v count=%v, want 200/true/2", s, b["counted"], att["violation_count"])
		}
		if att["status"] != "submitted" || att["submit_kind"] != "auto" {
			t.Fatalf("threshold report attempt = status %v submit_kind %v, want submitted/auto", att["status"], att["submit_kind"])
		}
		// The append-only log reads in causal order: the triggering violation,
		// then exactly one auto-submit it caused.
		evs := events(t, ctx, sqlDB, attemptID)
		last, prev := evs[len(evs)-1], evs[len(evs)-2]
		if prev.typ != "attempt.violation" || prev.payload["violation_count"] != float64(2) {
			t.Fatalf("second-to-last event = %v, want attempt.violation at count 2", prev)
		}
		if last.typ != "attempt.submitted" || last.payload["submit_kind"] != "auto" {
			t.Fatalf("last event = %v, want attempt.submitted kind auto", last)
		}
		if sub := filter(evs, "attempt.submitted"); len(sub) != 1 {
			t.Fatalf("auto-submit wrote %d submitted events, want 1", len(sub))
		}
		// Both deltas relayed, submitted after violation.
		ps := filterCaptured(pub.forAttempt(attemptID), "attempt.submitted")
		if len(ps) != 1 || ps[0].payload["submit_kind"] != "auto" {
			t.Fatalf("published submitted = %v, want one kind auto", ps)
		}
		// The trap: the auto-submit must enqueue grading, or the attempt never grades.
		if n := gradeJobs(); n != 1 {
			t.Fatalf("auto-submit enqueued %d grade jobs, want 1", n)
		}
	})

	t.Run("the ladder fires exactly once: a further report hits the terminal state", func(t *testing.T) {
		if s, b := report(map[string]any{"type": "fullscreen"}); s != 409 || b["code"] != "ATTEMPT_ALREADY_SUBMITTED" {
			t.Fatalf("report after auto-submit = %d %v, want 409 ATTEMPT_ALREADY_SUBMITTED", s, b)
		}
		if c := violationCount(t, ctx, sqlDB, attemptID); c != 2 {
			t.Fatalf("violation_count after terminal report = %d, want 2 (untouched)", c)
		}
	})

	t.Run("the auto-submitted work grades to graded with submit_kind auto kept", func(t *testing.T) {
		if graded, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil || graded != 1 {
			t.Fatalf("grade = %d (err %v), want 1", graded, err)
		}
		var st, kind string
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT status, submit_kind FROM attempts WHERE id = $1`, attemptID).Scan(&st, &kind); err != nil {
			t.Fatalf("read graded attempt: %v", err)
		}
		if st != "graded" || kind != "auto" {
			t.Fatalf("graded auto-submit = status %q submit_kind %q, want graded/auto", st, kind)
		}
	})

	t.Run("the auto_submit ladder never notifies: the actions are exclusive", func(t *testing.T) {
		if n := pub.notifyCount(); n != 0 {
			t.Fatalf("auto_submit ladder sent %d notifications, want 0", n)
		}
	})
}

// TestViolationNotifyLadderE2E pins the violation ladder's notify action
// (docs/06 section 3): the quiz owner is alerted on their own
// user:{id}:notify channel, and the attempt is left running.
//
//   - Counted violations below the snapshotted max_violations alert nobody.
//   - The counted report that reaches max_violations relays exactly one
//     attempt.violation_alert to the QUIZ OWNER (not the student, not the whole
//     quiz channel), naming the student, the quiz, the type, and the count.
//   - It fires again on every further counted report - the same stateless >=
//     test auto_submit uses. Nothing records that an alert was sent, so the
//     escalating count is what the teacher keeps seeing; their client keys the
//     banner by attempt_id so the repeats replace rather than stack.
//   - notify is not a termination: the attempt stays in_progress across all of
//     it, appends no attempt.submitted event, and enqueues no grade job. This is
//     the whole difference from auto_submit, and the reason the doc lists them
//     as separate rungs.
//   - An uncounted (warn-policy) report past the threshold alerts nobody: only
//     the counted tally drives the ladder.
func TestViolationNotifyLadderE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_notifyladdertest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	pub := &capturePublisher{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:      sqlDB,
		Auth:    authusers.NewHandler(authSvc, false),
		Quiz:    quiz.NewHandler(quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: t.TempDir()}), authSvc),
		Attempt: attempt.NewHandler(attempt.NewService(sqlDB, log, pub), authSvc),
	})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	provision(t, ctx, sqlDB, "teacher", "notifyowner@school.test")
	provision(t, ctx, sqlDB, "student", "notified@school.test")
	teacher := login(t, server, "notifyowner@school.test", "account-password")
	student := login(t, server, "notified@school.test", "account-password")
	ownerID := userID(t, ctx, sqlDB, "notifyowner@school.test")
	studentID := userID(t, ctx, sqlDB, "notified@school.test")

	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Notify Ladder"}, teacher)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	quizID := body["quiz"].(map[string]any)["id"].(string)
	if status, b, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", map[string]any{
		"type": "single", "body": map[string]string{"text": "1 + 1 = ?"},
		"options": []map[string]string{{"key": "a", "text": "2"}, {"key": "b", "text": "3"}},
		"correct": "a", "points": 1,
	}, teacher); status != 201 {
		t.Fatalf("add question = %d %v", status, b)
	}
	if status, _, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{studentID}}, teacher); status != 200 {
		t.Fatalf("assign = %d", status)
	}
	// fullscreen=count with a low ladder (max 2, notify), focus_tracking=warn so
	// one attempt can also prove an uncounted report past the threshold is silent.
	if status, b, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
		"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"duration_sec": 120,
		"guardrails": map[string]any{
			"fullscreen": "count", "focus_tracking": "warn", "block_clipboard": false,
			"max_violations": 2, "violation_action": "notify",
		},
	}, teacher); status != 200 {
		t.Fatalf("publish = %d %v", status, b)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
		t.Fatalf("backdate starts_at: %v", err)
	}

	attemptID := start(t, server, quizID, student)
	report := func(vtype string) (int, map[string]any) {
		t.Helper()
		s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+attemptID+"/events",
			map[string]any{"type": vtype}, student)
		return s, b
	}
	// gradeJobs proves notify is not a termination: auto_submit's grade job is the
	// clearest fingerprint of the funnel this rung must not enter.
	gradeJobs := func() int {
		t.Helper()
		var jobs int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM river_job
			 WHERE kind = 'grade_attempt' AND args->>'attempt_id' = $1`, attemptID).Scan(&jobs); err != nil {
			t.Fatalf("count grade jobs: %v", err)
		}
		return jobs
	}

	t.Run("a counted report below the threshold alerts nobody", func(t *testing.T) {
		s, b := report("fullscreen")
		att := b["attempt"].(map[string]any)
		if s != 200 || att["violation_count"] != float64(1) || att["status"] != "in_progress" {
			t.Fatalf("first report = %d count=%v status=%v, want 200/1/in_progress", s, att["violation_count"], att["status"])
		}
		if n := pub.notifyCount(); n != 0 {
			t.Fatalf("notifications below threshold = %d, want 0", n)
		}
	})

	t.Run("the report that reaches max_violations alerts the owner, and only the owner", func(t *testing.T) {
		s, b := report("fullscreen")
		att := b["attempt"].(map[string]any)
		if s != 200 || att["violation_count"] != float64(2) {
			t.Fatalf("threshold report = %d count=%v, want 200/2", s, att["violation_count"])
		}
		// notify is not a termination - that is the whole difference from auto_submit.
		if att["status"] != "in_progress" {
			t.Fatalf("threshold report left attempt %v, want in_progress (notify must not submit)", att["status"])
		}
		if sub := filter(events(t, ctx, sqlDB, attemptID), "attempt.submitted"); len(sub) != 0 {
			t.Fatalf("notify ladder submitted the attempt: %v", sub)
		}
		if n := gradeJobs(); n != 0 {
			t.Fatalf("notify ladder enqueued %d grade jobs, want 0", n)
		}

		notes := pub.notifies(ownerID)
		if len(notes) != 1 {
			t.Fatalf("owner notifications = %d, want exactly 1", len(notes))
		}
		got := notes[0]
		if got.typ != "attempt.violation_alert" {
			t.Fatalf("notify type = %q, want attempt.violation_alert", got.typ)
		}
		want := map[string]any{
			"quiz_id": quizID, "quiz_title": "Notify Ladder",
			// provision seeds full_name = email, so that is the student's display name.
			"attempt_id": attemptID, "student_id": studentID, "student_name": "notified@school.test",
			"violation_type": "fullscreen", "violation_count": float64(2),
		}
		for k, v := range want {
			if got.payload[k] != v {
				t.Fatalf("notify payload[%s] = %v, want %v (full payload %v)", k, got.payload[k], v, got.payload)
			}
		}
		// The alert is addressed to a person, so it must not also land on the
		// student's own channel - they already know, and correct is nearby.
		if n := pub.notifies(studentID); len(n) != 0 {
			t.Fatalf("student received %d notifications, want 0", len(n))
		}
	})

	t.Run("every further counted violation re-alerts with the escalating count", func(t *testing.T) {
		if s, b := report("fullscreen"); s != 200 || b["attempt"].(map[string]any)["violation_count"] != float64(3) {
			t.Fatalf("third report = %d %v, want 200 at count 3", s, b["attempt"])
		}
		notes := pub.notifies(ownerID)
		if len(notes) != 2 {
			t.Fatalf("owner notifications after third report = %d, want 2", len(notes))
		}
		if notes[1].payload["violation_count"] != float64(3) {
			t.Fatalf("re-alert count = %v, want 3", notes[1].payload["violation_count"])
		}
	})

	t.Run("an uncounted report past the threshold alerts nobody", func(t *testing.T) {
		// focus is warn-policy: it logs and publishes its evidence row but never
		// advances the tally, so it cannot move the ladder either.
		if s, b := report("focus"); s != 200 || b["counted"] != false {
			t.Fatalf("warn report = %d counted=%v, want 200/false", s, b["counted"])
		}
		if n := len(pub.notifies(ownerID)); n != 2 {
			t.Fatalf("owner notifications after a warn report = %d, want 2 (unchanged)", n)
		}
		if c := violationCount(t, ctx, sqlDB, attemptID); c != 3 {
			t.Fatalf("violation_count after warn report = %d, want 3 (untouched)", c)
		}
	})
}

// violationCount reads the accumulated violation_count for one attempt.
func violationCount(t *testing.T, ctx context.Context, sqlDB *sql.DB, attemptID string) int {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT violation_count FROM attempts WHERE id = $1`, attemptID).Scan(&n); err != nil {
		t.Fatalf("read violation_count: %v", err)
	}
	return n
}
