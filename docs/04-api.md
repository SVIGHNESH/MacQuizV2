# MacQuiz v2 - API Specification

Source: SDD-001 v2.0 sections 3, 7, 8, 9, 13.
Status: implementation baseline.

REST, JSON, versioned under `/api/v1`.
WebSocket endpoint at `/ws` (path `/realtime` behind the gateway route).

## 1. Authorization model

Every handler and every WebSocket channel subscribe calls the single policy function `can(actor, action, resource)`.
Ownership checks (quiz owner, attempt owner, assignment) live in the policy, not scattered in handlers.
Unassigned resources return 404, not 403, so existence is never leaked.

### Permission matrix

| Capability | Admin | Teacher | Student |
|-----------|-------|---------|---------|
| Create / deactivate users | Yes | - | - |
| Create / edit quiz | - | Own only | - |
| Bulk-upload questions | - | Own quizzes | - |
| Schedule live window | - | Own quizzes | - |
| Assign quiz audience | - | Own quizzes | - |
| Take a quiz | - | - | If assigned, in window |
| Live attempt dashboard | Read-only, all | Own quizzes | - |
| Configure attempt guardrails | - | Own quizzes | - |
| Kick student from live attempt | Any quiz | Own quizzes | - |
| Re-admit a kicked student | Any quiz | Own quizzes | - |
| Student analytics | All students | Assigned students | Self only |
| Teacher analytics | All teachers | Self only | - |
| Audit log | Yes | - | - |

## 2. Endpoints

### Users and groups (admin)

| Endpoint | Purpose |
|----------|---------|
| `POST /users` | Provision a teacher or student; generates a first-login credential |
| `POST /users/import` | Bulk-provision from a CSV/XLSX roster (`role,email,full_name`); all-or-nothing, credentials returned once |
| `PATCH /users/:id` | Deactivate, reset password, edit profile, group membership |
| `POST /groups` / `PUT /groups/:id/members` | Manage cohorts |

### Quiz authoring (teacher)

| Endpoint | Purpose |
|----------|---------|
| `POST /quizzes` | Create draft quiz |
| `POST /quizzes/:id/questions` | Add a question (validated: correct answer among options, points > 0, choice types 2-8 options) |
| `PATCH /questions/:id` / `DELETE` | Edit and remove while draft |
| `PUT /quizzes/:id/questions/order` | Reorder; API rewrites dense `position` |
| `POST /quizzes/:id/imports` | Register a bulk upload; returns pre-signed URL + import id |
| `POST /imports/:id/commit` | Commit a validated import transactionally |
| `POST /quizzes/:id/publish` | Snapshot questions, set window + duration + guardrails, transition to scheduled |
| `PUT /quizzes/:id/assignments` | Set the audience (student ids and/or group ids, expanded to rows) |
| `POST /quizzes/:id/extend` | Live only: extend ends_at (audited, broadcast) |
| `POST /quizzes/:id/close` | Live only: force-close early (audited, broadcast) |
| `GET /quizzes/:id/results` | Per-student attempt/score table; owner sees scores as grading lands, before release |
| `POST /quizzes/:id/release-results` | Closed only: release scores to students (audited, idempotent); publish's `release_policy: auto` does this automatically |

Publish preconditions: at least one question, `starts_at < ends_at` and in the future, a duration, at least one assigned student.

### Student flow

| Endpoint | Purpose |
|----------|---------|
| `GET /quizzes/assigned` | Upcoming, live, and past quizzes for the caller |
| `POST /quizzes/:id/attempts` | Start an attempt; transactional check: assigned + window (server time) + attempts used < max_attempts; sets `started_at = now()`, `deadline_at = least(now() + duration, ends_at)`; returns question set WITHOUT `correct` |
| `GET /attempts/:id` | Resume: saved answers, current server time, deadline |
| `PUT /attempts/:id/answers/:qid` | Autosave one answer; idempotent upsert on (attempt_id, question_id); rejected when `now() > deadline_at + 5 s` or status is not in_progress |
| `POST /attempts/:id/submit` | Manual submit (client confirms "n unanswered" first) |
| `GET /attempts/:id/result` | Released review: score, answer key, per-question grading; 409 until the quiz's results are released |
| `POST /attempts/:id/events` | Report a guardrail violation (REST fallback for the attempt socket) |

