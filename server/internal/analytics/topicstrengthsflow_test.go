package analytics_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"macquiz/server/internal/analytics"
	"macquiz/server/internal/attempt"
	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
)

// TestTopicStrengthsRollupE2E pins student_stats.topic_strengths, the
// "strength/weakness by topic tag" metric docs/07 section 3 promises: the
// student's accuracy per questions.topic, upserted by the same rollup-on-close
// job that writes accuracy_trend beside it.
//
// It lives in its own test function - and so its own database - rather than as
// a subtest of TestRollupFlowE2E, because that suite pins exact org-wide
// counts (five quizzes, two teachers, four students) that any extra fixture
// would silently break.
//
// The four rules under test, each of which a plausible implementation gets
// wrong:
//   - an UNTAGGED question contributes to no topic key at all;
//   - a SKIPPED question does not count against its topic, the same denominator
//     item analysis' p-value uses - only answered questions do;
//   - a topic spanning several questions averages their correctness, so a
//     half-right topic reads 0.5 rather than 0 or 1;
//   - the tag comes from the VERSION SNAPSHOT the student sat, so retagging the
//     live question afterwards cannot rewrite an already-closed result.
func TestTopicStrengthsRollupE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_topictest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:        sqlDB,
		Auth:      authusers.NewHandler(authSvc, false),
		Quiz:      quiz.NewHandler(quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: t.TempDir()}), authSvc),
		Attempt:   attempt.NewHandler(attempt.NewService(sqlDB, log), authSvc),
		Analytics: analytics.NewHandler(analytics.NewService(sqlDB, log), authSvc),
	})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	provision(t, ctx, sqlDB, "teacher", "owner@school.test")
	for _, email := range []string{"ace@school.test", "mixed@school.test", "skipper@school.test", "noshow@school.test"} {
		provision(t, ctx, sqlDB, "student", email)
	}
	teacher := login(t, server, "owner@school.test", "account-password")
	ace := login(t, server, "ace@school.test", "account-password")
	mixed := login(t, server, "mixed@school.test", "account-password")
	skipper := login(t, server, "skipper@school.test", "account-password")

	addQuestion := func(quizID string, q map[string]any) string {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", q, teacher)
		if status != 201 {
			t.Fatalf("add question = %d %v", status, body)
		}
		return body["question"].(map[string]any)["id"].(string)
	}

	newQuiz := func(title string) string {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
			map[string]string{"title": title}, teacher)
		if status != 201 {
			t.Fatalf("create quiz = %d %v", status, body)
		}
		return body["quiz"].(map[string]any)["id"].(string)
	}

	publishLive := func(quizID string, studentEmails ...string) {
		t.Helper()
		ids := make([]string, len(studentEmails))
		for i, e := range studentEmails {
			ids[i] = userID(t, ctx, sqlDB, e)
		}
		if status, body, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": ids}, teacher); status != 200 {
			t.Fatalf("assign = %d %v", status, body)
		}
		if status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
			"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			"duration_sec": 600,
		}, teacher); status != 200 {
			t.Fatalf("publish = %d %v", status, body)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("backdate starts_at: %v", err)
		}
	}

	attemptQuiz := func(cookies map[string]string, quizID string, answers map[string]any) {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, cookies)
		if status != 200 && status != 201 {
			t.Fatalf("start = %d %v", status, body)
		}
		attemptID := body["attempt"].(map[string]any)["id"].(string)
		for qid, resp := range answers {
			if status, b, _ := itest.Call(t, server, "PUT",
				"/api/v1/attempts/"+attemptID+"/answers/"+qid,
				map[string]any{"response": resp}, cookies); status != 200 {
				t.Fatalf("autosave = %d %v", status, b)
			}
		}
		if status, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+attemptID+"/submit", nil, cookies); status != 200 {
			t.Fatalf("submit = %d %v", status, b)
		}
	}

	closeQuiz := func(quizID string) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET status = 'closed', ends_at = now() WHERE id = $1`, quizID); err != nil {
			t.Fatalf("close quiz: %v", err)
		}
	}

	readTopics := func(email string) map[string]float64 {
		t.Helper()
		var raw []byte
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT topic_strengths FROM student_stats WHERE student_id = $1`,
			userID(t, ctx, sqlDB, email)).Scan(&raw); err != nil {
			t.Fatalf("read topic_strengths for %s: %v", email, err)
		}
		var out map[string]float64
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode topic_strengths for %s: %v", email, err)
		}
		if out == nil {
			t.Fatalf("topic_strengths for %s decoded to nil - the column must hold {} not null", email)
		}
		return out
	}

	wantTopics := func(email string, want map[string]float64) {
		t.Helper()
		got := readTopics(email)
		if len(got) != len(want) {
			t.Fatalf("%s topic_strengths = %v, want %v", email, got, want)
		}
		for topic, accuracy := range want {
			if math.Abs(got[topic]-accuracy) > 1e-9 {
				t.Fatalf("%s topic_strengths = %v, want %v", email, got, want)
			}
		}
	}

	// Quiz one. "Data privacy" spans two questions so a half-right topic has a
	// value only an averaging implementation produces; the fourth question is
	// deliberately untagged.
	quizOne := newQuiz("Foundations")
	access := addQuestion(quizOne, map[string]any{
		"type": "single", "body": map[string]string{"text": "Pick b."},
		"options": []map[string]string{{"key": "a", "text": "A"}, {"key": "b", "text": "B"}},
		"correct": "b", "topic": "Access control",
	})
	privacyTF := addQuestion(quizOne, map[string]any{
		"type": "truefalse", "body": map[string]string{"text": "True?"},
		"correct": true, "topic": "Data privacy",
	})
	privacyShort := addQuestion(quizOne, map[string]any{
		"type": "short", "body": map[string]string{"text": "Say yes."},
		"correct": map[string]any{"accepted": []string{"yes"}}, "topic": "Data privacy",
	})
	// Blank topics are untagged, not a topic named "": Validate trims to nil.
	untagged := addQuestion(quizOne, map[string]any{
		"type": "single", "body": map[string]string{"text": "Also pick b."},
		"options": []map[string]string{{"key": "a", "text": "A"}, {"key": "b", "text": "B"}},
		"correct": "b", "topic": "   ",
	})
	publishLive(quizOne, "ace@school.test", "mixed@school.test", "skipper@school.test", "noshow@school.test")

	attemptQuiz(ace, quizOne, map[string]any{
		access: "b", privacyTF: true, privacyShort: "yes", untagged: "b",
	})
	// mixed is right on one Data privacy question and wrong on the other.
	attemptQuiz(mixed, quizOne, map[string]any{
		access: "b", privacyTF: false, privacyShort: "yes", untagged: "a",
	})
	// skipper answers only the Access control question. Data privacy must not
	// appear at all - a skip is not a wrong answer.
	attemptQuiz(skipper, quizOne, map[string]any{access: "b"})

	if _, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil {
		t.Fatalf("grade: %v", err)
	}
	closeQuiz(quizOne)
	if rolled, err := analytics.RollupDue(ctx, sqlDB); err != nil || rolled != 1 {
		t.Fatalf("rollup = (%d, %v), want (1, nil)", rolled, err)
	}

	t.Run("a tagged, fully answered attempt scores every topic it touched", func(t *testing.T) {
		wantTopics("ace@school.test", map[string]float64{"Access control": 1, "Data privacy": 1})
	})

	t.Run("a topic spanning two questions averages their correctness", func(t *testing.T) {
		wantTopics("mixed@school.test", map[string]float64{"Access control": 1, "Data privacy": 0.5})
	})

	t.Run("a skipped question does not count against its topic", func(t *testing.T) {
		wantTopics("skipper@school.test", map[string]float64{"Access control": 1})
	})

	t.Run("a no-show rolls up an empty object, not null", func(t *testing.T) {
		wantTopics("noshow@school.test", map[string]float64{})
	})

	t.Run("an untagged question names no topic", func(t *testing.T) {
		for _, email := range []string{"ace@school.test", "mixed@school.test"} {
			for topic := range readTopics(email) {
				if topic != "Access control" && topic != "Data privacy" {
					t.Fatalf("%s gained an unexpected topic %q - the untagged question leaked", email, topic)
				}
			}
		}
	})

	// Retag the live question and roll a SECOND quiz up over ace. The rollup is
	// a full recompute of ace's row from every terminal quiz, so if it read the
	// live questions table the closed quiz's "Access control" would now be
	// filed under "Renamed after the fact".
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE questions SET topic = 'Renamed after the fact' WHERE id = $1`, access); err != nil {
		t.Fatalf("retag question: %v", err)
	}
	quizTwo := newQuiz("Follow-up")
	phishing := addQuestion(quizTwo, map[string]any{
		"type": "truefalse", "body": map[string]string{"text": "Report it?"},
		"correct": true, "topic": "Phishing",
	})
	publishLive(quizTwo, "ace@school.test")
	attemptQuiz(ace, quizTwo, map[string]any{phishing: true})
	if _, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil {
		t.Fatalf("grade quiz two: %v", err)
	}
	closeQuiz(quizTwo)
	if rolled, err := analytics.RollupDue(ctx, sqlDB); err != nil || rolled != 1 {
		t.Fatalf("second rollup = (%d, %v), want (1, nil)", rolled, err)
	}

	t.Run("topics come from the version snapshot the student sat, not the live question", func(t *testing.T) {
		wantTopics("ace@school.test", map[string]float64{
			"Access control": 1, "Data privacy": 1, "Phishing": 1,
		})
	})

	// The wire shape the player reads: a JSON object of topic -> 0-1 accuracy.
	t.Run("GET /analytics/students/:id serves the topic strengths", func(t *testing.T) {
		aceID := userID(t, ctx, sqlDB, "ace@school.test")
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/students/"+aceID, nil, ace)
		if status != 200 {
			t.Fatalf("GET own analytics = %d %v, want 200", status, body)
		}
		topics, ok := body["topic_strengths"].(map[string]any)
		if !ok {
			t.Fatalf("topic_strengths = %v, want an object", body["topic_strengths"])
		}
		if len(topics) != 3 || topics["Phishing"].(float64) != 1 {
			t.Fatalf("topic_strengths = %v, want three topics all at 1", topics)
		}
	})
}
