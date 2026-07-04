# MacQuiz v2 - Security

Source: SDD-001 v2.0 sections 3, 10, 14.
Status: implementation baseline.

## 1. Authentication

- Short-lived JWT access tokens (15 min) plus rotating refresh tokens in httpOnly cookies.
- First login with an admin-issued credential forces a password reset.
- Password hashing: Argon2id.
- Single active session per student during attempts: starting an attempt from a second device invalidates the first socket and is logged as an event the teacher can see.

## 2. Authorization

- Central `can(actor, action, resource)` policy invoked by every handler and by the realtime gateway at channel subscribe.
- Ownership checks (quiz owner, attempt owner, assignment) live in the policy, not in handlers.
- 404-not-403 for unassigned resources; an unassigned student can never learn a quiz exists.
- Admin cannot author quizzes; this keeps the audit story clean.

## 3. Answer-key isolation

- The `questions.correct` field is stripped by the serializer for any student-facing response.
- Results endpoints expose it only after the quiz closes and the teacher releases results.
- Enforced at the serialization layer so no new endpoint can leak it by accident.
- Test requirement: a serializer-level test asserting `correct` is absent from every student-role response shape.

## 4. Anti-abuse during attempts

- Rate limits on answer saves per attempt.
- Kick and readmit rate-limited per teacher; both require a non-empty reason.
- Login rate limits per IP and per account.

## 5. Guardrail trust model (v2)

- Violation reports originate in the client and are spoofable in both directions.
- They are stored as evidence and surfaced to the teacher, never used as a sole automatic trigger beyond the configured ladder.
- Enforcement actions (kick, auto-submit) are server-side status flips that hold regardless of what the client does or fails to report.

## 6. Kick integrity (v2)

- The kick endpoint requires quiz ownership or admin role and a non-empty reason.
- The terminated attempt, its answers, and the reason are immutable after the fact; a kick can be reviewed but never silently rewritten.

## 7. Auditability

- Every admin or teacher mutation lands in `audit_log` with actor, action, resource, and diff.
- `attempt_events` provides the same trail for students.
- Both tables are append-only (no UPDATE/DELETE grants).

## 8. Transport and storage

- TLS everywhere (Cloudflare edge + Caddy/Let's Encrypt at origin).
- Encryption at rest.
- Pre-signed upload URLs scoped to content type and size.
- PII limited to name and email.

## 9. Infrastructure hardening (from DEP-001)

- Only ports 80/443 open to the world, restricted to Cloudflare IPs.
- SSH on a non-standard port, key-only auth.
- Postgres and Redis never exposed; they live on the private Compose network.