### Moderation (teacher/admin, v2)

| Endpoint | Purpose |
|----------|---------|
| `POST /attempts/:id/kick` | Terminate a live attempt; body requires non-empty `reason`; policy: quiz owner or admin; rate-limited; audited |
| `POST /attempts/:id/readmit` | Grant a kicked student one fresh attempt inside the window; audited |

### Monitoring and analytics

| Endpoint | Purpose |
|----------|---------|
| `GET /quizzes/:id/live` | Roster snapshot for the live dashboard (materialized from attempts + recent attempt_events) |
| `GET /analytics/quizzes/:id` | Quiz stats + item analysis (from quiz_stats) |
| `GET /analytics/students/:id` | Student performance profile (teacher: assigned students only) |
| `GET /analytics/teachers/:id` | Teacher activity and outcomes (admin) |
| `GET /analytics/teachers/:id/students` | Per-student performance on that teacher's quizzes (admin or self) |
| `GET /analytics/teachers` | Every teacher's activity summary, one row each (admin) |
| `GET /analytics/students` | Every student's profile summary with cohort ids, one row each (admin) |
| `GET /audit` | Filterable audit log (admin) |

## 3. Error vocabulary (non-exhaustive, the load-bearing ones)

| Code | HTTP | Meaning |
|------|------|---------|
| `ATTEMPT_KICKED` | 409 | Write refused because the attempt was kicked; client shows the lockout screen |
| `ATTEMPT_DEADLINE_PASSED` | 409 | Autosave/submit after deadline + grace |
| `ATTEMPT_LIMIT_REACHED` | 409 | max_attempts exhausted |
| `QUIZ_NOT_LIVE` | 409 | Start outside the window |
| `QUIZ_NOT_CLOSED` | 409 | Results release before the window has ended |
| `RESULTS_NOT_RELEASED` | 409 | Results read before the quiz's release moment |
| `NOT_FOUND` | 404 | Also returned for resources the caller is not assigned to |
| `VALIDATION_FAILED` | 422 | Body-level validation with field errors |
| `RATE_LIMITED` | 429 | Includes Retry-After |

## 4. Submit funnel

All attempt terminations go through one idempotent routine `submit(attempt_id, kind)`.

| Kind | Trigger | Behavior |
|------|---------|----------|
| `manual` | Student clicks Submit | Validated against deadline; confirmed with an "n unanswered" prompt client-side |
| `auto` | `deadline_at` reached | Per-attempt timer job fires at the deadline; whatever is autosaved becomes the submission |
| `forced` | Quiz `ends_at` or teacher force-close | The close job sweeps all in-progress attempts of the quiz |
| `kicked` | Teacher/admin kick | Same funnel with kind='kicked'; see 06-attempt-lifecycle.md |

Submission enqueues a grading job.
Choice and true/false questions compare against the snapshot key; short answers use normalized exact/numeric matching.
A correct answer earns the question's marks; an answered-but-wrong one subtracts its penalty (negative marking).
Marks and penalty resolve per question at publish from the quiz's `default_points`/`default_penalty` unless the question overrides them, and are frozen in the version snapshot.
Unanswered questions score 0 and are never penalized, and the attempt total floors at zero.
Grading writes `is_correct`, `points_awarded` (which may be negative for a penalized answer), and the attempt `score`, then emits a `graded` event.

## 5. Rate limits

- Answer saves: per attempt (generous; autosave is debounced 2 s client-side).
- Kick and readmit: per teacher, to prevent kick storms.
- Login: per IP and per account.

## 6. WebSocket channels

See 05-realtime-events.md for the event vocabulary.

| Channel | Who may subscribe |
|---------|-------------------|
| `quiz:{id}:monitor` | Quiz owner or admin |
| `attempt:{id}` | The student who owns the attempt |
| `user:{id}:notify` | The user themselves |

Authorization is checked once at subscribe and revalidated on token refresh.
Starting an attempt from a second device invalidates the first socket (single active session) and is logged as an event.
