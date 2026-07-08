package quiz_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
)

// captureEmailSender is a test double for quiz.EmailSender. SetAssignments
// fires each send from its own detached goroutine (lifecycle.go's
// sendAssignmentEmail), so tests poll waitForEmails rather than asserting
// immediately after the HTTP call returns.
type captureEmailSender struct {
	mu   sync.Mutex
	sent []capturedEmail
}

type capturedEmail struct {
	to, toName, subject, body string
}

func (c *captureEmailSender) Send(_ context.Context, to, toName, subject, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, capturedEmail{to: to, toName: toName, subject: subject, body: body})
	return nil
}

func (c *captureEmailSender) snapshot() []capturedEmail {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedEmail, len(c.sent))
	copy(out, c.sent)
	return out
}

// waitForEmails polls until at least want sends have arrived or the timeout
// elapses, so the test does not race the background send goroutine.
func waitForEmails(t *testing.T, sender *captureEmailSender, want int) []capturedEmail {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := sender.snapshot()
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("email sends = %d, want at least %d", len(got), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestQuizAssignmentEmailsE2E pins the email leg of "Notifications on
// assignment changes": SetAssignments (lifecycle.go) sends one email to
// every student whose membership in the audience changed - an "assigned"
// email for a newly-added student, a "removed" email for a dropped one -
// and stays silent for a no-op re-save. It runs independently of the
// existing in-app TestQuizAssignmentNotificationsE2E (publishflow_test.go),
// which pins the WS/user:{id}:notify leg of the same feature.
//
// It runs in its own database (macquiz_assignemailtest).
func TestQuizAssignmentEmailsE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_assignemailtest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	quizSvc := quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: t.TempDir()})
	sender := &captureEmailSender{}
	quizSvc.SetEmailSender(sender)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:   sqlDB,
		Auth: authusers.NewHandler(authSvc, false),
		Quiz: quiz.NewHandler(quizSvc, authSvc),
	})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	provision(t, ctx, sqlDB, "teacher", "owner3@school.test")
	provision(t, ctx, sqlDB, "student", "carol@school.test")
	provision(t, ctx, sqlDB, "student", "dave@school.test")
	owner := login(t, server, "owner3@school.test", "account-password")
	carolID := userID(t, ctx, sqlDB, "carol@school.test")
	daveID := userID(t, ctx, sqlDB, "dave@school.test")

	quizID := authorMinimalQuiz(t, server, owner, "Assignment Emails")

	t.Run("assigning a new student emails only them", func(t *testing.T) {
		assign(t, server, owner, quizID, carolID)
		got := waitForEmails(t, sender, 1)
		if got[0].to != "carol@school.test" {
			t.Fatalf("email to = %q, want carol@school.test", got[0].to)
		}
		if got[0].subject != "Assigned: Assignment Emails" {
			t.Fatalf("email subject = %q, want assignment subject", got[0].subject)
		}
	})

	t.Run("re-saving the same audience sends nothing new", func(t *testing.T) {
		before := len(sender.snapshot())
		assign(t, server, owner, quizID, carolID)
		// No new send can ever arrive for a true no-op, but give any
		// erroneous async send a moment to land before asserting silence.
		time.Sleep(100 * time.Millisecond)
		if got := len(sender.snapshot()); got != before {
			t.Fatalf("email sends after no-op re-save = %d, want %d", got, before)
		}
	})

	t.Run("swapping the audience emails both the added and the dropped student", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": []string{daveID}}, owner)
		if status != 200 {
			t.Fatalf("swap assignments = %d %v", status, body)
		}
		got := waitForEmails(t, sender, 3)
		var sawAssignedDave, sawRemovedCarol bool
		for _, e := range got[1:] {
			if e.to == "dave@school.test" && e.subject == "Assigned: Assignment Emails" {
				sawAssignedDave = true
			}
			if e.to == "carol@school.test" && e.subject == "Removed: Assignment Emails" {
				sawRemovedCarol = true
			}
		}
		if !sawAssignedDave {
			t.Fatalf("no assigned email to dave in %+v", got)
		}
		if !sawRemovedCarol {
			t.Fatalf("no removed email to carol in %+v", got)
		}
	})
}
