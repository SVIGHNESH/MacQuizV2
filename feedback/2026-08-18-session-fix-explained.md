# The "Authentication Failed" Fix, Explained

**Date fixed:** 18 August 2026
**Who reported it:** test users, in the 18 August feedback form (5 separate reports)
**Status:** fixed, tested, and verified end to end

## What users were seeing

Students taking a quiz would suddenly start getting "authentication failed" errors partway through.
Nothing they did was wrong.
The errors kept appearing until they reloaded the page or logged in again, wasting quiz time and breaking their focus mid-exam.

## Why it happened, in plain terms

Think of logging in as being handed two things at the front desk:

- A **visitor badge** that proves who you are, valid for 15 minutes.
- A **renewal slip** that lets you swap an expired badge for a fresh one, valid for 14 days.

Every action in MacQuiz (saving an answer, moving to the next question, submitting) shows the visitor badge.
The badge only lasting 15 minutes is deliberate: if someone steals it, it is useless very quickly.
The renewal slip exists so honest users never notice the short badge life; the app is supposed to quietly swap badges behind the scenes.

Here was the bug: **the app only swapped badges once, at the moment the page loaded.**
It never renewed the badge again after that.
So exactly 15 minutes into a quiz the badge expired, and every action after that was refused with "authentication failed".
The only way out was reloading the page, because reloading triggered that one-time badge swap again.

Most quizzes last longer than 15 minutes, which is why this hit almost every serious quiz session.

## What we changed

Three things, all invisible to the user:

1. **Automatic retry.**
If any action is refused because the badge expired, the app now quietly gets a fresh badge and repeats the action on its own.
The user never sees the error; the action just works.

2. **Renewing ahead of time.**
While you are signed in, the app now renews the badge every 10 minutes, well before the 15-minute expiry.
This keeps the live connection (the one that powers real-time quiz updates) healthy too, since it also relies on a valid badge.

3. **Two tabs can no longer log each other out.**
Renewal slips are single-use: using one issues a replacement and burns the old one.
Before the fix, if two open tabs tried to renew at the same moment, the server assumed the second attempt was a thief replaying a stolen slip and killed the whole session as a safety measure.
Now the server recognizes that two attempts within 30 seconds of each other are just the same person's browser tabs racing, and it lets the session live.
A replay that arrives later than 30 seconds is still treated as theft and still kills the session, so the security protection remains.

## What this means during a quiz

- **No page reloads.** The renewal is a silent background request; the screen never refreshes or flickers.
- **No violation counts.** Violations come from things like switching tabs or leaving fullscreen; a background renewal triggers none of those signals.
- **No auto-submissions.** Nothing about renewing a badge touches the quiz timer or the submission rules.
- **No visible change at all.** A student on a 2-hour exam will simply never see "authentication failed" anymore.

The only time someone is sent back to the login screen now is when their session is genuinely gone: the account was disabled, they logged out somewhere else, or they have been away longer than 14 days.

## How we proved it works

1. We rebuilt a test version where badges expire in 45 seconds instead of 15 minutes, so the bug appears in under a minute instead of after a quarter hour.
2. We reproduced the exact failure users reported: after expiry, requests were refused.
3. With the fix in place, we watched the same moment in a real browser: the refused request was quietly retried and succeeded, the page kept working, and nobody was logged out.
4. We also simulated two tabs racing to renew at once and confirmed the session survives.
5. All automated tests pass, including a new test that covers the two-tabs scenario.

## One thing to know for the future

The 15-minute badge and the renew-on-use design are security features, not bugs.
Any future change to login, sessions, or the quiz player should keep the automatic renewal in mind: if requests start failing at exactly the 15-minute mark, this mechanism is the first place to look.
