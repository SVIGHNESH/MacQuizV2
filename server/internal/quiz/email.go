package quiz

import (
	"context"
	"time"
)

// emailSendTimeout bounds each detached, per-recipient email goroutine
// SetAssignments spawns (lifecycle.go's sendAssignmentEmail) - long enough
// for a normal provider round trip, short enough that a hung provider never
// leaks a goroutine indefinitely.
const emailSendTimeout = 10 * time.Second

// EmailSender delivers one plain-text transactional email. It is the other
// half of "Notifications on assignment changes (in-app/email)" - the
// user:{id}:notify channel (events.go) covers the in-app half; this covers
// the email leg docs/09-deployment.md's cost table names ("Email | Brevo
// free or Resend | Credential mail is low-volume"). email.ResendSender
// (server/internal/email) is the concrete implementation; the interface
// lives here so this package never imports net/http or a specific provider,
// matching the EventPublisher/SnapshotCache decoupling pattern already used
// for realtime and Redis.
type EmailSender interface {
	Send(ctx context.Context, to, toName, subject, textBody string) error
}

// noopEmailSender is the default: every test, and any deploy that has not
// configured an email provider, gets one that silently drops every send.
// Assignment notifications remain fully functional over the in-app channel
// with no provider configured - email is additive, never load-bearing.
type noopEmailSender struct{}

func (noopEmailSender) Send(context.Context, string, string, string, string) error { return nil }
