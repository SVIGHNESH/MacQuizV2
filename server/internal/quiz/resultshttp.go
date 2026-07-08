package quiz

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/httpapi"
)

// handleReleaseResults serves POST /quizzes/{id}/release-results (owner).
// Idempotent: a second release returns the same released quiz.
func (h *Handler) handleReleaseResults(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	q, err := h.svc.ReleaseResults(r.Context(), actor, id)
	if err != nil {
		if errors.Is(err, ErrQuizNotClosed) {
			httpapi.WriteError(w, http.StatusConflict, httpapi.CodeQuizNotClosed,
				"results can be released once the quiz has closed")
			return
		}
		h.writeQuizError(w, "release results", err, "no such quiz")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"quiz": q})
}

// handleResults serves GET /quizzes/{id}/results (owner): the per-student
// attempt/score table the teacher reads before deciding to release.
func (h *Handler) handleResults(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	q, results, err := h.svc.Results(r.Context(), actor, id)
	if h.writeQuizError(w, "list results", err, "no such quiz") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"quiz":    q,
		"results": results,
	})
}

// csvUnsafeChars strips everything but the characters safe to embed in a
// Content-Disposition filename without quoting/escaping games; a quiz title
// can contain quotes, commas, or newlines, none of which belong in a header.
var csvUnsafeChars = regexp.MustCompile(`[^A-Za-z0-9 _-]+`)

// resultsCSVFilename turns a quiz title into a short, header-safe download
// filename, falling back to a generic name for a title that sanitizes to
// nothing (e.g. one written entirely in a non-Latin script).
func resultsCSVFilename(title string) string {
	name := csvUnsafeChars.ReplaceAllString(title, "")
	if len(name) > 60 {
		name = name[:60]
	}
	if name == "" {
		name = "quiz-results"
	}
	return name + ".csv"
}

// handleResultsCSV serves GET /quizzes/{id}/results.csv (owner): the same
// per-student results table as handleResults, rendered as a downloadable CSV
// gradebook (docs/07 section 4's "CSV exports", docs/12 Milestone 8's last
// unimplemented gap). It reuses Service.Results directly rather than
// re-querying, so the CSV and JSON views can never disagree.
func (h *Handler) handleResultsCSV(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	q, results, err := h.svc.Results(r.Context(), actor, id)
	if h.writeQuizError(w, "export results", err, "no such quiz") {
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", resultsCSVFilename(q.Title)))
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"student_name", "email", "attempt_no", "status", "submit_kind",
		"started_at", "submitted_at", "score", "max_score", "score_overridden",
	})
	for _, res := range results {
		_ = cw.Write([]string{
			res.FullName,
			res.Email,
			intPtrString(res.AttemptNo),
			strPtrString(res.Status),
			strPtrString(res.SubmitKind),
			timePtrString(res.StartedAt),
			timePtrString(res.SubmittedAt),
			floatPtrString(res.Score),
			floatPtrString(res.MaxScore),
			strconv.FormatBool(res.ScoreOverridden),
		})
	}
	cw.Flush()
}

func intPtrString(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

func strPtrString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func timePtrString(v *time.Time) string {
	if v == nil {
		return ""
	}
	return v.UTC().Format(time.RFC3339)
}

func floatPtrString(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}
