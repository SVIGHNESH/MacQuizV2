package attempt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"

	"macquiz/server/internal/audit"
	"macquiz/server/internal/authusers"
)

// Sentinel errors the HTTP layer maps onto the docs/04-api.md vocabulary.
var (
	// ErrNotFound covers absent attempts, attempts owned by someone else,
	// and quizzes the student is not assigned to: existence never leaks
	// (docs/04-api.md section 1).
	ErrNotFound = errors.New("attempt not found")
	// ErrQuizNotLive marks a start outside the live window.
	ErrQuizNotLive = errors.New("quiz is not live")
	// ErrAttemptLimit marks a start after max_attempts is exhausted.
	ErrAttemptLimit = errors.New("attempt limit reached")
	// ErrDeadlinePassed marks any write after deadline_at plus grace.
	ErrDeadlinePassed = errors.New("attempt deadline passed")
	// ErrKicked marks writes against a kicked attempt; the client shows the
	// lockout screen (docs/06 section 4).
	ErrKicked = errors.New("attempt was kicked")
	// ErrAlreadySubmitted marks autosaves against a submitted or graded
	// attempt; a stale tab learns its attempt is over.
	ErrAlreadySubmitted = errors.New("attempt already submitted")
)

// writeGrace is the autosave slack after deadline_at (docs/06 section 2:
// "server rejects writes where now() > deadline_at + 5 s grace").
const writeGrace = 5 * time.Second

// Attempt is the student-facing attempt shape. StudentID stays internal (it
// is always the caller) and score is withheld until the results-release
// policy lands with grading.
type Attempt struct {
	ID             string     `json:"id"`
	QuizID         string     `json:"quiz_id"`
	StudentID      string     `json:"-"`
	AttemptNo      int        `json:"attempt_no"`
	QuizVersion    int        `json:"quiz_version"`
	StartedAt      time.Time  `json:"started_at"`
	DeadlineAt     time.Time  `json:"deadline_at"`
	SubmittedAt    *time.Time `json:"submitted_at"`
	SubmitKind     *string    `json:"submit_kind"`
	Status         string     `json:"status"`
	ViolationCount int        `json:"violation_count"`
}

// Question is one snapshot question as the player may see it. Correct is
// carried internally for the grader but tagged `json:"-"`, the same
// structural serializer boundary as quiz.Question (docs/12 Milestone 2).
type Question struct {
	ID       string          `json:"id"`
	Position int             `json:"position"`
	Type     string          `json:"type"`
	Body     json.RawMessage `json:"body"`
	Options  json.RawMessage `json:"options,omitempty"`
	Correct  json.RawMessage `json:"-"`
	Points   float64         `json:"points"`
}

// Answer is one autosaved response as resume returns it.
type Answer struct {
	QuestionID  string          `json:"question_id"`
	Response    json.RawMessage `json:"response"`
	TimeSpentMs int             `json:"time_spent_ms"`
	SavedAt     time.Time       `json:"saved_at"`
}

// Detail is the full player payload: the attempt, the quiz identity, the
// snapshotted guardrails, the questions (answer key stripped), the saved
// answers, and the server clock for the cosmetic countdown
// (docs/06 section 2: "server deadline + clock-offset estimate").
type Detail struct {
	Attempt    Attempt         `json:"attempt"`
	QuizTitle  string          `json:"quiz_title"`
	Guardrails json.RawMessage `json:"guardrails"`
	Questions  []Question      `json:"questions"`
	Answers    []Answer        `json:"answers"`
	Now        time.Time       `json:"now"`
}

// Service owns the attempt lifecycle. All multi-statement writes use
// explicit transactions; student actions are not audited (docs/08 section 7
// scopes audit_log to admin and teacher mutations) - the attempt_events
// stream is their record when Milestone 5 lands.
type Service struct {
	db  *sql.DB
	log *slog.Logger
	// jobs is an insert-only River client (no queues, no workers): start
	// uses it to enqueue the attempt's deadline timer inside its own
	// transaction. The worker process consumes it (internal/worker).
	jobs *river.Client[*sql.Tx]
	// events relays each committed lifecycle event to Redis pub/sub after its
	// transaction commits (docs/05 section 1). It defaults to a no-op, so the
	// service works with no realtime layer wired.
	events EventPublisher
}

