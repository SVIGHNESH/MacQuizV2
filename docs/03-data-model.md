# MacQuiz v2 - Data Model

Source: SDD-001 v2.0 section 5.
Status: implementation baseline; this is the authoritative schema reference for migrations.

## 1. Principles

- PostgreSQL 16 is the single source of truth; Redis holds only ephemeral or derivable state.
- Quizzes snapshot their questions on publish so mid-quiz edits can never corrupt an in-flight attempt.
- Answers are one row per question for granular autosave and per-question analytics.
- All timestamps are `timestamptz` stored in UTC; rendering in the user's timezone is a client concern.
- Kicked attempts are terminal, not deleted; only new writes are refused.

## 2. Tables

### users

Every account is admin-created; `created_by` is NOT NULL.

```sql
users (
  id            uuid PRIMARY KEY,
  role          enum('admin','teacher','student'),
  email         citext UNIQUE,
  password_hash text,                -- Argon2id
  full_name     text,
  status        enum('active','disabled'),
  created_by    uuid REFERENCES users(id),  -- the admin who provisioned this account
  created_at    timestamptz
)
```

### groups and group_members

Optional cohorts, for example "Class 10-B".

```sql
groups        (id, name, created_by)
group_members (group_id FK, student_id FK, UNIQUE(group_id, student_id))
```

### quizzes

Owned by exactly one teacher; `owner_id` scopes every permission check.

```sql
quizzes (
  id                uuid PRIMARY KEY,
  owner_id          uuid REFERENCES users(id),
  title             text,
  status            enum('draft','scheduled','live','closed','archived'),
  starts_at         timestamptz,      -- go-live moment (FR-4)
  ends_at           timestamptz,      -- hard close
  duration_sec      int,              -- per-attempt time limit
  max_attempts      int DEFAULT 1,
  shuffle_questions boolean,
  guardrails        jsonb,            -- v2, snapshotted at publish:
                                      -- {fullscreen, focus_tracking, block_clipboard,
                                      --  max_violations, violation_action}
  published_at      timestamptz,
  version           int               -- bumped on republish
)
```

### questions

```sql
questions (
  id         uuid PRIMARY KEY,
  quiz_id    uuid REFERENCES quizzes(id),
  position   int,                       -- dense integer, rewritten on reorder
  type       enum('single','multi','truefalse','short'),
  body       jsonb,                     -- rich text + optional image ref
  options    jsonb,                     -- [{key, text}] for choice types
  correct    jsonb,                     -- NEVER serialized to student clients
  points     numeric DEFAULT 1,
  source     enum('manual','import'),   -- FR-2 vs FR-3 provenance
  import_id  uuid NULL REFERENCES imports(id)
)
```

### quiz_assignments

Explicit per-student visibility (FR-5).
Group assignment is expanded to individual rows at assignment time, so removing a student from a group never silently revokes an already-assigned quiz.

```sql
quiz_assignments (
  quiz_id     uuid REFERENCES quizzes(id),
  student_id  uuid REFERENCES users(id),
  assigned_by uuid REFERENCES users(id),
  assigned_at timestamptz,
  PRIMARY KEY (quiz_id, student_id)
)
```

### attempts

One row per student per try; the timing authority.

```sql
attempts (
  id              uuid PRIMARY KEY,
  quiz_id         uuid FK,
  student_id      uuid FK,
  attempt_no      int,
  quiz_version    int,               -- pins the snapshot the student saw
  started_at      timestamptz,       -- server time, set on start
  deadline_at     timestamptz,       -- min(started_at + duration, quiz.ends_at); precomputed
  submitted_at    timestamptz NULL,
  submit_kind     enum('manual','auto','forced','kicked') NULL,
  score           numeric NULL,
  status          enum('in_progress','submitted','graded','kicked'),
  violation_count int DEFAULT 0,     -- v2: guardrail violations accumulated
  kicked_by       uuid NULL FK,      -- v2: teacher/admin who removed the student
  kick_reason     text NULL,         -- v2: required when kicked; shown in audit
  UNIQUE (quiz_id, student_id, attempt_no)
)
```

`deadline_at` is precomputed at attempt start.
Every autosave and the final submit are validated against this one column: a single comparison, no clock math scattered around the codebase.

### attempt_answers

```sql
attempt_answers (
  attempt_id     uuid FK,
  question_id    uuid FK,
  response       jsonb,
  is_correct     boolean NULL,       -- filled by grader
  points_awarded numeric NULL,
  time_spent_ms  int,                -- accumulated focus time, for analytics
  saved_at       timestamptz,        -- last autosave
  PRIMARY KEY (attempt_id, question_id)
)
```

### imports

```sql
imports (
  id           uuid PRIMARY KEY,
  quiz_id      uuid FK,
  uploaded_by  uuid FK,
  file_ref     text,
  status       enum('validating','failed','ready','committed'),
  row_count    int,
  error_report jsonb
)
```

### attempt_events

Append-only; drives the realtime dashboard and doubles as replay/audit.
Because events persist, a teacher who opens the dashboard late (or the admin, after the fact) gets an accurate timeline.

```sql
attempt_events (
  id         BIGSERIAL,
  attempt_id uuid FK,
  type       text,
  payload    jsonb,
  at         timestamptz
)
```

### audit_log

```sql
audit_log (
  id            bigserial,
  actor_id      uuid,
  action        text,
  resource_type text,
  resource_id   uuid,
  detail        jsonb,   -- includes diff for mutations
  at            timestamptz
)
```

### Rollup tables (written by the analytics module, read by dashboards)

```sql
quiz_stats    (quiz_id PK, distribution jsonb, mean numeric, median numeric,
               participation numeric, item_analysis jsonb, integrity jsonb, computed_at)
student_stats (student_id PK, accuracy_trend jsonb, avg_time_per_question numeric,
               completion_rate numeric, topic_strengths jsonb, updated_at)
```

## 3. Indexing checklist

- `quiz_assignments (student_id)` for the student quiz list (one indexed join).
- `attempts (quiz_id, status)` for the live roster snapshot.
- `attempts (student_id)` for student history.
- `attempt_events (attempt_id, id)` for timeline replay.
- `questions (quiz_id, position)` for player ordering.
- `audit_log (actor_id, at)` and `audit_log (resource_type, resource_id)` for filtered audit views.

## 4. Snapshot and versioning rules

- Publish copies the question set into an immutable version and writes `quizzes.version`.
- Editing a scheduled quiz creates version n+1.
- Attempts record `quiz_version`, so results always match what the student actually saw.
- Guardrail config is snapshotted with the question set; rules cannot change under a student mid-window.

## 5. Data safety invariants (enforce in code review and tests)

1. `questions.correct` never appears in any student-facing serialization.
2. No write to `attempt_answers` or attempt submit when `now() > deadline_at + 5 s grace` or `attempts.status != 'in_progress'`.
3. Kick flips status, records kicked_by/kick_reason/submitted_at, and writes the audit row in one transaction.
4. Grading is a pure function of (snapshot, answers); reruns are safe and idempotent.
5. `audit_log` and `attempt_events` are append-only; no UPDATE or DELETE grants.
