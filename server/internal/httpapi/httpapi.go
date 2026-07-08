// Package httpapi holds the JSON response and error vocabulary shared by
// every API module (docs/04-api.md section 3). Modules import it; it imports
// no modules, so it can never grow business logic.
package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Error codes from docs/04-api.md section 3 plus the auth-flow codes.
// The vocabulary is deliberately small; clients switch on Code.
const (
	CodeNotFound                = "NOT_FOUND"
	CodeValidationFailed        = "VALIDATION_FAILED"
	CodeRateLimited             = "RATE_LIMITED"
	CodeInvalidCredentials      = "INVALID_CREDENTIALS"
	CodeUnauthenticated         = "UNAUTHENTICATED"
	CodeForbidden               = "FORBIDDEN"
	CodePasswordChangeRequired  = "PASSWORD_CHANGE_REQUIRED"
	CodeQuizNotEditable         = "QUIZ_NOT_EDITABLE"
	CodeQuizNotLive             = "QUIZ_NOT_LIVE"
	CodeQuizNotClosed           = "QUIZ_NOT_CLOSED"
	CodeResultsNotReleased      = "RESULTS_NOT_RELEASED"
	CodeAttemptLimitReached     = "ATTEMPT_LIMIT_REACHED"
	CodeAttemptDeadlinePassed   = "ATTEMPT_DEADLINE_PASSED"
	CodeAttemptKicked           = "ATTEMPT_KICKED"
	CodeAttemptAlreadySubmitted = "ATTEMPT_ALREADY_SUBMITTED"
	CodeAttemptNotKicked        = "ATTEMPT_NOT_KICKED"
	CodeAttemptNotGraded        = "ATTEMPT_NOT_GRADED"
	CodeGuardrailOff            = "GUARDRAIL_OFF"
	CodeImportNotReady          = "IMPORT_NOT_READY"
)

// ErrorBody is the wire shape of every non-2xx response.
type ErrorBody struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// WriteJSON writes v with the given status. Encoding errors are unrecoverable
// mid-response, so they are ignored; the client sees a truncated body.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the standard error envelope.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Code: code, Message: message})
}

// WriteFieldErrors writes a 422 VALIDATION_FAILED with per-field messages.
func WriteFieldErrors(w http.ResponseWriter, fields map[string]string) {
	WriteJSON(w, http.StatusUnprocessableEntity, ErrorBody{
		Code:    CodeValidationFailed,
		Message: "validation failed",
		Fields:  fields,
	})
}

// WriteRateLimited writes a 429 RATE_LIMITED with a Retry-After header set
// from retry (rounded up to the next whole second).
func WriteRateLimited(w http.ResponseWriter, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
	WriteError(w, http.StatusTooManyRequests, CodeRateLimited, "too many attempts; slow down")
}
