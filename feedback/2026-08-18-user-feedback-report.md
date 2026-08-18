# MacQuiz User Feedback Report - 18 Aug 2026

Source: Google Form "MacQuiz Feedback" (created via `scripts/feedback-form.gs`), 18 responses collected 14:21-16:12 on 18 Aug 2026 from test users/students.

## Summary

- Average happiness: **4.06 / 5** (median 4).
- Distribution: 5★ ×6, 4★ ×8, 3★ ×2, 2★ ×1 (one response omitted the required scale export cleanly; counted from raw rows).
- Overall sentiment is positive, but one issue dominates the free text: **authentication failing repeatedly during a live quiz session**.

## Area votes (checkbox question)

| Area | Votes |
|---|---|
| Login and account management | 8 |
| Results and analytics dashboards | 7 |
| Look and feel of the interface | 4 |
| Live monitoring during a quiz | 3 |
| Creating and scheduling quizzes | 3 |
| Taking a quiz (timer, navigation, submitting) | 1 |
| Mobile / phone experience | 1 |
| Speed and reliability | 1 |
| Session timeout (written in via "Other") | 1 |

## Findings, ranked

### P0 - Repeated "authentication failed" during an active quiz session - FIXED 18 Aug 2026

Root cause: the access token lives 15 minutes but the SPA only refreshed it once at page load, so every API call after minute 15 returned 401 until a manual reload.
Fix: the API client now refreshes the session and replays the request on any 401 (single-flight per tab, Web-Lock-serialized across tabs), the auth provider refreshes proactively every 10 minutes (keeps WebSocket reconnects working), and the server treats a refresh-token replay within 30 seconds of rotation as a same-browser race instead of theft, so concurrent tabs can no longer revoke the whole session.
Verified end to end with a 45-second token build: expiry produced the reported 401, and the fixed client rode through it transparently (401 -> refresh 200 -> replay 200, no logout).

The single biggest complaint, reported independently by at least 5 users:

- "please improve the authentication problem during the session"
- "the main fault that the authentication faild between the quizze again and again"
- "improve the running session because the authentication failed appears several times"
- "Login credentials failure"
- "Session time out problem."


### P1 - Bug: cannot clear a selected answer - FIXED 18 Aug 2026

- "clear selection are not found so i can not remove the selected one"

A "Clear selection" control now appears under any answered choice question.
It autosaves a null that the server stores as a true blank, so a cleared question is never graded (or negative-marked) as a wrong commitment, and the sidebar cell returns to unanswered.

### P1 - Results and analytics dashboards (7 votes)

Second-most-voted area, though free text gave no specifics.
Worth a short follow-up with the teachers on what exactly feels lacking (per-question breakdown, export, student-facing results view).

### P2 - Proctoring / live monitoring

- "proctored and proper monitoring of the candidate" (plus 3 area votes)

Guardrail signals exist server-side; the ask is for stronger, more visible proctoring for teachers during a live quiz.

### P2 - Look and feel / UI polish (4 votes)

- "User interface", "Look and feel of the interface", "Animations"

No specific defect named; general polish and motion-design pass requested.

### P3 - Feature requests (single mentions)

- Dark mode.
- Leaderboard and progress tracking to motivate repeat quiz-taking.
- More interactive question types: image-based and scenario-based questions.
- More/clearer instructions for users before or during a quiz.

## Recommended action order

1. ~~Reproduce and fix the mid-attempt authentication failure (P0)~~ - done 18 Aug 2026, see the P0 section.
2. ~~Add a clear-selection control in the attempt player (P1)~~ - done 18 Aug 2026.
3. Interview 2-3 teachers about what the analytics dashboards are missing (P1).
4. Scope proctoring/monitoring improvements and a UI polish pass (P2).
5. Backlog the P3 feature requests (dark mode, leaderboard, new question types, instructions).

## Notes on data quality

- Two responses put area-style text ("Session time out problem.", ".") in the wrong field via the "Other" option; they were folded into the analysis above.
- One contact field contained junk ("zwasexdcrfgtvbhynju"); contactable respondents: akshatmishra2904@gmail.com, devpathak123devpathak@gmail.com, cs23vivek.s@rbmi.in, cs23rajeev@rbmi.in, akkuthakur7302@gmail.com, kjai08647@gmail.com.