// NewService wires the attempt service. An optional EventPublisher relays
// lifecycle events (started/progress/submitted) onto Redis pub/sub after they
// commit; omitting it leaves realtime delivery a no-op (the attempt_events
// rows are still written, so nothing is lost).
func NewService(db *sql.DB, log *slog.Logger, publishers ...EventPublisher) *Service {
	jobs, err := river.NewClient(riverdatabasesql.New(db), &river.Config{})
	if err != nil {
		// The empty config is statically valid; NewClient has nothing left
		// to reject, so this cannot happen at runtime.
		panic(fmt.Sprintf("build insert-only river client: %v", err))
	}
	return &Service{db: db, log: log, jobs: jobs, events: resolvePublisher(publishers)}
}

const attemptColumns = `id, quiz_id, student_id, attempt_no, quiz_version,
	started_at, deadline_at, submitted_at, submit_kind, status, violation_count`

func scanAttempt(scan func(dest ...any) error) (Attempt, error) {
	var a Attempt
	err := scan(&a.ID, &a.QuizID, &a.StudentID, &a.AttemptNo, &a.QuizVersion,
		&a.StartedAt, &a.DeadlineAt, &a.SubmittedAt, &a.SubmitKind, &a.Status,
		&a.ViolationCount)
	return a, err
}

