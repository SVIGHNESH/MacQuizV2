# 13. Landing design language: "The Question Paper"

This is the design system of the signed-out landing page (`web/src/screens/LandingScreen.tsx` + the landing block of `web/src/App.css`).
It is a marketing-surface dialect of the core app system (`docs/11-frontend-design-system.md`): it inherits every color token from `web/src/styles/tokens.css` and layers an editorial voice on top.
Use this document to build any new page in the same style (about page, docs page, results poster, printable reports).

## 1. The concept

Every visual decision imitates one artifact: **an engineering-college examination paper**.

- The page is paper: warm off-white, faint ruled lines, hairline and double-rule borders.
- Headlines are typeset in a bookish serif, like a printed question booklet.
- All metadata (codes, clocks, marks, section labels) is monospaced, like a paper's header block.
- The one dark surface is the proctor's console (the "dark island" the core system reserves for the leaderboard).
- Red appears only as the answer-booklet margin line and as penalty/violation evidence.
- Blue is spent exclusively on interaction and emphasis, exactly as in the core system.

When adding anything new, ask: "what would this element be on a real question paper?"
If it has no answer (a glassy gradient orb, a floating 3D blob), it does not belong.

## 2. Fonts

Three families, three jobs. Never swap their roles.

| Family | Token | Job |
| --- | --- | --- |
| Fraunces Variable (serif) | `--font-display` | Headlines, section titles, card titles, names being honored |
| IBM Plex Mono | `--font-mono` | ALL metadata: eyebrows, codes, clocks, buttons, nav links, tags, footnotes |
| Inter Variable | `--font-sans` | Body copy, list items, anything longer than one line |

Imports (already done once in `LandingScreen.tsx`; a new standalone page must repeat them):

```ts
import '@fontsource-variable/fraunces'
import '@fontsource-variable/fraunces/wght-italic.css'
import '@fontsource/ibm-plex-mono/400.css'
import '@fontsource/ibm-plex-mono/500.css'
import '@fontsource/ibm-plex-mono/600.css'
```

Serif rules:

- Weights 440 to 590 only. Fraunces gets muddy when bolder; hierarchy comes from size, not weight.
- Always set `font-optical-sizing: auto` on display sizes.
- The accent move: one italic phrase inside a roman headline, colored `--color-primary`.
  Markup: `<em className="landing-title-accent">`.
- Tracking: `-0.02em` at hero size, `-0.015em` at section size.

Mono rules:

- Almost always uppercase with wide tracking: `letter-spacing: 0.08em` to `0.2em` (wider as the text gets smaller).
- Sizes live between 7.5px and 13px. Mono is never body text.
- Numerals that update (clocks, counts) get `font-variant-numeric: tabular-nums`.

## 3. Color

No raw hex anywhere.
Every color is a token from `tokens.css` or a `color-mix()` of tokens.
The landing scope defines these derived variables on `.landing` (reuse or re-declare them on your page root):

```css
--paper:      color-mix(in srgb, var(--color-surface) 86%, var(--color-warning-tint)); /* warm page bg */
--paper-rule: color-mix(in srgb, var(--color-ink) 6%, transparent);   /* faint ruled lines */
--ink-30:     color-mix(in srgb, var(--color-ink) 30%, transparent);  /* soft structural borders */
--margin-red: color-mix(in srgb, var(--color-danger) 38%, transparent); /* answer-booklet margin */
--stamp-ink:  color-mix(in srgb, var(--color-primary) 72%, transparent); /* rubber-stamp ink */
```

Role assignments:

- **Paper** (`--paper`): the page background. Cards sit on it in pure `--color-surface`.
- **Ink** (`--color-ink`): headlines, strong rules, the topline strip, the nav CTA button.
- **Blue** (`--color-primary`): links, primary buttons, italic accents, section tags, live progress. Interaction and emphasis only.
- **Slate ramp** (`--color-slate-900/950`): reserved for at most two dark islands per page (hero console, sign-in band).
- **Red** (`--color-danger` family): margin line, penalties, violations. Evidence, never decoration.
- **Green** (`--color-success-dot`): live/healthy signals only (pulsing dots, submitted states).

