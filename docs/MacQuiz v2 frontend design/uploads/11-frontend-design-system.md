# MacQuiz v2 - Frontend Design System

Source: design_system.html ("MacQuiz Design System v2").
Status: implementation baseline for all UI work.

Philosophy: dense, calm, and data-forward.
Blue is spent only on interaction, semantic color only on state, everything else stays neutral.
All numeric UI uses tabular figures.
Brand mark: blue "M" (rect `#2563EB`, radius 56, white "M").

## 1. Color

### Neutral ramp

| Role | Hex |
|------|-----|
| ink | `#131A26` |
| text-strong | `#344054` |
| text-secondary | `#667085` |
| text-muted | `#98A2B3` |
| border-input | `#D0D5DD` |
| border-hairline | `#E9EBEF` |
| divider / well | `#F2F4F7` |
| page ground | `#F4F5F7` |
| table header | `#FAFBFC` |
| surface | `#FFFFFF` |

### Semantic trios

Each hue ships as text / tint background / tint border, so chips, banners, and badges are always built the same way.

| Meaning | Base | Hover/dot | Tint bg | Text on tint |
|---------|------|-----------|---------|--------------|
| Primary / interactive | `#2563EB` | hover `#1D4ED8` | `#EFF4FF` | `#1849A9` |
| Success / submitted | `#079455` | dot `#12B76A` | `#ECFDF3` | `#067647` |
| Warning / integrity | `#DC6803` | dot `#F79009` | `#FFFAEB` | `#93370D` |
| Danger / live / kick | `#D92D20` | hover `#B42318` | `#FEF3F2` | `#B42318` |

### Usage rules

- Blue means clickable or selected, never decoration.
- Red is shared by LIVE and destructive actions; context disambiguates.
- Amber is evidence, not judgment (violations, disconnects).
- Charts use primary plus its 40% tints `#B3CCF9` / `#C7D7FE`; semantic color only when the value has meaning.
- Dark slate ramp (`#020617` / `#0F172A` / `#1E293B` / `#94A3B8`) is reserved for the leaderboard "dark island" only; never mix slate into light screens.

## 2. Typography

- Family: Inter, self-hosted woff2, weights 400-800 (must actually be loaded, not just referenced).
- Hierarchy comes from weight steps (450/550/650/750), not size jumps.
- Negative letter-tracking above 14px.
- Stat numerals (timers like `04:38`, scores, percentages) are always `tabular-nums`.

| Level | Size / weight / tracking |
|-------|--------------------------|
| Display (masthead) | 34 / 750 / -0.025em |
| Page title | 22 / 750 / -0.02em |
| Card / entity title | 15 / 650 / -0.008em |
| Body (default UI, cells) | 13.5 / 450-600 |
| Secondary (metadata, helper) | 12.5 / 450-550, color `#667085` |
| Eyebrow / section label / table header | 10.5-11 / 650 / +0.08-0.14em, uppercase |

## 3. Shape, elevation, motion

Surfaces are defined by 1px hairlines, not drop shadows.

Radius tier (grows with element size):

- 7-8px small controls
- 9-10px buttons and inputs
- 11-12px option rows
- 14-16px cards and modals
- 999px chips (pill)

Two elevations only:

- resting: `0 1px 2px rgba(16,24,40,.04)`
- floating: `0 24px 64px -16px rgba(16,24,40,.4)`
- primary button lift: `0 1px 2px rgba(brand,0.4)`; hero CTAs may add a soft 16px glow

Motion:

- Micro-interactions: 0.12-0.15s on color/border.
- Entrances: 0.35s `cubic-bezier(0.22, 1, 0.36, 1)` rise-and-fade.
- Live indicators pulse at 2s; progress bars ease width over 0.4s.
- Everything respects `prefers-reduced-motion`.

## 4. Component recipes

### Buttons

Variants: Primary, Secondary, Commit/final, Tonal, Kick, Disabled.

- Ink (`#131A26`) marks point-of-no-return actions (review and submit, confirm steps).
- Destructive buttons start as quiet outlines and only turn solid red at the final confirmation.
- Disabled is a gray fill, never reduced opacity.

### Status chips and badges

- Lifecycle chips (Live, Scheduled, Closed, Draft): tint background + dark text, 10.5px caps.
- Integrity badge (for example `▲ 2`): tint border plus a count; hover reveals detail.
- The pulsing-dot LIVE marker is bare text, reserved for truly realtime contexts.

