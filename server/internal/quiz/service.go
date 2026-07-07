package quiz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"

	"macquiz/server/internal/audit"
	"macquiz/server/internal/authusers"
)

// Sentinel errors the HTTP layer maps onto the docs/04-api.md vocabulary.
var (
	// ErrNotFound covers both truly absent quizzes and quizzes the actor
	// does not own: existence is never leaked (docs/04-api.md section 1).
	ErrNotFound = errors.New("quiz not found")
	// ErrNotEditable marks mutations against a quiz that has left draft;
	// published question sets are immutable snapshots (docs/03 section 1).
	ErrNotEditable = errors.New("quiz is not editable once published")
	// ErrBadOrder marks a reorder list that is not a permutation of the
	// quiz's question ids.
	ErrBadOrder = errors.New("order must list every question id exactly once")
	// ErrQuizNotClosed marks a results release before the quiz's window has
	// ended; results only exist once every attempt is terminated (docs/08
	// section 3: "only after the quiz closes").
	ErrQuizNotClosed = errors.New("quiz is not closed yet")
	// ErrNotClosable marks a force-close of a quiz that is not live or
	// scheduled - a draft was never open, and a closed/archived quiz is
	// already terminal (docs/06 section 1: force-close is a Live/Scheduled
	// action).
	ErrNotClosable = errors.New("only a live or scheduled quiz can be force-closed")
	// ErrNotExtendable marks an extend of a quiz that is not effectively live:
	// a not-yet-started quiz uses reschedule, a closed/archived one is already
	// terminal (docs/06 section 1: extend is the once-Live affordance).
	ErrNotExtendable = errors.New("only a live quiz can be extended")
	// ErrNotArchivable marks an archive of a quiz that is not stored 'closed':
	// archiving is the terminal Closed -> Archived retirement (docs/06 section
	// 1), so a draft/scheduled/live quiz must be force-closed first.
	ErrNotArchivable = errors.New("only a closed quiz can be archived")
)

// Quiz is the authoring-facing quiz shape. Window and guardrail fields stay
// null until publish (Milestone 3).
type Quiz struct {
	ID               string     `json:"id"`
	OwnerID          string     `json:"owner_id"`
	Title            string     `json:"title"`
	Status           string     `json:"status"`
	StartsAt         *time.Time `json:"starts_at"`
	EndsAt           *time.Time `json:"ends_at"`
	DurationSec      *int       `json:"duration_sec"`
	MaxAttempts      int        `json:"max_attempts"`
	ShuffleQuestions bool       `json:"shuffle_questions"`
	PublishedAt      *time.Time `json:"published_at"`
	Version          int        `json:"version"`
	CreatedAt        time.Time  `json:"created_at"`
	QuestionCount    int        `json:"question_count"`
	// ReleasePolicy decides when scores become visible to students: auto
	// releases in the worker pass that grades the closed quiz, manual waits
	// for POST /quizzes/:id/release-results. ResultsReleasedAt is the fact
	// every results read gates on; null means withheld.
	ReleasePolicy     string     `json:"release_policy"`
	ResultsReleasedAt *time.Time `json:"results_released_at"`
}

// Question is the internal question shape. Correct is tagged `json:"-"` so
// the answer key can never leak through a default serialization; only the
// explicit TeacherQuestion view (serialize.go) carries it to clients.
type Question struct {
	ID       string          `json:"id"`
	QuizID   string          `json:"quiz_id"`
	Position int             `json:"position"`
	Type     string          `json:"type"`
	Body     json.RawMessage `json:"body"`
	Options  json.RawMessage `json:"options,omitempty"`
	Correct  json.RawMessage `json:"-"`
	Points   float64         `json:"points"`
	Source   string          `json:"source"`
}

// QuizPatch carries the optional PATCH /quizzes/:id mutations; nil means
// "leave unchanged". Only draft-safe settings live here - windows, duration,
// and guardrails are set at publish time (Milestone 3).
type QuizPatch struct {
	Title            *string
	MaxAttempts      *int
	ShuffleQuestions *bool
}

