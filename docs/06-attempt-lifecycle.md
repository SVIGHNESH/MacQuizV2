# MacQuiz v2 - Quiz and Attempt Lifecycle, Guardrails, Kick

Source: SDD-001 v2.0 sections 6, 8, 9, 10.
Status: implementation baseline.

## 1. Quiz state machine

```
Draft -> Scheduled -> Live -> Closed -> Archived
```

| Transition | Trigger | Rules |
|-----------|---------|-------|
| Draft -> Scheduled | Teacher publishes | Requires >= 1 question, valid future window (starts_at < ends_at), duration, >= 1 assigned student; snapshots questions and guardrails, writes version |
| Scheduled -> Live | Scheduler at `starts_at` | `open_quiz` job enqueued at the exact timestamp; API also treats the quiz as live on read if starts_at has passed (lazy validation) |
| Scheduled -> Draft | Teacher cancels | `POST /quizzes/:id/cancel` before the quiz opens; clears the window and unlocks the editor |
| Live -> Closed | Scheduler at `ends_at`, or teacher force-close | `close_quiz` job force-submits all open attempts (`kind='forced'`), grading runs, results release per policy |
| Closed -> Archived | Teacher archives | Read-only; analytics retained |

While Scheduled: reschedule and cancel are allowed.
Cancel returns the quiz to Draft with `starts_at`/`ends_at`/`published_at` cleared, so it drops off student dashboards and becomes editable again; the version history is append-only and the audience, duration, and guardrails survive, so the next publish is a plain version n+1 reschedule.
It is refused (409 `QUIZ_NOT_CANCELLABLE`) once the quiz has effectively opened - the gate is `status = 'scheduled' AND starts_at > now()` on the server clock, so a scheduled row whose `starts_at` has passed counts as open even before the `open_quiz` job lands; that one is force-closed, not cancelled.
Once Live: the teacher can extend `ends_at`, force-close early, kick individual students, or edit the audience (`PUT /quizzes/:id/assignments`) - adding a student is a late invite, removing one is allowed unless they have an in-progress attempt (409 `ASSIGNMENT_IN_PROGRESS`; kick is the only sanctioned way to interrupt one). All of these are audited and broadcast to connected students.

## 2. Attempt timing

Three time controls, all teacher-set at publish:

| Field | Meaning | Enforced where |
|-------|---------|----------------|
| `starts_at` | Earliest moment any assigned student can start; before it the quiz shows as "upcoming" with a countdown | `POST /attempts` rejects earlier starts; scheduler flips status |
| `ends_at` | Hard close; no new starts, all open attempts force-submitted regardless of remaining personal time | Scheduler job + deadline clamp |
| `duration_sec` | Per-attempt budget; a student starting late gets `min(duration, time-to-close)` | Precomputed into `attempts.deadline_at` |

Rules:

- `deadline_at = least(started_at + duration_sec, quiz.ends_at)`, computed once at start.
- Server time only; the client countdown is cosmetic (server deadline + clock-offset estimate).
- Autosave: debounced 2 s client-side, PUT per answer, idempotent upsert; server rejects writes where `now() > deadline_at + 5 s grace`.
- Resume: `GET /attempts/:id` returns saved answers, current server time, and the deadline.
- The disappearing student: a per-attempt timer job fires server-side at `deadline_at` and auto-submits the autosaved answers.
  This is why answers persist on every change rather than at submit time.

## 3. Guardrails (v2)

Configured per quiz before publish and snapshotted with the question set.
Each guardrail can be off, warn-only, or counted toward the violation ladder.

| Guardrail | What it does | Policy options |
|-----------|--------------|----------------|
| Fullscreen lock | Player requests fullscreen at start; leaving raises a violation and overlays a "return to fullscreen" blocker | off / warn / count |
| Focus tracking | Window blur and tab switches reported with duration (visibilitychange + blur), so the teacher sees "left the tab for 40 s" | off / warn / count |
| Clipboard and context menu block | Copy, cut, paste, right-click disabled in the player; usage attempts logged | off / on (logged) |
| Single active session | Second-device start invalidates the first socket; logged as an event | always on |
| Violation ladder | After `max_violations` counted violations, the configured action fires | flag / auto_submit / notify (default: flag at 3) |

Violation pipeline: client reports over the attempt WebSocket (REST fallback `POST /attempts/:id/events`); the attempt module increments `attempts.violation_count`, appends an `attempt_events` row, and publishes `attempt.violation`.
Student sees a warning toast ("Violation 2 of 3 - stay in the quiz window"); the teacher's roster row gains an amber badge with count and types on hover.

Trust model: browser guardrails deter and document; they do not make cheating impossible.
Violations are evidence for the teacher, never a sole automatic trigger beyond the configured ladder.
That is why the default ladder action is `flag`, and why the kick decision stays with a human.

## 4. Kick (v2)

Server-authoritative removal of a candidate from a live attempt.
Available on every in-progress roster row for the quiz owner, and to the admin on any quiz.
Deliberately a two-step UI: pick a reason (canned list plus free text), then confirm.

Sequence:

1. `POST /attempts/:id/kick {reason}`; policy check: caller owns the quiz or is admin.
2. One transaction: `status='kicked'`, `submit_kind='kicked'`, `kicked_by`, `kick_reason`, `submitted_at=now()`; audit row written.
3. Publish `attempt.kicked`; enqueue grading of whatever was autosaved.
4. Gateway delivers a lockout message to the student's `attempt:{id}` socket, then force-closes it; fans the event to the monitor channel.
5. Student client renders the removed-from-quiz screen with the reason.
   If the socket message was lost, the next autosave returns `409 ATTEMPT_KICKED` and triggers the same screen.

Invariants:

- Enforcement is the status flip, not the socket.
  Once the transaction commits, every autosave and submit is rejected by the same `status = 'in_progress'` check that guards the deadline.
- Kicked work is graded, not discarded; results are flagged `kicked` wherever scores appear, and the teacher can override the score to zero per attempt.
- Re-admission is a new attempt, not a resurrection: `POST /attempts/:id/readmit` grants one extra attempt slot (audited); the student starts fresh with whatever time remains; the kicked attempt stays in the record.
- Kick and readmit are rate-limited per teacher and always require the reason field.
- Kick reuses the idempotent `submit(attempt_id, kind='kicked')` funnel; a kick that races an auto-submit resolves cleanly to whichever committed first.
- The terminated attempt, its answers, and the reason are immutable afterwards.
