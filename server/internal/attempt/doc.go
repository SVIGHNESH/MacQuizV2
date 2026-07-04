// Package attempt owns the attempt lifecycle: start/resume transactions,
// autosave upserts, server-authoritative deadline enforcement, guardrail
// violations, kick/readmit, and the append-only attempt_events stream.
//
// The single idempotent submit funnel lives here: manual submit, deadline
// auto-submit, force-close, and kick all pass through one
// submit(attemptID, kind) routine so deadline checks, grading triggers, event
// emission, and race resolution are written exactly once
// (docs/06-attempt-lifecycle.md).
//
// Boundary: it never edits quiz content.
package attempt
