package quiz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"

	"macquiz/server/internal/apischema"
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
	// ErrImportNotReady marks a commit attempt against an import that has not
	// finished validation clean (docs/07 section 2 step 5): only an import in
	// 'ready' state may be committed. 'validating' means the worker has not
	// finished yet, 'failed' means the file has row errors to fix, and
	// 'committed' means this already happened once.
	ErrImportNotReady = errors.New("import is not ready to commit")
	// ErrAssignmentInProgress marks an audience change on a live quiz that
	// would remove a student with an in-progress attempt: interrupting a
	// live attempt must go through the explicit, audited kick control
	// instead, so every interruption is attributed and reasoned (docs/06
	// section 1's audience rule).
	ErrAssignmentInProgress = errors.New("cannot remove a student with an in-progress attempt while the quiz is live")
)

// Quiz is the authoring-facing quiz shape. Window and guardrail fields stay
// null until publish (Milestone 3). It is a direct alias to the generated
// apischema.Quiz type (api/openapi.yaml, oapi-codegen - see
// internal/apischema), so this response can never silently drift from the
// spec.
type Quiz = apischema.Quiz

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
	// Topic is the free-text taxonomy tag student_stats.topic_strengths
	// aggregates against; nil when the question is untagged.
	Topic  *string `json:"topic"`
	Source string  `json:"source"`
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
	// uploads receives a registered bulk-import file (docs/07 section 2 step
	// 2); RegisterImport writes through it before enqueueing the
	// import_validate job the worker (internal/quiz/importworker.go) later
	// reads back through the same store, and CommitImport reads it a second
	// time at commit to recover the parsed rows to insert.
	uploads ImportFileStore
	// events relays quiz.extended/quiz.closed onto Redis pub/sub after Extend
	// and ForceClose commit (docs/05 section 2). It defaults to a no-op, so
	// the service works with no realtime layer wired.
	events EventPublisher
	// email delivers the email leg of assignment-change notifications
	// (SetAssignments). It defaults to a no-op, so the service works with no
	// provider configured - the in-app user:{id}:notify channel already
	// covers the same event and never depends on this.
	email EmailSender
}

// NewService wires the quiz authoring service. An optional EventPublisher
// relays quiz.extended/quiz.closed onto Redis pub/sub after they commit;
// omitting it leaves realtime delivery a no-op.
func NewService(db *sql.DB, log *slog.Logger, uploads ImportFileStore, publishers ...EventPublisher) *Service {
	jobs, err := river.NewClient(riverdatabasesql.New(db), &river.Config{})
	if err != nil {
		// The empty config is statically valid; NewClient has nothing left
		// to reject, so this cannot happen at runtime.
		panic(fmt.Sprintf("build insert-only river client: %v", err))
	}
	return &Service{db: db, log: log, jobs: jobs, uploads: uploads, events: resolvePublisher(publishers), email: noopEmailSender{}}
}

// SetEmailSender wires the email leg of assignment-change notifications
// (email.NewResendSender in production). Mirrors the SetSnapshotCache setter
// convention elsewhere in the codebase: optional, called once at boot,
// nil-safe to omit entirely (the service keeps the no-op default and every
// assignment change still delivers over the in-app channel).
func (s *Service) SetEmailSender(e EmailSender) {
	if e != nil {
		s.email = e
	}
}

const quizColumns = `id, owner_id, title, status, starts_at, ends_at, duration_sec,
	max_attempts, shuffle_questions, published_at, version, created_at,
	release_policy, results_released_at, guardrails`

// decodeGuardrails fills q.Guardrails from the raw jsonb the guardrails column
// holds. It is NULL (nil bytes) until publish, which must leave the pointer nil
// rather than error - hence a []byte, not *json.RawMessage, per the repo's
// nullable-jsonb convention. Every read site that selects quizColumns must run
// its guardrails value through this.
func decodeGuardrails(raw []byte, q *Quiz) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &q.Guardrails); err != nil {
		return fmt.Errorf("decode guardrails: %w", err)
	}
	return nil
}