## 4. Surfaces and rules (borders)

The border system carries the exam-paper feel; use it instead of shadows wherever possible.

- **Double rule** (opens a table or data block): `border-top: 2.5px solid var(--color-ink)` on the block, then 1px hairlines inside.
- **Header strip**: `border-top: 2px solid var(--color-ink); border-bottom: 1px solid var(--ink-30)` (see `.landing-paper-meta`, `.landing-ticker`).
- **Hairline row separators**: `1px solid var(--color-border-hairline)`.
- **Perforation** (detachable-slip feel inside cards): `1px dashed var(--color-border-input)` (see `.landing-role-head`, OMR rows).
- **Margin line**: an absolute 1.5px vertical line in `--margin-red`, offset ~90px from the left of a list (see `.landing-lifecycle::before`).
- **Ruled paper background** for hero-like areas:

```css
background: repeating-linear-gradient(to bottom, transparent 0 43px, var(--paper-rule) 43px 44px);
```

Cards: `--color-surface`, `1px solid var(--color-border-hairline)`, `border-radius: var(--radius-card)`, `box-shadow: var(--shadow-resting)`.
A card that must dominate (a credit, a callout) adds `border-left: 4px solid var(--color-primary)` (see `.landing-v2-credit`).
Dark islands use `linear-gradient(162deg, var(--color-slate-900), var(--color-slate-950))` with a `color-mix` slate border and `--shadow-floating`.

## 5. Type scale (recipes)

| Role | Recipe |
| --- | --- |
| Hero headline | `560 clamp(46px, 5.2vw, 74px)/1.02 var(--font-display)`, tracking `-0.02em` |
| Section title | `540 clamp(32px, 3.6vw, 46px)/1.08 var(--font-display)`, tracking `-0.015em` |
| Card / item title | `550 22px-27px/1.15 var(--font-display)` |
| Body lead | `450 15.5px/1.7 var(--font-sans)`, `--color-text-secondary`, max-width `56ch` |
| Body | `450 14.5px/1.65 var(--font-sans)`, max-width `58ch` |
| List item | `500-550 12.5px-14px var(--font-sans)`, `--color-text-strong` |
| Mono label (eyebrow) | `500-600 9px-10.5px/1 var(--font-mono)`, uppercase, tracking `0.14em-0.2em`, `--color-text-muted` |
| Mono value | `600 12.5px/1 var(--font-mono)`, `--color-ink` |
| Mono annotation (`[4 marks]`) | `500 10.5px/1 var(--font-mono)`, `--color-text-muted` |

Long text is always sans; titles are always serif; anything shorter than a sentence and colder than prose is mono.

## 6. Layout

- Content column: `width: min(1180px, 100%)` centered, `padding-inline: 32px`.
- Full-bleed bands (hero, sign-in island) pad with `max(32px, calc((100% - 1180px) / 2))` so their content aligns to the same column.
- Section rhythm: `padding-top: 92px` desktop, `72px` under 1020px.
- Section head: left-aligned (never centered), stacked `tag -> title -> sub` with `gap: 16px`, `margin-bottom: 44px`.
- Asymmetry is welcome: the hero is a `1.04fr / 0.96fr` split; artifacts overlap with small rotations (`rotate: 2.4deg` and `-1.7deg`) and a z-index stack.
- Sticky nav is 60px tall over a blurred paper wash; every `section[id]` sets `scroll-margin-top: 84px`.

## 7. Component inventory (classes already in `App.css`)

Reuse these classes directly; they are not scoped to landing content, only to a `.landing` ancestor.

- `landing-topline`: ink strip with org name, motto, and the live server clock (`useClock()` in `LandingScreen.tsx`, 1s interval, `en-IN`, tabular numerals).
- `landing-nav` + `landing-brand` + `landing-nav-links`: sticky paper nav; links are mono uppercase.
- `landing-btn` variants:
  - `landing-btn-primary`: blue fill, mono uppercase, lifts 2px on hover.
  - `landing-btn-ghost`: 1.5px ink outline, fills ink on hover.
  - `landing-btn-nav`: compact ink fill, turns blue on hover.
