// Package email sends transactional email through Brevo's HTTP API
// (docs/09-deployment.md section 3 - credential mail is low-volume; Brevo's
// free tier of 300/day covers it). It is the concrete implementation slotted
// in behind the small Sender interfaces quiz.Service depends on (mirrors
// realtime.Publisher satisfying attempt/quiz's EventPublisher without either
// module importing go-redis directly): quiz never imports net/http or knows
// Brevo exists.
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

const defaultAPIURL = "https://api.brevo.com/v3/smtp/email"

// BrevoSender delivers mail through Brevo's transactional email API. The
// zero value is not usable - construct with NewBrevoSender.
type BrevoSender struct {
	apiKey     string
	fromEmail  string
	fromName   string
	httpClient *http.Client
	// apiURL is the Brevo endpoint; always defaultAPIURL in production.
	// Tests point it at an httptest.Server so Send's request-building logic
	// runs for real without calling out to the network.
	apiURL string
}

// NewBrevoSender builds a sender that authenticates with apiKey (Brevo's
// "xkeysib-..." key) and sends from fromEmail, displayed as fromName.
func NewBrevoSender(apiKey, fromEmail, fromName string) *BrevoSender {
	return &BrevoSender{
		apiKey:     apiKey,
		fromEmail:  fromEmail,
		fromName:   fromName,
		httpClient: &http.Client{Timeout: sendTimeout},
		apiURL:     defaultAPIURL,
	}
}

type brevoAddress struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type brevoRequest struct {
	Sender      brevoAddress   `json:"sender"`
	To          []brevoAddress `json:"to"`
	Subject     string         `json:"subject"`
	TextContent string         `json:"textContent"`
}

// Send delivers one plain-text email. It returns an error on any non-2xx
// response or transport failure; callers in this codebase treat email as
// best-effort (docs/05 section 1's "persist first, notify second" discipline
// applied to a slower transport than Redis) and log rather than propagate it.
func (s *BrevoSender) Send(ctx context.Context, to, toName, subject, textBody string) error {
	body, err := json.Marshal(brevoRequest{
		Sender:      brevoAddress{Email: s.fromEmail, Name: s.fromName},
		To:          []brevoAddress{{Email: to, Name: toName}},
		Subject:     subject,
		TextContent: textBody,
	})
	if err != nil {
		return fmt.Errorf("marshal brevo request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build brevo request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", s.apiKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call brevo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("brevo responded %d: %s", resp.StatusCode, bytes.TrimSpace(errBody))
	}
	return nil
}