// Service owns quiz and question authoring. All multi-statement writes use
// explicit transactions and append their audit row inside the same
// transaction.
type Service struct {
	db  *sql.DB
	log *slog.Logger
	// jobs is an insert-only River client (no queues, no workers): publish
	// uses it to enqueue the open_quiz/close_quiz transitions inside its own
	// transaction. The worker process consumes them (internal/worker).
	jobs *river.Client[*sql.Tx]
}

// NewService wires the quiz authoring service.
func NewService(db *sql.DB, log *slog.Logger) *Service {
	jobs, err := river.NewClient(riverdatabasesql.New(db), &river.Config{})
	if err != nil {
		// The empty config is statically valid; NewClient has nothing left
		// to reject, so this cannot happen at runtime.
		panic(fmt.Sprintf("build insert-only river client: %v", err))
	}
	return &Service{db: db, log: log, jobs: jobs}
}

const quizColumns = `id, owner_id, title, status, starts_at, ends_at, duration_sec,
	max_attempts, shuffle_questions, published_at, version, created_at,
	release_policy, results_released_at`

func scanQuiz(scan func(dest ...any) error) (Quiz, error) {
	var q Quiz
	err := scan(&q.ID, &q.OwnerID, &q.Title, &q.Status, &q.StartsAt, &q.EndsAt,
		&q.DurationSec, &q.MaxAttempts, &q.ShuffleQuestions, &q.PublishedAt,
		&q.Version, &q.CreatedAt, &q.ReleasePolicy, &q.ResultsReleasedAt)
	return q, err
}

// CreateQuiz opens a new draft owned by the acting teacher
// (docs/04-api.md: POST /quizzes).
func (s *Service) CreateQuiz(ctx context.Context, actor authusers.User, title string) (Quiz, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Quiz{}, fmt.Errorf("begin create-quiz tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	q, err := scanQuiz(tx.QueryRowContext(ctx,
		`INSERT INTO quizzes (owner_id, title) VALUES ($1, $2)
		 RETURNING `+quizColumns, actor.ID, title).Scan)
	if err != nil {
		return Quiz{}, fmt.Errorf("insert quiz: %w", err)
	}
	if err := audit.Write(ctx, tx, actor.ID, "quizzes.created", "quiz", q.ID,
		map[string]any{"title": title}); err != nil {
		return Quiz{}, err
	}
	if err := tx.Commit(); err != nil {
		return Quiz{}, fmt.Errorf("commit create quiz: %w", err)
	}
	return q, nil
}

// ListQuizzes returns the actor's own quizzes newest-first with question
// counts - the authoring workspace's quiz table reads from this.
func (s *Service) ListQuizzes(ctx context.Context, actor authusers.User) ([]Quiz, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+prefixColumns("z", quizColumns)+`, count(q.id)
		 FROM quizzes z LEFT JOIN questions q ON q.quiz_id = z.id
		 WHERE z.owner_id = $1
		 GROUP BY z.id ORDER BY z.created_at DESC, z.id`, actor.ID)
	if err != nil {
		return nil, fmt.Errorf("list quizzes: %w", err)
	}
	defer rows.Close()

	now := time.Now()
	quizzes := []Quiz{}
	for rows.Next() {
		var q Quiz
		if err := rows.Scan(&q.ID, &q.OwnerID, &q.Title, &q.Status, &q.StartsAt,
			&q.EndsAt, &q.DurationSec, &q.MaxAttempts, &q.ShuffleQuestions,
			&q.PublishedAt, &q.Version, &q.CreatedAt, &q.ReleasePolicy,
			&q.ResultsReleasedAt, &q.QuestionCount); err != nil {
			return nil, fmt.Errorf("scan quiz: %w", err)
		}
		q.Status = effectiveStatus(q.Status, q.StartsAt, q.EndsAt, now)
		quizzes = append(quizzes, q)
	}
	return quizzes, rows.Err()
}

