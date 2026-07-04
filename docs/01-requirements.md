# MacQuiz v2 - Requirements and Capacity Model

Source: SDD-001 v2.0 (quiz-system-design.html), sections 1, 2, 15.
Status: implementation baseline.

## 1. Product summary

MacQuiz is a closed-enrollment assessment platform.
There is no self-signup: a single admin (or admin team) creates every teacher and student account.
Teachers own the full quiz lifecycle: authoring, scheduling, audience selection, live monitoring, and post-quiz analytics.
The admin has organization-wide visibility over both teachers and students.
Version 2 adds in-attempt integrity guardrails and live moderation (the teacher can remove a candidate from a quiz mid-attempt).

## 2. Functional requirements

| ID | Requirement | Actor |
|----|-------------|-------|
| FR-1 | Create, deactivate, and manage teacher and student accounts; no self-signup exists anywhere in the system. | Admin |
| FR-2 | Create quizzes by adding questions one at a time through the authoring UI. | Teacher |
| FR-3 | Bulk-upload questions via CSV/XLSX with validation, per-row error reporting, and an all-or-nothing commit. | Teacher |
| FR-4 | Schedule a quiz with a go-live time and end time; students can start an attempt only inside that window. | Teacher |
| FR-5 | Select exactly which students a quiz is visible to (individually or by group); unassigned students never see it. | Teacher |
| FR-6 | Take an assigned quiz during its live window, with a per-attempt countdown, autosave, and auto-submit at the deadline. | Student |
| FR-7 | Watch a live dashboard of attempts on their own quiz: who started, current question, answered count, submissions, disconnects. | Teacher |
| FR-8 | View analytics for their own quizzes and assigned students: scores, distributions, per-question difficulty, time spent. | Teacher |
| FR-9 | View organization-wide analytics covering all students and all teachers (quizzes created, participation, outcomes). | Admin |
| FR-10 | Configure per-quiz attempt guardrails: fullscreen lock, tab/focus-loss tracking, clipboard blocking, and a violation ladder (warn, flag, auto-submit) enforced server-side. | Teacher |
| FR-11 | Remove (kick) a candidate from a live attempt with a recorded reason; the attempt is terminated server-side immediately, retained for audit, and optionally re-admitted. | Teacher |

## 3. Non-goals for v2

- Camera-based proctoring (webcam or screen recording).
  Browser-level guardrails only; the violation event stream leaves room for adding camera signals later.
- Subjective grading workflows (essay questions with manual marking).
  Supported auto-gradable types: single choice, multiple choice, true/false, short numeric/text answers.
- Public quiz sharing or anonymous access.

## 4. Non-functional requirements

| Concern | Target |
|---------|--------|
| Concurrency | A single quiz go-live can bring 1,000+ students online within the same minute; the platform must absorb this thundering herd. |
| Latency | Answer autosave acknowledged < 300 ms p95; live-tracking events visible to the teacher < 1 s after the student action. |
| Correctness | No attempt may be accepted after its server-computed deadline; grading is deterministic and idempotent. |
| Durability | Every saved answer survives a client crash; a student who reconnects resumes exactly where they left off. |
| Auditability | All admin and teacher mutations (user creation, quiz publication, assignment changes, kicks, re-admissions) are written to an append-only audit log. |
| Enforcement | A kick takes effect server-side in the same transaction that records it; the student's UI reflects it within 1 s over WebSocket, or on the next autosave (<= 2 s debounce) if the socket is gone; no write is accepted after the kick regardless of client state. |

## 5. Capacity estimates (back of the envelope)

These numbers size every downstream choice; do not change components without re-running them.

- Assumed peak: 2,000 concurrent students in a live window; 1,000 attempt-starts within one minute at go-live.
- Autosave writes: 2,000 students x 1 save / 10 s = ~200 writes/s, single-row primary-key upserts.
- Go-live herd: 1,000 starts over ~60 s = ~17 small transactions/s, with the question snapshot served from Redis cache (no re-serialization per start).
- Event fan-out: attempt.progress coalesced to at most 1 event / 2 s per student; a 500-student quiz peaks around 250 events/s through Redis pub/sub.
- WebSocket footprint: ~2,000 student sockets plus dashboards; roughly 0.5-1 GB RAM in one Node process.
- Grading spike at close: 2,000 attempts x ~30 questions, queue-absorbed; minutes of one worker core and latency-insensitive.
- Storage: attempt data for a full academic year fits in single-digit GB.

Conclusion: one modest machine handles this with roughly 10x headroom.
Horizontal scaling paths exist for growth, not for day one (see 02-architecture.md and 09-deployment.md).

## 6. Open product questions

Resolve these with stakeholders before or during the build; defaults are chosen so work is not blocked.

1. Results release: automatic at quiz close or explicit teacher action.
   Default: per-quiz teacher toggle, default auto-release.
2. Per-question timing in addition to per-attempt duration.
   Default: per-attempt duration only.
3. Whether email is required for provisioning, or the admin distributes printed credentials.
   Default: email optional; credentials can be generated and printed.
4. Whether a kicked student sees their result and kick reason immediately or after results release.
   Default: kick reason shown immediately on the lockout screen; score follows the quiz's release policy.
5. Whether the violation ladder needs per-guardrail thresholds.
   Default: single combined counter for v2.
