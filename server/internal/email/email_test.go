package email

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrevoSenderSendsExpectedRequest(t *testing.T) {
	var gotAuth string
	var gotBody brevoRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("api-key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	sender := NewBrevoSender("test-key", "notify@macquiz.example.edu", "MacQuiz")
	sender.apiURL = server.URL

	if err := sender.Send(context.Background(), "student@school.test", "Alice", "Assigned: Quiz", "body text"); err != nil {
		t.Fatalf("send: %v", err)
	}

	if gotAuth != "test-key" {
		t.Fatalf("api-key header = %q, want test-key", gotAuth)
	}
	if gotBody.Sender.Email != "notify@macquiz.example.edu" || gotBody.Sender.Name != "MacQuiz" {
		t.Fatalf("sender = %+v", gotBody.Sender)
	}
	if len(gotBody.To) != 1 || gotBody.To[0].Email != "student@school.test" || gotBody.To[0].Name != "Alice" {
		t.Fatalf("to = %+v", gotBody.To)
	}
	if gotBody.Subject != "Assigned: Quiz" {
		t.Fatalf("subject = %q", gotBody.Subject)
	}
	if gotBody.TextContent != "body text" {
		t.Fatalf("textContent = %q", gotBody.TextContent)
	}
}

func TestBrevoSenderReturnsErrorOnNonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid api key"}`))
	}))
	defer server.Close()

	sender := NewBrevoSender("bad-key", "notify@macquiz.example.edu", "MacQuiz")
	sender.apiURL = server.URL

	err := sender.Send(context.Background(), "student@school.test", "Alice", "subject", "body")
	if err == nil {
		t.Fatal("send with bad key = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want it to mention the 401 status", err)
	}
}
