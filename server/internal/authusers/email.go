package authusers

import (
	"context"
	"fmt"
	"time"
)

// emailSendTimeout bounds each detached credential-email goroutine - long
// enough for a normal provider round trip, short enough that a hung provider
// never leaks a goroutine indefinitely (same rationale as quiz's assignment
// emails).
const emailSendTimeout = 10 * time.Second

// EmailSender delivers one plain-text transactional email - the credential
// leg of admin provisioning: a newly created account, or one whose password
// an admin reset, gets its one-time credential mailed to the address on
// file. email.BrevoSender (server/internal/email) is the concrete
// implementation; the interface lives here so this package never imports
// net/http or a specific provider, mirroring quiz.EmailSender.
type EmailSender interface {
	Send(ctx context.Context, to, toName, subject, textBody string) error
}

// noopEmailSender is the default: every test, and any deploy that has not
// configured an email provider, gets one that silently drops every send.
// The credential still reaches the admin in the API response either way -
// email is additive, never load-bearing.
type noopEmailSender struct{}

func (noopEmailSender) Send(context.Context, string, string, string, string) error { return nil }

// SetEmailSender wires the credential-email leg of admin provisioning
// (email.NewBrevoSender in production). Mirrors quiz.Service.SetEmailSender:
// optional, called once at boot, nil-safe to omit entirely. publicURL, when
// non-empty (cfg.PublicURL), puts a sign-in link in the mail body.
func (s *Service) SetEmailSender(e EmailSender, publicURL string) {
	if e != nil {
		s.email = e
	}
	s.publicURL = publicURL
}

// sendCredentialEmail fires one best-effort email carrying a first-login
// credential in its own goroutine, after the provisioning transaction has
// committed - a provider outage must never fail or stall user creation. reset
// selects the password-reset wording over the new-account one.
func (s *Service) sendCredentialEmail(ctx context.Context, u User, password string, reset bool) {
	if u.Email == "" || password == "" {
		return
	}
	subject := "Your MacQuiz account"
	intro := "An account has been created for you on MacQuiz."
	if reset {
		subject = "Your MacQuiz password was reset"
		intro = "An administrator has reset your MacQuiz password."
	}
	signIn := "Sign in with:"
	if s.publicURL != "" {
		signIn = fmt.Sprintf("Sign in at %s with:", s.publicURL)
	}
	body := fmt.Sprintf(
		"Hi %s,\n\n%s\n\n%s\n\n  Email: %s\n  One-time password: %s\n\n"+
			"This password works exactly once - you will be asked to choose your own the first time you sign in.\n",
		u.FullName, intro, signIn, u.Email, password)
	go func() {
		sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), emailSendTimeout)
		defer cancel()
		if err := s.email.Send(sendCtx, u.Email, u.FullName, subject, body); err != nil {
			s.log.Warn("send credential email", "user_id", u.ID, "reset", reset, "err", err)
		}
	}()
}
