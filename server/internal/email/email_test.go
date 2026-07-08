package email

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResendSenderSendsExpectedRequest(t *testing.T) {
	var gotAuth string
	var gotBody resendRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := NewResendSender("test-key", "notify@macquiz.example.edu", "MacQuiz")
	sender.apiURL = server.URL

	if err := sender.Send(context.Background(), "student@school.test", "Alice", "Assigned: Quiz", "body text"); err != nil {
		t.Fatalf("send: %v", err)
	}

	if gotAuth != "Bearer test-key" {
		t.Fatalf("authorization header = %q, want Bearer test-key", gotAuth)
	}
	if gotBody.From != "MacQuiz <notify@macquiz.example.edu>" {
		t.Fatalf("from = %q", gotBody.From)
	}
	if len(gotBody.To) != 1 || gotBody.To[0] != "Alice <student@school.test>" {
		t.Fatalf("to = %v", gotBody.To)
	}
	if gotBody.Subject != "Assigned: Quiz" {
		t.Fatalf("subject = %q", gotBody.Subject)
	}
}

func TestResendSenderReturnsErrorOnNonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid api key"}`))
	}))
	defer server.Close()

	sender := NewResendSender("bad-key", "notify@macquiz.example.edu", "MacQuiz")
	sender.apiURL = server.URL

	err := sender.Send(context.Background(), "student@school.test", "Alice", "subject", "body")
	if err == nil {
		t.Fatal("send with bad key = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want it to mention the 401 status", err)
	}
}