func scanQuiz(scan func(dest ...any) error) (Quiz, error) {
	var q Quiz
	var guardrailsJSON []byte
	err := scan(&q.Id, &q.OwnerId, &q.Title, &q.Status, &q.StartsAt, &q.EndsAt,
		&q.DurationSec, &q.MaxAttempts, &q.ShuffleQuestions, &q.PublishedAt,
		&q.Version, &q.CreatedAt, &q.ReleasePolicy, &q.ResultsReleasedAt,
		&guardrailsJSON)
	if err != nil {
		return q, err
	}
	return q, decodeGuardrails(guardrailsJSON, &q)
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
	if err := audit.Write(ctx, tx, actor.ID, "quizzes.created", "quiz", q.Id.String(),
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
		var guardrailsJSON []byte
		// quizColumns now carries guardrails, so this list scan - which appends
		// count(q.id) and therefore can't reuse scanQuiz - must read it too, or
		// the trailing count lands in the wrong column and the query 500s.
		if err := rows.Scan(&q.Id, &q.OwnerId, &q.Title, &q.Status, &q.StartsAt,
			&q.EndsAt, &q.DurationSec, &q.MaxAttempts, &q.ShuffleQuestions,
			&q.PublishedAt, &q.Version, &q.CreatedAt, &q.ReleasePolicy,
			&q.ResultsReleasedAt, &guardrailsJSON, &q.QuestionCount); err != nil {
			return nil, fmt.Errorf("scan quiz: %w", err)
		}
		if err := decodeGuardrails(guardrailsJSON, &q); err != nil {
			return nil, err
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
	if !authusers.Can(actor, authusers.ActionQuizEdit, authusers.Resource{OwnerID: q.OwnerId.String()}) {
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

const questionColumns = `id, quiz_id, position, type, body, options, correct, points::float8, topic, source`

func scanQuestion(scan func(dest ...any) error) (Question, error) {
	var q Question
	// options is scanned via a plain []byte: database/sql refuses to store a
	// NULL into the named json.RawMessage type, and options is NULL for
	// truefalse and short questions.
	var body, options, correct []byte
	err := scan(&q.ID, &q.QuizID, &q.Position, &q.Type, &body, &options,
		&correct, &q.Points, &q.Topic, &q.Source)
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
		`INSERT INTO questions (quiz_id, position, type, body, options, correct, points, topic)
		 SELECT $1, coalesce(max(position), 0) + 1,
		        $2::question_type, $3::jsonb, $4::jsonb, $5::jsonb, $6::numeric, $7
		 FROM questions WHERE quiz_id = $1
		 RETURNING `+questionColumns,
		quizID, in.Type, in.Body, nullableJSON(in.Options), in.Correct, in.points, in.topic).Scan)
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

// Import is the bulk-upload registration/status shape (docs/07 section 2).
// file_ref never leaves the service: it is an internal storage handle, not
// something a client needs.
type Import struct {
	ID          string          `json:"id"`
	QuizID      string          `json:"quiz_id"`
	UploadedBy  string          `json:"uploaded_by"`
	Status      string          `json:"status"`
	RowCount    *int            `json:"row_count"`
	ErrorReport json.RawMessage `json:"error_report,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

const importColumns = `id, quiz_id, uploaded_by, status, row_count, error_report, created_at`

func scanImport(scan func(dest ...any) error) (Import, error) {
	var imp Import
	var errorReport []byte
	err := scan(&imp.ID, &imp.QuizID, &imp.UploadedBy, &imp.Status,
		&imp.RowCount, &errorReport, &imp.CreatedAt)
	imp.ErrorReport = errorReport
	return imp, err
}

// GetImport loads one bulk-upload import by id, for the review UI to poll
// status (docs/07 section 2 step 4: "validating" resolves to "ready" or
// "failed"). Non-owners get ErrNotFound, never a 403, so existence is not
// leaked - same convention as GetQuiz. Unlike draftForUpdate this is a plain
// read: no row lock, and importable regardless of whether the quiz has since
// left draft, so a teacher can still see how a past import resolved.
func (s *Service) GetImport(ctx context.Context, actor authusers.User, importID string) (Import, error) {
	var ownerID string
	imp, err := scanImport(func(dest ...any) error {
		return s.db.QueryRowContext(ctx,
			`SELECT `+prefixColumns("imports", importColumns)+`, quizzes.owner_id
			 FROM imports JOIN quizzes ON quizzes.id = imports.quiz_id
			 WHERE imports.id = $1`, importID).Scan(append(dest, &ownerID)...)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Import{}, ErrNotFound
	}
	if err != nil {
		return Import{}, fmt.Errorf("load import: %w", err)
	}
	if !authusers.Can(actor, authusers.ActionQuizEdit, authusers.Resource{OwnerID: ownerID}) {
		return Import{}, ErrNotFound
	}
	return imp, nil
}

// RegisterImport saves a bulk-upload file for a draft quiz the actor owns,
// creates its imports row in 'validating', and enqueues the import_validate
// job the worker picks up (docs/07 section 2 steps 2-3). file has already
// been size-capped by the handler (ImportUploadStore.Save does not re-check
// it); a file that is too large surfaces as whatever error the capped
// reader produces from Save's io.Copy.
func (s *Service) RegisterImport(ctx context.Context, actor authusers.User, quizID string, file io.Reader) (Import, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Import{}, fmt.Errorf("begin register-import tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := s.draftForUpdate(ctx, tx, actor, quizID); err != nil {
		return Import{}, err
	}

	fileRef, err := s.uploads.Save(ctx, file)
	if err != nil {
		return Import{}, fmt.Errorf("save import file: %w", err)
	}

	imp, err := scanImport(tx.QueryRowContext(ctx,
		`INSERT INTO imports (quiz_id, uploaded_by, file_ref)
		 VALUES ($1, $2, $3)
		 RETURNING `+importColumns, quizID, actor.ID, fileRef).Scan)
	if err != nil {
		return Import{}, fmt.Errorf("insert import: %w", err)
	}
	if _, err := s.jobs.InsertTx(ctx, tx, ImportValidateArgs{ImportID: imp.ID}, nil); err != nil {
		return Import{}, fmt.Errorf("enqueue import validation: %w", err)
	}
	if err := audit.Write(ctx, tx, actor.ID, "imports.registered", "import", imp.ID,
		map[string]any{"quiz_id": quizID}); err != nil {
		return Import{}, err
	}
	if err := tx.Commit(); err != nil {
		return Import{}, fmt.Errorf("commit register import: %w", err)
	}
	return imp, nil
}

// CommitImport inserts every row of a validated bulk-upload file as ordinary
// questions, tagged source='import' with import_id for provenance, and
// flips the import to 'committed' (docs/07 section 2 step 5: "the commit is
// all-or-nothing"). The file was already fully validated by the worker
// (ValidateImport); CommitImport re-parses it here rather than persisting
// the parsed rows earlier, since the uploaded file is immutable once saved
// (LocalImportStorage.Save never overwrites a file_ref) so re-parsing always
// reproduces the same rows the review step showed the teacher. Everything -
// the re-parse, every row insert, and the status flip - runs inside one
// transaction under the quiz row lock, so a failure partway leaves neither a
// changed import nor any new questions.
func (s *Service) CommitImport(ctx context.Context, actor authusers.User, importID string) (Import, []Question, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Import{}, nil, fmt.Errorf("begin commit-import tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var quizID, fileRef, status string
	err = tx.QueryRowContext(ctx,
		`SELECT quiz_id, file_ref, status FROM imports WHERE id = $1 FOR UPDATE`, importID).Scan(&quizID, &fileRef, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return Import{}, nil, ErrNotFound
	}
	if err != nil {
		return Import{}, nil, fmt.Errorf("load import: %w", err)
	}

	if _, err := s.draftForUpdate(ctx, tx, actor, quizID); err != nil {
		return Import{}, nil, err
	}
	if status != "ready" {
		return Import{}, nil, ErrImportNotReady
	}

	f, err := s.uploads.Open(ctx, fileRef)
	if err != nil {
		return Import{}, nil, fmt.Errorf("reopen import file: %w", err)
	}
	defer f.Close()
	rows, rowErrors, err := ParseImportFile(f)
	if err != nil || len(rowErrors) > 0 {
		return Import{}, nil, fmt.Errorf("import file changed since it was validated ready")
	}

	var basePos int
	if err := tx.QueryRowContext(ctx,
		`SELECT coalesce(max(position), 0) FROM questions WHERE quiz_id = $1`, quizID).Scan(&basePos); err != nil {
		return Import{}, nil, fmt.Errorf("load current position: %w", err)
	}

	inserted := make([]Question, 0, len(rows))
	for i, row := range rows {
		q, err := scanQuestion(tx.QueryRowContext(ctx,
			`INSERT INTO questions (quiz_id, position, type, body, options, correct, points, topic, source, import_id)
			 VALUES ($1, $2, $3::question_type, $4::jsonb, $5::jsonb, $6::jsonb, $7::numeric, $8, 'import', $9)
			 RETURNING `+questionColumns,
			quizID, basePos+i+1, row.Input.Type, row.Input.Body, nullableJSON(row.Input.Options),
			row.Input.Correct, row.Input.points, row.Input.topic, importID).Scan)
		if err != nil {
			return Import{}, nil, fmt.Errorf("insert imported question (row %d): %w", row.Row, err)
		}
		inserted = append(inserted, q)
	}

	imp, err := scanImport(tx.QueryRowContext(ctx,
		`UPDATE imports SET status = 'committed'::import_status WHERE id = $1
		 RETURNING `+importColumns, importID).Scan)
	if err != nil {
		return Import{}, nil, fmt.Errorf("mark import committed: %w", err)
	}
	if err := audit.Write(ctx, tx, actor.ID, "imports.committed", "import", importID,
		map[string]any{"quiz_id": quizID, "question_count": len(inserted)}); err != nil {
		return Import{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return Import{}, nil, fmt.Errorf("commit commit-import tx: %w", err)
	}
	return imp, inserted, nil
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
		`UPDATE questions SET type = $1, body = $2, options = $3, correct = $4, points = $5, topic = $6
		 WHERE id = $7
		 RETURNING `+questionColumns,
		in.Type, in.Body, nullableJSON(in.Options), in.Correct, in.points, in.topic, questionID).Scan)
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