- `landing-section-head` + `landing-section-tag`: the rubber-stamped section label (blue outline chip, `rotate: -0.8deg`), lettered like a paper: "Section A", "Section B", "Appendix".
- `landing-paper-meta`: the ruled key/value strip (TIME / MAX. MARKS / ...). Use for any fact row.
- `landing-ticker`: full-width marquee between double rules; 52s linear loop, content duplicated once for the seam.
- `landing-lifecycle` / `landing-moment`: numbered Q1..Qn list with the red margin line and `[n marks]` annotations.
- `landing-role`: hall-ticket card (dashed head rule, serif title, mono index number, check list).
- `landing-check` + `landing-check-dot`: tick list (blue tinted circle, ✓).
- `landing-register`: attendance-register card (mono title over a 2px ink rule, hairline rows, mono roll numbers).
- `landing-v2-credit`: the blue-spined credit card with a serif name and mono handle link.
- `landing-omr`, `landing-console`, `landing-stamp`: the hero artifacts (OMR sheet, dark proctor console, circular rubber stamp with `mix-blend-mode: multiply`).
- `landing-signin` / `landing-band`: the dark slate full-bleed island with a blue radial glow.
- `landing-live-dot`: 7px green dot with an expanding pulse ring.
- `landing-foot`: hairline-topped mono footer.

## 8. Motion

- One orchestrated entrance per page: children rise 16px and fade in over 0.55s with `cubic-bezier(0.22, 1, 0.36, 1)`, staggered by 0.07s (`landing-rise`).
- One signature moment maximum: on the landing it is the stamp pressing down last (`landing-stamp`, scale 1.7 -> 0.93 -> 1 at 0.85s).
- Micro-interactions use `var(--motion-micro)`: color/transform on hover, cards lift 3px.
- Continuous motion is limited to the marquee and the pulse dot.
- Everything animated must sit inside `@media (prefers-reduced-motion: no-preference)`.

## 9. Voice

Copy is part of the system.

- Exam-hall vocabulary: papers are "set" and "sealed", students "sit" papers, teachers "invigilate", results "settle".
- Headlines: short roman statement + italic blue payoff ("The exam hall, *rebuilt as software.*").
- Mono strings read like paper stationery: "QUESTION PAPER № MQ-2026", "DO NOT WRITE BELOW THIS LINE", "SERVER TIME 14:32:08 IST".
- Numbers are honest and specific (mean 7.2, 96% sat the paper); never fake vanity metrics.

## 10. Hard rules

1. No raw hex; tokens and `color-mix()` of tokens only.
2. Fraunces never below 16px; Plex Mono never above 13px; Inter never in a headline.
3. Blue only where it means interaction or emphasis; red only where it means evidence.
4. At most two dark islands per page.
5. Left-aligned section heads; centered layouts break the editorial voice.
6. Decoration must survive the question: "what is this on a real exam paper?"
7. Respect `prefers-reduced-motion` for every animation.
8. All landing classes require a `.landing` ancestor; new pages in this style start from the skeleton below.

## 11. Page skeleton

```tsx
export default function AnyPage() {
  return (
    <div className="landing">
      {/* optional: <div className="landing-topline">...</div> */}
      <header className="landing-nav">...</header>
      <main>
        <section className="landing-section">
          <header className="landing-section-head">
            <span className="landing-section-tag">Section A — Your label</span>
            <h2 className="landing-section-title">
              Roman statement, <em className="landing-title-accent">italic payoff.</em>
            </h2>
            <p className="landing-section-sub">One supporting sentence in Inter.</p>
          </header>
          {/* content: registers, moments, roles, meta strips */}
        </section>
      </main>
      <footer className="landing-foot">...</footer>
    </div>
  )
}
```
