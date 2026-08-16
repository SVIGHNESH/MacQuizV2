package authusers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
)

// credentialCaptureSender is a test double for authusers.EmailSender. The
// service fires each send from its own detached goroutine
// (email.go's sendCredentialEmail), so tests poll waitForCredentialEmails
// rather than asserting immediately after the HTTP call returns.
type credentialCaptureSender struct {
	mu   sync.Mutex
	sent []credentialEmail
}

type credentialEmail struct {
	to, toName, subject, body string
}

func (c *credentialCaptureSender) Send(_ context.Context, to, toName, subject, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, credentialEmail{to: to, toName: toName, subject: subject, body: body})
	return nil
}

func (c *credentialCaptureSender) snapshot() []credentialEmail {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]credentialEmail, len(c.sent))
	copy(out, c.sent)
	return out
}

func waitForCredentialEmails(t *testing.T, sender *credentialCaptureSender, want int) []credentialEmail {
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

// TestCredentialEmailsE2E pins the credential leg of admin provisioning:
// POST /users mails the new account its one-time password, PATCH /users/:id
// with reset_password mails the fresh one, and any other patch stays silent.
// The password in the mail must be the same one the API response carries -
// both come from the single generatePassword call.
//
// It runs in its own database (macquiz_credemailtest).
func TestCredentialEmailsE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := freshDatabase(t, ctx, baseURL, "macquiz_credemailtest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := authusers.NewService(sqlDB, "test-secret", log)
	sender := &credentialCaptureSender{}
	svc.SetEmailSender(sender, "https://macquiz.example.edu")
	router := httpserver.New(httpserver.BuildInfo{Version: "test"},
		httpserver.Deps{DB: sqlDB, Auth: authusers.NewHandler(svc, false)})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := svc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	_, _, admin := call(t, server, "POST", "/api/v1/auth/login",
		map[string]string{"email": "admin@school.test", "password": "admin-password-1"}, nil)

	var studentID string
	t.Run("provisioning mails the one-time credential", func(t *testing.T) {
		status, body, _ := call(t, server, "POST", "/api/v1/users",
			map[string]string{"role": "student", "email": "pupil@school.test", "full_name": "Pat Pupil"}, admin)
		if status != 201 {
			t.Fatalf("provision student = %d %v, want 201", status, body)
		}
		studentID = body["user"].(map[string]any)["id"].(string)
		password, _ := body["initial_password"].(string)
		if password == "" {
			t.Fatal("provisioning did not return the one-time initial_password")
		}

		mails := waitForCredentialEmails(t, sender, 1)
		m := mails[0]
		if m.to != "pupil@school.test" || m.toName != "Pat Pupil" {
			t.Fatalf("mail recipient = %q %q, want pupil@school.test Pat Pupil", m.to, m.toName)
		}
		if m.subject != "Your MacQuiz account" {
			t.Fatalf("mail subject = %q, want the new-account wording", m.subject)
		}
		if !strings.Contains(m.body, password) {
			t.Fatalf("mail body does not carry the one-time password the API returned")
		}
		if !strings.Contains(m.body, "pupil@school.test") {
			t.Fatalf("mail body does not carry the sign-in email")
		}
		if !strings.Contains(m.body, "https://macquiz.example.edu") {
			t.Fatalf("mail body does not carry the sign-in link")
		}
	})

	t.Run("a password reset mails the fresh credential", func(t *testing.T) {
		status, body, _ := call(t, server, "PATCH", "/api/v1/users/"+studentID,
			map[string]any{"reset_password": true}, admin)
		if status != 200 {
			t.Fatalf("reset password = %d %v, want 200", status, body)
		}
		password, _ := body["initial_password"].(string)
		if password == "" {
			t.Fatal("reset did not return the fresh initial_password")
		}

		mails := waitForCredentialEmails(t, sender, 2)
		m := mails[1]
		if m.to != "pupil@school.test" {
			t.Fatalf("reset mail recipient = %q, want pupil@school.test", m.to)
		}
		if m.subject != "Your MacQuiz password was reset" {
			t.Fatalf("reset mail subject = %q, want the reset wording", m.subject)
		}
		if !strings.Contains(m.body, password) {
			t.Fatalf("reset mail body does not carry the fresh one-time password")
		}
	})

	t.Run("a rename patch sends no email", func(t *testing.T) {
		status, body, _ := call(t, server, "PATCH", "/api/v1/users/"+studentID,
			map[string]any{"full_name": "Pat Q. Pupil"}, admin)
		if status != 200 {
			t.Fatalf("rename = %d %v, want 200", status, body)
		}
		// The send goroutine is fired before the handler responds, so a short
		// settle is enough to catch a stray third mail.
		time.Sleep(100 * time.Millisecond)
		if got := sender.snapshot(); len(got) != 2 {
			t.Fatalf("email sends after rename = %d, want 2 (no new mail)", len(got))
		}
	})
}
