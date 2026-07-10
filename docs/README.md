# MacQuiz v2 - Implementation Documentation

These docs are distilled from the three source HTML documents in the repo root and are the working reference for building the system.

| Source | Covers |
|--------|--------|
| `quiz-system-design.html` (SDD-001 v2.0) | Product, architecture, data model, flows, security |
| `deployment-plan.html` (DEP-001 v1.0) | $0-hosting deployment, CI/CD, ops |
| `design_system.html` (Design System v2) | UI tokens, components, voice |

## Reading order

1. [01-requirements.md](01-requirements.md) - what we are building, for whom, and the capacity numbers that size everything.
2. [02-architecture.md](02-architecture.md) - modular monolith, module boundaries, key decisions, scaling path.
3. [03-data-model.md](03-data-model.md) - authoritative schema, indexes, invariants.
4. [04-api.md](04-api.md) - REST endpoints, permission matrix, error codes, submit funnel.
5. [05-realtime-events.md](05-realtime-events.md) - event pipeline, vocabulary, channels, snapshot + delta.
6. [06-attempt-lifecycle.md](06-attempt-lifecycle.md) - quiz state machine, timing enforcement, guardrails, kick.
7. [07-authoring-imports-analytics.md](07-authoring-imports-analytics.md) - question authoring, bulk import pipeline, analytics rollups.
8. [08-security.md](08-security.md) - authn/authz, answer-key isolation, trust model, audit.
9. [09-deployment.md](09-deployment.md) - one-VM Compose stack, free-tier services, cost tiers, paying triggers.
10. [10-operations.md](10-operations.md) - backups, monitoring, alerts, runbook.
11. [11-frontend-design-system.md](11-frontend-design-system.md) - design tokens, component recipes, screen mapping.
12. [12-implementation-plan.md](12-implementation-plan.md) - milestone build order with exit criteria and the design scorecard.
13. [13-landing-design-system.md](13-landing-design-system.md) - the "question paper" design language of the signed-out landing page and other marketing-style surfaces.

## Non-negotiable invariants (the short list every contributor must know)

1. Server time is the only clock; `deadline_at` is the single timing authority.
2. `questions.correct` never reaches a student client.
3. All attempt terminations (manual, auto, forced, kicked) go through one idempotent `submit(attempt_id, kind)` funnel.
4. Every permission decision goes through `can(actor, action, resource)`; unassigned resources return 404.
5. Events and audit rows are append-only; persist first, publish second.
6. Client guardrail signals are evidence; only server-side status flips enforce anything.
7. No deploys while any quiz is live.
