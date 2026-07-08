# MacQuiz v2 - Realtime Events and Live Tracking

Source: SDD-001 v2.0 sections 4, 10, 11.
Status: implementation baseline.

## 1. Pipeline

Every student action follows the same four hops:

1. Student client action (start, answer autosave, navigate, submit, heartbeat, violation) hits the attempt module over REST or WebSocket.
2. Attempt module persists the change, appends a row to `attempt_events`, and publishes to Redis channel `quiz:{quiz_id}:events`.
3. Realtime gateway, subscribed to the channel, fans the event out to every authorized WebSocket on `quiz:{quiz_id}:monitor`.
4. Teacher dashboard applies the event to its in-memory roster; the UI cell updates in under 1 s end to end.

Persist first, publish second.
The event row is the source of truth; the publish is best-effort delivery.

## 2. Event vocabulary

| Event | Payload | Dashboard effect |
|-------|---------|------------------|
| `attempt.started` | student_id, attempt_id, deadline_at | Row moves to "in progress", countdown starts |
| `attempt.progress` | current_question, answered_count | Progress bar and question indicator update |
| `attempt.disconnected` | last_seen_at | Row flagged amber "disconnected" (clock keeps running) |
| `attempt.reconnected` | - | Flag cleared |
| `attempt.violation` | type, duration_ms, violation_count | Amber badge increments; violation type shown on hover (v2) |
| `attempt.kicked` | kicked_by, reason | Row moves to "kicked"; student client shows lockout screen (v2) |
| `attempt.submitted` | submit_kind, answered_count | Row moves to "submitted"; summary counters update |
| `attempt.graded` | score | Score appears (visible per quiz result policy) |
| `quiz.extended` / `quiz.closed` | new ends_at | Banner to teacher and all in-progress students |

## 3. Channels and authorization

| Channel | Purpose | Subscribe policy |
|---------|---------|------------------|
| `quiz:{id}:monitor` | Teacher/admin live dashboard | Quiz owner or admin |
| `attempt:{id}` | Student's own attempt: kick delivery, quiz.extended banners, heartbeat | Attempt owner |
| `user:{id}:notify` | Assignment notifications | The user |

The gateway checks `can()` once at subscribe and revalidates on token refresh.

## 4. Consistency: snapshot + delta

On connect, the dashboard first fetches `GET /quizzes/:id/live` (current roster state materialized from `attempts` plus recent `attempt_events`), then applies streamed deltas.
Late joins and reconnects are therefore consistent; there is no missed-event drift.

Implemented as `web/src/authoring/LiveMonitorPanel.tsx`: shown on a quiz's editor whenever it reads `live`, it fetches the snapshot, opens `quiz:{id}:monitor` over `/ws/quizzes/:id/monitor`, and applies `attempt.progress`/`violation`/`kicked`/`submitted`/`graded` deltas in place.
`attempt.started` re-fetches the snapshot instead of patching, since the delta carries no question/version data and it fires only once per attempt.
The kick and readmit escalations post to the existing `/attempts/:id/kick` and `/attempts/:id/readmit` endpoints from the same roster row.
The `attempt:{id}` student-facing channel's heartbeat and disconnected-state pieces remain unimplemented, so the dashboard never shows "disconnected". `current_question` is now wired: it is the 1-based ordinal (within the pinned quiz_version's questions array) of the last question the student's autosave resolved, persisted on `attempts.current_question` and carried by both the snapshot and the `attempt.progress` delta.

## 5. Throttling and degradation

- `attempt.progress` is coalesced per student to at most 1 event per 2 s.
  A 500-student quiz peaks around 250 events/s, trivial for Redis pub/sub and a single gateway node.
- Heartbeat: the attempt WebSocket sends a heartbeat every 10 s.
  Missed heartbeats mark the student "disconnected" on the dashboard but do not pause the clock.
- If the WebSocket cannot connect (restrictive school network), the dashboard falls back to polling the snapshot endpoint every 10 s.
- Students' attempts never depend on the socket; REST autosave is the primary write path.

## 6. Roster states

A student on the dashboard is in exactly one of:
`not started`, `in progress`, `disconnected`, `submitted`, `kicked`.
Each in-progress row shows current question number, answered count, violation badge, elapsed time, and the kick control.
The admin can open the same dashboard for any quiz, read-only except for the kick escalation power.