// Start begins (or resumes) an attempt (docs/04: POST /quizzes/:id/attempts).
// One transaction checks assignment, window, and attempts used against
// max_attempts, then precomputes deadline_at = least(now() + duration,
// ends_at) so every later write validates against that one column. If an
// unexpired attempt is already in progress it is returned instead - starting
// is idempotent, so a second device or a reloaded tab resumes rather than
// burning an attempt slot. The bool reports whether an existing attempt was
// resumed.
func (s *Service) Start(ctx context.Context, actor authusers.User, quizID string) (Detail, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Detail{}, false, fmt.Errorf("begin start tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// The assignment row is both the visibility check (unassigned reads 404,
	// docs/04 section 1) and the serialization point: FOR UPDATE makes
	// concurrent starts by the same student on the same quiz queue up, so
	// attempt_no assignment cannot race.
	var assigned int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM quiz_assignments WHERE quiz_id = $1 AND student_id = $2 FOR UPDATE`,
		quizID, actor.ID).Scan(&assigned)
	if errors.Is(err, sql.ErrNoRows) {
		return Detail{}, false, ErrNotFound
	}
	if err != nil {
		return Detail{}, false, fmt.Errorf("check assignment: %w", err)
	}

	var (
		status                   string
		startsAt, endsAt         *time.Time
		durationSec, maxAttempts int
		version                  int
		shuffle                  bool
		now                      time.Time
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT status, starts_at, ends_at, coalesce(duration_sec, 0), max_attempts,
		        version, shuffle_questions, now()
		 FROM quizzes WHERE id = $1`, quizID).Scan(
		&status, &startsAt, &endsAt, &durationSec, &maxAttempts, &version, &shuffle, &now); err != nil {
		return Detail{}, false, fmt.Errorf("load quiz: %w", err)
	}
	// A draft assignment is invisible in the student list, so it stays
	// invisible here too; anything published but outside its window answers
	// QUIZ_NOT_LIVE (docs/04 section 3).
	if status == "draft" {
		return Detail{}, false, ErrNotFound
	}
	if status != "scheduled" && status != "live" {
		return Detail{}, false, ErrQuizNotLive
	}
	if startsAt == nil || endsAt == nil || now.Before(*startsAt) || !now.Before(*endsAt) {
		return Detail{}, false, ErrQuizNotLive
	}

	// An unexpired in-progress attempt is resumed, never duplicated.
	a, err := scanAttempt(tx.QueryRowContext(ctx,
		`SELECT `+attemptColumns+` FROM attempts
		 WHERE quiz_id = $1 AND student_id = $2 AND status = 'in_progress' AND deadline_at > now()
		 ORDER BY attempt_no DESC LIMIT 1`, quizID, actor.ID).Scan)
	resumed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Detail{}, false, fmt.Errorf("find open attempt: %w", err)
	}

	if !resumed {
		var used, lastNo int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*), coalesce(max(attempt_no), 0) FROM attempts
			 WHERE quiz_id = $1 AND student_id = $2`, quizID, actor.ID).Scan(&used, &lastNo); err != nil {
			return Detail{}, false, fmt.Errorf("count attempts: %w", err)
		}
		if used >= maxAttempts {
			return Detail{}, false, ErrAttemptLimit
		}
		a, err = scanAttempt(tx.QueryRowContext(ctx,
			`INSERT INTO attempts (quiz_id, student_id, attempt_no, quiz_version, started_at, deadline_at)
			 VALUES ($1, $2, $3, $4, now(), least(now() + make_interval(secs => $5), $6::timestamptz))
			 RETURNING `+attemptColumns,
			quizID, actor.ID, lastNo+1, version, durationSec, endsAt).Scan)
		if err != nil {
			return Detail{}, false, fmt.Errorf("insert attempt: %w", err)
		}
		// The disappearing student (docs/06 section 2): the timer that will
		// auto-submit this attempt commits with the attempt itself.
		if err := s.enqueueDeadlineJob(ctx, tx, a.ID, a.DeadlineAt); err != nil {
			return Detail{}, false, err
		}
		// Persist first (docs/05 section 1): the started delta commits with the
		// attempt row, so the dashboard can move this student to "in progress"
		// the moment the socket relays it. A resume emits nothing - the row is
		// already on the roster from its original start.
		if err := appendEvent(ctx, tx, a.ID, eventStarted, startedPayload{
			StudentID: a.StudentID, AttemptID: a.ID, DeadlineAt: a.DeadlineAt,
		}); err != nil {
			return Detail{}, false, err
		}
	}

	detail, err := s.buildDetail(ctx, tx, a)
	if err != nil {
		return Detail{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Detail{}, false, fmt.Errorf("commit start: %w", err)
	}
	// Publish second (docs/05 section 1): the started row is now durable, so
	// relay the same delta to the live dashboard. A resume republishes nothing -
	// the row it resumed is already on the roster from its original start.
	if !resumed {
		s.events.Publish(ctx, a.QuizID, a.ID, eventStarted, startedPayload{
			StudentID: a.StudentID, AttemptID: a.ID, DeadlineAt: a.DeadlineAt,
		})
	}
	return detail, resumed, nil
}

// Get resumes an attempt (docs/04: GET /attempts/:id) - the saved answers,
// the deadline, and the current server time. Owner-only; anyone else reads
// 404.
func (s *Service) Get(ctx context.Context, actor authusers.User, id string) (Detail, error) {
	a, err := scanAttempt(s.db.QueryRowContext(ctx,
		`SELECT `+attemptColumns+` FROM attempts WHERE id = $1`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, fmt.Errorf("load attempt: %w", err)
	}
	if a.StudentID != actor.ID {
		return Detail{}, ErrNotFound
	}
	return s.buildDetail(ctx, s.db, a)
}

// SaveAnswer upserts one response (docs/04: PUT /attempts/:id/answers/:qid).
// Idempotent on (attempt_id, question_id); refused once the deadline plus
// grace has passed or the attempt has left in_progress - the same checks
// that guard submit, so there is exactly one write gate.
func (s *Service) SaveAnswer(ctx context.Context, actor authusers.User, attemptID, questionID string, response json.RawMessage, timeSpentMs int) (Answer, time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Answer{}, time.Time{}, fmt.Errorf("begin save tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	a, err := s.writableForUpdate(ctx, tx, actor, attemptID)
	if err != nil {
		return Answer{}, time.Time{}, err
	}

	// The question must exist in the snapshot version this attempt pinned;
	// a mid-window republish can never make a stale player write outside
	// what its student actually saw.
	var inSnapshot bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM quiz_versions v, jsonb_array_elements(v.questions) q
		    WHERE v.quiz_id = $1 AND v.version = $2 AND q->>'id' = $3)`,
		a.QuizID, a.QuizVersion, questionID).Scan(&inSnapshot); err != nil {
		return Answer{}, time.Time{}, fmt.Errorf("check snapshot membership: %w", err)
	}
	if !inSnapshot {
		return Answer{}, time.Time{}, ErrNotFound
	}

	var ans Answer
	ans.QuestionID = questionID
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO attempt_answers (attempt_id, question_id, response, time_spent_ms)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (attempt_id, question_id)
		 DO UPDATE SET response = $3, time_spent_ms = $4, saved_at = now()
		 RETURNING response, time_spent_ms, saved_at`,
		attemptID, questionID, []byte(response), timeSpentMs).Scan(
		&ans.Response, &ans.TimeSpentMs, &ans.SavedAt); err != nil {
		return Answer{}, time.Time{}, fmt.Errorf("upsert answer: %w", err)
	}
	// The progress delta rides the autosave transaction. It carries only the
	// answered count read after this upsert; the cursor stays null because the
	// server never learns the student's position over REST (see progressPayload).
	answered, err := countAnswered(ctx, tx, attemptID)
	if err != nil {
		return Answer{}, time.Time{}, err
	}
	if err := appendEvent(ctx, tx, attemptID, eventProgress, progressPayload{
		AnsweredCount: answered,
	}); err != nil {
		return Answer{}, time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return Answer{}, time.Time{}, fmt.Errorf("commit save: %w", err)
	}
	// Publish second: the progress row is durable, so relay the answered-count
	// delta. current_question stays null for the same reason the row does - no
	// server cursor over REST (see progressPayload).
	s.events.Publish(ctx, a.QuizID, attemptID, eventProgress, progressPayload{
		AnsweredCount: answered,
	})
	return ans, a.DeadlineAt, nil
}

// SubmitManual is the student's own submit (docs/04 section 4, kind=manual).
// It rides the shared funnel, so the deadline check, the terminal-state
// rules, and race resolution are written exactly once.
func (s *Service) SubmitManual(ctx context.Context, actor authusers.User, attemptID string) (Attempt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Attempt{}, fmt.Errorf("begin submit tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	a, err := s.ownedForUpdate(ctx, tx, actor, attemptID)
	if err != nil {
		return Attempt{}, err
	}
	freshlySubmitted := a.Status == "in_progress"
	a, submitted, err := submit(ctx, tx, a, "manual")
	if err != nil {
		return Attempt{}, err
	}
	// Submission enqueues the grading job in the same transaction (docs/04
	// section 4); a repeat submit of a terminal attempt enqueues nothing.
	if freshlySubmitted {
		if err := s.enqueueGradeJob(ctx, tx, a.ID); err != nil {
			return Attempt{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Attempt{}, fmt.Errorf("commit submit: %w", err)
	}
	// Publish second: only a real in_progress -> submitted flip returns a
	// payload (a repeat submit returns nil), so the submitted delta relays
	// exactly once per attempt, mirroring the single appendEvent it committed.
	if submitted != nil {
		s.events.Publish(ctx, a.QuizID, a.ID, eventSubmitted, *submitted)
	}
	return a, nil
}

// submit is the idempotent per-request termination funnel (docs/04 section
// 4) for the student-driven kinds: manual now, kicked when the kick endpoint
// lands. The batch kinds - auto and forced - are applied by SweepDueAttempts
// (scheduler.go) behind the same status = 'in_progress' guard this UPDATE
// uses, so the two paths can never double-terminate. The caller holds the
// attempt row lock. A repeat submit of an already-terminated attempt returns
// it unchanged, so a manual submit racing the deadline job resolves cleanly
// to whichever committed first. The caller enqueues grading for a fresh
// termination; the sweep-driven kinds are graded by the same worker pass
// that applies them (GradeSubmitted, grade.go).
// It returns the submitted payload it appended when it actually flipped the
// attempt, and nil for the idempotent terminal cases, so the caller can relay
// the event to Redis after commit exactly when a row changed (docs/05).
func submit(ctx context.Context, tx *sql.Tx, a Attempt, kind string) (Attempt, *submittedPayload, error) {
	switch a.Status {
	case "kicked":
		return Attempt{}, nil, ErrKicked
	case "submitted", "graded":
		return a, nil, nil
	}
	// Only the student-driven kind races the clock; auto and forced ARE the
	// deadline firing, and a kick is valid until the moment it commits.
	if kind == "manual" {
		var late bool
		if err := tx.QueryRowContext(ctx,
			`SELECT now() > $1::timestamptz + $2::interval`,
			a.DeadlineAt, writeGrace.String()).Scan(&late); err != nil {
			return Attempt{}, nil, fmt.Errorf("check deadline: %w", err)
		}
		if late {
			return Attempt{}, nil, ErrDeadlinePassed
		}
	}
	a, err := scanAttempt(tx.QueryRowContext(ctx,
		`UPDATE attempts
		 SET status = 'submitted', submit_kind = $1, submitted_at = now()
		 WHERE id = $2 AND status = 'in_progress'
		 RETURNING `+attemptColumns, kind, a.ID).Scan)
	if err != nil {
		return Attempt{}, nil, fmt.Errorf("mark submitted: %w", err)
	}
	// Only a real in_progress -> submitted flip reaches here (the terminal
	// cases returned above), so the submitted delta is emitted exactly once per
	// attempt, in the same transaction as the flip.
	answered, err := countAnswered(ctx, tx, a.ID)
	if err != nil {
		return Attempt{}, nil, err
	}
	payload := submittedPayload{SubmitKind: kind, AnsweredCount: answered}
	if err := appendEvent(ctx, tx, a.ID, eventSubmitted, payload); err != nil {
		return Attempt{}, nil, err
	}
	return a, &payload, nil
}

// Kick terminates a live attempt on a teacher's or admin's order (docs/06
// section 4). Authorization is ActionAttemptModerate - the owning teacher or
// any admin - decided against the attempt's quiz owner, so a non-owning teacher
// (and an unknown attempt) reads 404 and existence never leaks. One transaction
// flips the attempt to 'kicked', records who kicked it and why, appends the
// attempt.kicked event, writes the audit row, and enqueues grading of whatever
// was autosaved ("kicked work is graded, not discarded"). Enforcement is that
// status flip, not the socket: the same status = 'in_progress' gate that gates
// every autosave and submit now rejects the kicked student (writableForUpdate,
// submit). A repeat kick, or a kick that lost the race to an auto-submit,
// resolves cleanly to whichever committed first and emits nothing.
func (s *Service) Kick(ctx context.Context, actor authusers.User, attemptID, reason string) (Attempt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Attempt{}, fmt.Errorf("begin kick tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	a, err := s.moderatableForUpdate(ctx, tx, actor, attemptID)
	if err != nil {
		return Attempt{}, err
	}

	freshlyKicked := a.Status == "in_progress"
	a, kicked, err := kick(ctx, tx, a, actor.ID, reason)
	if err != nil {
		return Attempt{}, err
	}
	// A real flip enqueues grading and records the audit trail; a no-op re-kick
	// (or a kick the auto-submit beat) touches neither, so the audit_log carries
	// exactly one row per attempt actually removed.
	if freshlyKicked {
		if err := s.enqueueGradeJob(ctx, tx, a.ID); err != nil {
			return Attempt{}, err
		}
		if err := audit.Write(ctx, tx, actor.ID, "attempt.kicked", "attempt", a.ID,
			map[string]any{"quiz_id": a.QuizID, "student_id": a.StudentID, "reason": reason}); err != nil {
			return Attempt{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Attempt{}, fmt.Errorf("commit kick: %w", err)
	}
	// Publish second: only a real in_progress -> kicked flip returns a payload,
	// so the kicked delta relays exactly once per attempt, mirroring the single
	// appendEvent it committed.
	if kicked != nil {
		s.events.Publish(ctx, a.QuizID, a.ID, eventKicked, *kicked)
	}
	return a, nil
}

// kick is the idempotent kick transition (docs/06 section 4). It shares the
// terminal-state discipline of submit - a kick that races an auto-submit, a
// manual submit, or a second kick resolves to whichever committed first and
// writes nothing - but it is its own transition, not a submit: 'kicked' is a
// distinct terminal status the roster shows separately, and the delta is
// attempt.kicked, not attempt.submitted. It returns the kicked payload it
// appended when it actually flipped the row, and nil for the terminal no-ops,
// so the caller relays the event to Redis after commit exactly when a row
// changed.
func kick(ctx context.Context, tx *sql.Tx, a Attempt, kickedBy, reason string) (Attempt, *kickedPayload, error) {
	switch a.Status {
	case "kicked", "submitted", "graded":
		// Already terminal: a repeat kick is idempotent, and a kick that lost
		// the race to a submit leaves that submit standing (docs/06 section 4).
		return a, nil, nil
	}
	a, err := scanAttempt(tx.QueryRowContext(ctx,
		`UPDATE attempts
		 SET status = 'kicked', submit_kind = 'kicked', submitted_at = now(),
		     kicked_by = $1, kick_reason = $2
		 WHERE id = $3 AND status = 'in_progress'
		 RETURNING `+attemptColumns, kickedBy, reason, a.ID).Scan)
	if err != nil {
		return Attempt{}, nil, fmt.Errorf("mark kicked: %w", err)
	}
	payload := kickedPayload{KickedBy: kickedBy, Reason: reason}
	if err := appendEvent(ctx, tx, a.ID, eventKicked, payload); err != nil {
		return Attempt{}, nil, err
	}
	return a, &payload, nil
}

// moderatableForUpdate locks the attempt row and authorizes the caller to
// moderate it (kick) against the attempt's quiz owner: the owning teacher or
// any admin (ActionAttemptModerate). A missing attempt, and one the caller may
// not moderate, both read ErrNotFound so a non-owning teacher cannot even learn
// the attempt exists.
func (s *Service) moderatableForUpdate(ctx context.Context, tx *sql.Tx, actor authusers.User, id string) (Attempt, error) {
	a, err := scanAttempt(tx.QueryRowContext(ctx,
		`SELECT `+attemptColumns+` FROM attempts WHERE id = $1 FOR UPDATE`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("load attempt: %w", err)
	}
	var ownerID string
	if err := tx.QueryRowContext(ctx,
		`SELECT owner_id FROM quizzes WHERE id = $1`, a.QuizID).Scan(&ownerID); err != nil {
		return Attempt{}, fmt.Errorf("load quiz owner: %w", err)
	}
	if !authusers.Can(actor, authusers.ActionAttemptModerate, authusers.Resource{OwnerID: ownerID}) {
		return Attempt{}, ErrNotFound
	}
	return a, nil
}

// ownedForUpdate locks the attempt row and verifies the caller owns it;
// anything else reads ErrNotFound.
func (s *Service) ownedForUpdate(ctx context.Context, tx *sql.Tx, actor authusers.User, id string) (Attempt, error) {
	a, err := scanAttempt(tx.QueryRowContext(ctx,
		`SELECT `+attemptColumns+` FROM attempts WHERE id = $1 FOR UPDATE`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("load attempt: %w", err)
	}
	if a.StudentID != actor.ID {
		return Attempt{}, ErrNotFound
	}
	return a, nil
}

// writableForUpdate extends ownedForUpdate with the write gate every
// autosave passes (docs/03 section 5 invariant 2): in_progress status and
// the deadline plus grace, both judged on the database clock that set
// deadline_at in the first place.
func (s *Service) writableForUpdate(ctx context.Context, tx *sql.Tx, actor authusers.User, id string) (Attempt, error) {
	a, err := s.ownedForUpdate(ctx, tx, actor, id)
	if err != nil {
		return Attempt{}, err
	}
	switch a.Status {
	case "kicked":
		return Attempt{}, ErrKicked
	case "submitted", "graded":
		return Attempt{}, ErrAlreadySubmitted
	}
	var late bool
	if err := tx.QueryRowContext(ctx,
		`SELECT now() > $1::timestamptz + $2::interval`,
		a.DeadlineAt, writeGrace.String()).Scan(&late); err != nil {
		return Attempt{}, fmt.Errorf("check deadline: %w", err)
	}
	if late {
		return Attempt{}, ErrDeadlinePassed
	}
	return a, nil
}

// querier abstracts *sql.DB and *sql.Tx for the read helpers.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// buildDetail assembles the player payload for one attempt: quiz identity,
// snapshot questions and guardrails at the pinned version, saved answers,
// and the server clock.
func (s *Service) buildDetail(ctx context.Context, db querier, a Attempt) (Detail, error) {
	d := Detail{Attempt: a, Answers: []Answer{}}

	var questionsJSON []byte
	var shuffle bool
	if err := db.QueryRowContext(ctx,
		`SELECT z.title, z.shuffle_questions, v.questions, v.guardrails, now()
		 FROM quizzes z JOIN quiz_versions v ON v.quiz_id = z.id AND v.version = $2
		 WHERE z.id = $1`, a.QuizID, a.QuizVersion).Scan(
		&d.QuizTitle, &shuffle, &questionsJSON, &d.Guardrails, &d.Now); err != nil {
		return Detail{}, fmt.Errorf("load snapshot: %w", err)
	}
	questions, err := decodeSnapshot(questionsJSON)
	if err != nil {
		return Detail{}, err
	}
	if shuffle {
		shuffleForAttempt(questions, a.ID)
	}
	d.Questions = questions

	rows, err := db.QueryContext(ctx,
		`SELECT question_id, response, time_spent_ms, saved_at
		 FROM attempt_answers WHERE attempt_id = $1 ORDER BY saved_at, question_id`, a.ID)
	if err != nil {
		return Detail{}, fmt.Errorf("load answers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ans Answer
		var response []byte
		if err := rows.Scan(&ans.QuestionID, &response, &ans.TimeSpentMs, &ans.SavedAt); err != nil {
			return Detail{}, fmt.Errorf("scan answer: %w", err)
		}
		ans.Response = response
		d.Answers = append(d.Answers, ans)
	}
	return d, rows.Err()
}

// snapshotWire mirrors one element of quiz_versions.questions. It exists
// because Question tags Correct with `json:"-"`: decoding into Question
// directly would silently drop the answer key the grader needs.
type snapshotWire struct {
	ID       string          `json:"id"`
	Position int             `json:"position"`
	Type     string          `json:"type"`
	Body     json.RawMessage `json:"body"`
	Options  json.RawMessage `json:"options"`
	Correct  json.RawMessage `json:"correct"`
	Points   float64         `json:"points"`
}

// decodeSnapshot parses the immutable question snapshot into player
// questions, keeping the answer key on the internal field only.
func decodeSnapshot(raw []byte) ([]Question, error) {
	var wire []snapshotWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	questions := make([]Question, len(wire))
	for i, w := range wire {
		options := w.Options
		// truefalse and short questions snapshot a JSON null; drop it so
		// omitempty elides the field.
		if string(options) == "null" {
			options = nil
		}
		questions[i] = Question{
			ID: w.ID, Position: w.Position, Type: w.Type, Body: w.Body,
			Options: options, Correct: w.Correct, Points: w.Points,
		}
	}
	return questions, nil
}

// shuffleForAttempt orders questions deterministically per attempt: the sort
// key is a hash of (attempt id, question id), so every resume - any device,
// any time - sees the same order, with no per-attempt order storage.
// Positions are renumbered densely so the player can show "3 of 10".
func shuffleForAttempt(questions []Question, attemptID string) {
	key := func(questionID string) uint64 {
		h := fnv.New64a()
		h.Write([]byte(attemptID))
		h.Write([]byte(":"))
		h.Write([]byte(questionID))
		return h.Sum64()
	}
	sort.Slice(questions, func(i, j int) bool {
		return key(questions[i].ID) < key(questions[j].ID)
	})
	for i := range questions {
		questions[i].Position = i + 1
	}
}