// GetQuiz loads one quiz with its full question list in position order.
// Non-owners get ErrNotFound, never a 403, so existence is not leaked.
func (s *Service) GetQuiz(ctx context.Context, actor authusers.User, id string) (Quiz, []Question, error) {
	q, err := scanQuiz(s.db.QueryRowContext(ctx,
		`SELECT `+quizColumns+` FROM quizzes WHERE id = $1`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Quiz{}, nil, ErrNotFound
	}
	if err != nil {
		return Quiz{}, nil, fmt.Errorf("load quiz: %w", err)
	}
	if !authusers.Can(actor, authusers.ActionQuizEdit, authusers.Resource{OwnerID: q.OwnerID}) {
		return Quiz{}, nil, ErrNotFound
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+questionColumns+` FROM questions
		 WHERE quiz_id = $1 ORDER BY position`, id)
	if err != nil {
		return Quiz{}, nil, fmt.Errorf("load questions: %w", err)
	}
	defer rows.Close()

	questions := []Question{}
	for rows.Next() {
		qu, err := scanQuestion(rows.Scan)
		if err != nil {
			return Quiz{}, nil, fmt.Errorf("scan question: %w", err)
		}
		questions = append(questions, qu)
	}
	q.QuestionCount = len(questions)
	q.Status = effectiveStatus(q.Status, q.StartsAt, q.EndsAt, time.Now())
	return q, questions, rows.Err()
}

// UpdateQuiz applies draft-only settings edits (docs/12 Milestone 2:
// "quiz draft CRUD").
func (s *Service) UpdateQuiz(ctx context.Context, actor authusers.User, id string, patch QuizPatch) (Quiz, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Quiz{}, fmt.Errorf("begin update-quiz tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	q, err := s.draftForUpdate(ctx, tx, actor, id)
	if err != nil {
		return Quiz{}, err
	}

	changes := map[string]any{}
	if patch.Title != nil && *patch.Title != q.Title {
		q.Title = *patch.Title
		changes["title"] = *patch.Title
	}
	if patch.MaxAttempts != nil && *patch.MaxAttempts != q.MaxAttempts {
		q.MaxAttempts = *patch.MaxAttempts
		changes["max_attempts"] = *patch.MaxAttempts
	}
	if patch.ShuffleQuestions != nil && *patch.ShuffleQuestions != q.ShuffleQuestions {
		q.ShuffleQuestions = *patch.ShuffleQuestions
		changes["shuffle_questions"] = *patch.ShuffleQuestions
	}
	if len(changes) == 0 {
		return q, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE quizzes SET title = $1, max_attempts = $2, shuffle_questions = $3
		 WHERE id = $4`, q.Title, q.MaxAttempts, q.ShuffleQuestions, id); err != nil {
		return Quiz{}, fmt.Errorf("update quiz: %w", err)
	}
	if err := audit.Write(ctx, tx, actor.ID, "quizzes.updated", "quiz", id, changes); err != nil {
		return Quiz{}, err
	}
	if err := tx.Commit(); err != nil {
		return Quiz{}, fmt.Errorf("commit update quiz: %w", err)
	}
	return q, nil
}

// DeleteQuiz removes a draft and its questions (ON DELETE CASCADE). Quizzes
// that ever left draft are never deleted - attempts may reference them; the
// archive status (Milestone 3) is their retirement path.
func (s *Service) DeleteQuiz(ctx context.Context, actor authusers.User, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete-quiz tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	q, err := s.draftForUpdate(ctx, tx, actor, id)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM quizzes WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete quiz: %w", err)
	}
	if err := audit.Write(ctx, tx, actor.ID, "quizzes.deleted", "quiz", id,
		map[string]any{"title": q.Title}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete quiz: %w", err)
	}
	return nil
}

const questionColumns = `id, quiz_id, position, type, body, options, correct, points::float8, source`

func scanQuestion(scan func(dest ...any) error) (Question, error) {
	var q Question
	// options is scanned via a plain []byte: database/sql refuses to store a
	// NULL into the named json.RawMessage type, and options is NULL for
	// truefalse and short questions.
	var body, options, correct []byte
	err := scan(&q.ID, &q.QuizID, &q.Position, &q.Type, &body, &options,
		&correct, &q.Points, &q.Source)
	q.Body, q.Options, q.Correct = body, options, correct
	return q, err
}

// AddQuestion appends a validated question to a draft quiz. Position is
// assigned under the quiz row lock, so concurrent adds cannot collide.
func (s *Service) AddQuestion(ctx context.Context, actor authusers.User, quizID string, in QuestionInput) (Question, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Question{}, fmt.Errorf("begin add-question tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := s.draftForUpdate(ctx, tx, actor, quizID); err != nil {
		return Question{}, err
	}

	q, err := scanQuestion(tx.QueryRowContext(ctx,
		`INSERT INTO questions (quiz_id, position, type, body, options, correct, points)
		 SELECT $1, coalesce(max(position), 0) + 1,
		        $2::question_type, $3::jsonb, $4::jsonb, $5::jsonb, $6::numeric
		 FROM questions WHERE quiz_id = $1
		 RETURNING `+questionColumns,
		quizID, in.Type, in.Body, nullableJSON(in.Options), in.Correct, in.points).Scan)
	if err != nil {
		return Question{}, fmt.Errorf("insert question: %w", err)
	}
	if err := audit.Write(ctx, tx, actor.ID, "questions.created", "question", q.ID,
		map[string]any{"quiz_id": quizID, "type": in.Type}); err != nil {
		return Question{}, err
	}
	if err := tx.Commit(); err != nil {
		return Question{}, fmt.Errorf("commit add question: %w", err)
	}
	return q, nil
}

// UpdateQuestion replaces a question's content while its quiz is a draft.
// The whole content arrives every time (the editor autosaves full question
// state), so validation always sees a complete, consistent question.
func (s *Service) UpdateQuestion(ctx context.Context, actor authusers.User, questionID string, in QuestionInput) (Question, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Question{}, fmt.Errorf("begin update-question tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := s.questionQuizForUpdate(ctx, tx, actor, questionID); err != nil {
		return Question{}, err
	}

	q, err := scanQuestion(tx.QueryRowContext(ctx,
		`UPDATE questions SET type = $1, body = $2, options = $3, correct = $4, points = $5
		 WHERE id = $6
		 RETURNING `+questionColumns,
		in.Type, in.Body, nullableJSON(in.Options), in.Correct, in.points, questionID).Scan)
	if err != nil {
		return Question{}, fmt.Errorf("update question: %w", err)
	}
	if err := audit.Write(ctx, tx, actor.ID, "questions.updated", "question", questionID,
		map[string]any{"quiz_id": q.QuizID, "type": in.Type}); err != nil {
		return Question{}, err
	}
	if err := tx.Commit(); err != nil {
		return Question{}, fmt.Errorf("commit update question: %w", err)
	}
	return q, nil
}

// DeleteQuestion removes a question from a draft quiz and re-densifies the
// remaining positions, preserving the dense-integer invariant
// (docs/07 section 1).
func (s *Service) DeleteQuestion(ctx context.Context, actor authusers.User, questionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete-question tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	quizID, err := s.questionQuizForUpdate(ctx, tx, actor, questionID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM questions WHERE id = $1`, questionID); err != nil {
		return fmt.Errorf("delete question: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE questions q SET position = d.dense
		 FROM (SELECT id, row_number() OVER (ORDER BY position) AS dense
		       FROM questions WHERE quiz_id = $1) d
		 WHERE q.id = d.id AND q.position <> d.dense`, quizID); err != nil {
		return fmt.Errorf("re-densify positions: %w", err)
	}
	if err := audit.Write(ctx, tx, actor.ID, "questions.deleted", "question", questionID,
		map[string]any{"quiz_id": quizID}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete question: %w", err)
	}
	return nil
}

// ReorderQuestions rewrites positions to match ids exactly
// (docs/04-api.md: PUT /quizzes/:id/questions/order). The list must be a
// permutation of the quiz's question ids; anything else changes nothing and
// returns ErrBadOrder.
func (s *Service) ReorderQuestions(ctx context.Context, actor authusers.User, quizID string, ids []string) ([]Question, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin reorder tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := s.draftForUpdate(ctx, tx, actor, quizID); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM questions WHERE quiz_id = $1`, quizID)
	if err != nil {
		return nil, fmt.Errorf("load question ids: %w", err)
	}
	existing := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan question id: %w", err)
		}
		existing[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(ids) != len(existing) {
		return nil, ErrBadOrder
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !existing[id] || seen[id] {
			return nil, ErrBadOrder
		}
		seen[id] = true
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE questions q SET position = o.ord
		 FROM (SELECT id, ordinality AS ord
		       FROM unnest($2::uuid[]) WITH ORDINALITY AS t(id)) o
		 WHERE q.id = o.id AND q.quiz_id = $1`,
		quizID, uuidArray(ids)); err != nil {
		return nil, fmt.Errorf("rewrite positions: %w", err)
	}
	if err := audit.Write(ctx, tx, actor.ID, "questions.reordered", "quiz", quizID,
		map[string]any{"question_count": len(ids)}); err != nil {
		return nil, err
	}

	qrows, err := tx.QueryContext(ctx,
		`SELECT `+questionColumns+` FROM questions
		 WHERE quiz_id = $1 ORDER BY position`, quizID)
	if err != nil {
		return nil, fmt.Errorf("reload questions: %w", err)
	}
	defer qrows.Close()
	questions := []Question{}
	for qrows.Next() {
		q, err := scanQuestion(qrows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan question: %w", err)
		}
		questions = append(questions, q)
	}
	if err := qrows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit reorder: %w", err)
	}
	return questions, nil
}

// draftForUpdate locks the quiz row and verifies the actor may edit it and
// that it is still a draft. Ownership failures read as ErrNotFound so quiz
// existence never leaks to non-owners.
func (s *Service) draftForUpdate(ctx context.Context, tx *sql.Tx, actor authusers.User, id string) (Quiz, error) {
	q, err := s.ownedForUpdate(ctx, tx, actor, id)
	if err != nil {
		return Quiz{}, err
	}
	if q.Status != "draft" {
		return Quiz{}, ErrNotEditable
	}
	return q, nil
}

// questionQuizForUpdate resolves a question to its quiz, locks the quiz row,
// and runs the same ownership + draft checks as draftForUpdate. It returns
// the quiz id for follow-up statements.
func (s *Service) questionQuizForUpdate(ctx context.Context, tx *sql.Tx, actor authusers.User, questionID string) (string, error) {
	var quizID string
	err := tx.QueryRowContext(ctx,
		`SELECT quiz_id FROM questions WHERE id = $1`, questionID).Scan(&quizID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("resolve question: %w", err)
	}
	if _, err := s.draftForUpdate(ctx, tx, actor, quizID); err != nil {
		return "", err
	}
	return quizID, nil
}

// prefixColumns qualifies a comma-separated column list with a table alias
// for use in joins.
func prefixColumns(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// nullableJSON maps an absent JSON value to SQL NULL; questions of type
// truefalse and short store no options row.
func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

// uuidArray renders ids as a Postgres array literal for $n::uuid[] casts.
// Ids are uuid-shape-validated at the HTTP layer, so the literal is safe.
func uuidArray(ids []string) string {
	return "{" + strings.Join(ids, ",") + "}"
}
