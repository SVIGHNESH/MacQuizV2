// Package email sends transactional email through Resend's HTTP API
// (docs/09-deployment.md section 3: "Resend (3k/month), $0 - credential mail
// is low-volume"). It is the concrete implementation slotted in behind the
// small Sender interfaces quiz.Service depends on (mirrors realtime.Publisher
// satisfying attempt/quiz's EventPublisher without either module importing
// go-redis directly): quiz never imports net/http or knows Resend exists.
package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// sendTimeout hard-bounds a single API call. Callers already run this off
// the request goroutine (see quiz.Service.sendAssignmentEmail), but the
// provider itself must never be allowed to hang a background goroutine
// forever.
const sendTimeout = 10 * time.Second

const defaultAPIURL = "https://api.resend.com/emails"

// ResendSender delivers mail through Resend's transactional email API. The
// zero value is not usable - construct with NewResendSender.
type ResendSender struct {
	apiKey     string
	fromEmail  string
	fromName   string
	httpClient *http.Client
	// apiURL is the Resend endpoint; always defaultAPIURL in production.
	// Tests point it at an httptest.Server so Send's request-building logic
	// runs for real without calling out to the network.
	apiURL string
}

// NewResendSender builds a sender that authenticates with apiKey and sends
// from fromEmail (displayed as fromName, e.g. "MacQuiz <notify@example.edu>").
func NewResendSender(apiKey, fromEmail, fromName string) *ResendSender {
	return &ResendSender{
		apiKey:     apiKey,
		fromEmail:  fromEmail,
		fromName:   fromName,
		httpClient: &http.Client{Timeout: sendTimeout},
		apiURL:     defaultAPIURL,
	}
}

type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
}

// Send delivers one plain-text email. It returns an error on any non-2xx
// response or transport failure; callers in this codebase treat email as
// best-effort (docs/05 section 1's "persist first, notify second" discipline
// applied to a slower transport than Redis) and log rather than propagate it.
func (s *ResendSender) Send(ctx context.Context, to, toName, subject, textBody string) error {
	from := s.fromEmail
	if s.fromName != "" {
		from = fmt.Sprintf("%s <%s>", s.fromName, s.fromEmail)
	}
	recipient := to
	if toName != "" {
		recipient = fmt.Sprintf("%s <%s>", toName, to)
	}
	body, err := json.Marshal(resendRequest{
		From:    from,
		To:      []string{recipient},
		Subject: subject,
		Text:    textBody,
	})
	if err != nil {
		return fmt.Errorf("marshal resend request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build resend request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.apiKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call resend: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("resend responded %d: %s", resp.StatusCode, bytes.TrimSpace(errBody))
	}
	return nil
}