### Status dot + label (for table rows, lighter than chips)

States: In progress, Submitted, Disconnected (pulses), Kicked, Not started.
7px dot + 12px/600 label in the matching text color; only transient states animate.

### Inputs and selection

- One focus treatment everywhere: brand border + `0 0 0 3px rgba(37,99,235,.12)` ring.
- Selection = brand border + `#F5F8FF` wash + filled marker; never color alone.
- Includes answer option rows (A/B/C/D), toggles, checkboxes.

### Data tables (roster pattern)

- CSS-grid rows, `#FAFBFC` caps header, `#F2F4F7` hairline dividers, no zebra striping.
- Avatar-initial + name cells (for example "PN Priya Nair").
- Denominators render muted (the "10" in 8/10); inline 5px progress bars pair with a numeric label.

### Stat cards

- Value first (20-30px / 750, tabular), label under it (11px / 550, muted).
- Number is ink unless the state itself is semantic.
- At most one inverted ink card per screen: the hero number.

### Toast

- One style: ink surface, semantic 8px dot (green/blue/amber/red), top-center, auto-dismiss 3.2s.
- Copy states the consequence, not just the event ("attempt terminated, work kept for grading").

### Modal (destructive flow)

- Overlay `rgba(13,18,28,.52)` + `blur(2px)`, 16px-radius panel, floating shadow.
- Destructive actions are two-step: required reason first, red button only on the confirm step, consequences restated in a danger-tint card.
- This is the kick flow: "Confirm removal / student · quiz / Reason / [Back] [Remove student]".

### Sidebar navigation

- 224px rail, white on hairline.
- Active item: `#EFF4FF` pill + square brand dot; inactive dots stay round and gray (the marker morphs instead of adding an icon set).
- Realtime destinations may carry a pulsing LIVE tag.

### Dark island: leaderboard

- The one sanctioned dark surface, slate ramp `#020617` / `#0F172A` / `#1E293B` / `#94A3B8`.
- Ranks 1-3 get gold/silver/bronze gradient medallions; rank 1's row lifts one slate step.

## 5. Voice and principles

- Calm under pressure: the UI runs during timed exams; no decoration competes with the timer.
  Urgency is expressed once (timer goes red-tint under 2 minutes), not everywhere.
- Evidence, not judgment: integrity signals are amber and factual ("tab switch, 38 s"), never accusatory red.
  Red is reserved for the human decision (kick) and its consequences.
- Say the consequence: copy explains what the system did ("snapshot v1 frozen, 6 students notified").
- Sentence case everywhere; caps only for eyebrows.

## 6. Mapping to screens

The design system implies these UI contexts; build them from the recipes above:

| Screen | Primary recipes |
|--------|-----------------|
| Admin console (users, groups, audit, org analytics) | Sidebar, data tables, stat cards, modals |
| Teacher authoring (quiz editor, import review) | Inputs, buttons (ink commit for publish), tables for import errors |
| Teacher live monitor | Roster table with status dots, integrity badges, kick modal (two-step destructive), stat cards, LIVE markers |
| Teacher/admin analytics | Stat cards, charts (primary + tints), tables |
| Student quiz list | Lifecycle chips, cards |
| Student attempt player | Option-row selection, timer (tabular, red-tint under 2 min), guardrail warning toasts, fullscreen blocker overlay, lockout screen (danger tint, reason shown) |
| Results / leaderboard | Dark island leaderboard, stat cards, toast ("Quiz submitted - graded instantly") |

## 7. Key literal tokens for implementation

- Focus ring: `rgba(37,99,235,.12)` at 3px.
- Modal overlay: `rgba(13,18,28,.52)` + 2px blur.
- Resting shadow: `rgba(16,24,40,.04)`; floating shadow: `rgba(16,24,40,.4)`.
- Additional chart/semantic fills seen in the source: `#F04438`, `#F59E0B`, `#FDE68A`, `#FECDCA`, `#FEDF89`, `#ABEFC6`, `#7A5AF8`, `#175CD3`, `#E4E7EC`, `#E2E8F0`, `#F1F5F9`.

Implement these as CSS custom properties (or Tailwind theme tokens) in one file; components must consume tokens, never raw hex.
