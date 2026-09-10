# NODAL — UI/UX SYSTEM

The design system the product application is built on: what every token means, how the
three kinds of capital are kept apart visually, what each primitive is for, and which
rules are enforced by a test rather than by a convention.

Everything here lives under `apps/web/`. The source of truth is the code; this document
exists so that the *reasons* survive, because a rule whose reason is lost is a rule
somebody deletes.

```
apps/web/src/styles.css              the import root: fonts, then the three layers below
apps/web/src/styles/tokens.css       every colour, size, duration and texture, once
apps/web/src/styles/base.css         reset, typography, focus, temperatures, motion
apps/web/src/styles/components.css   one block per primitive
apps/web/src/lib/format.ts           the number-formatting spec, on strings
apps/web/src/lib/table.ts            the table's keyboard and sort rules
apps/web/src/components/*.tsx        the primitives
apps/web/public/brand/               the mark, the compact mark, the wordmark, the favicon
```

---

## 1. The governing rule

> **A user must never lose money because of something this interface did or failed to
> say.**

Every rule below is downstream of that one. Where a rule here conflicts with an aesthetic
instinct, the rule wins. Where it conflicts with engagement, retention or "how the
competitors do it", the rule wins.

Three of the rules are enforced by `src/lib/source-scan.test.ts` and
`src/lib/honesty.test.ts`, which scan the whole source tree on every `pnpm test`:

- no floating-point conversion may touch any value anywhere in the app — not
  `Number()`, not `parseFloat`, not `parseInt`, not `.toFixed`, not `Math.*`, not
  `Intl.NumberFormat`, not `toLocaleString`, **including inside comments**, so the rule
  has no grey area to argue about;
- no money-shaped literal and no decimal literal may appear in a page or a component,
  because a hardcoded `1,234.56` is a fake balance waiting to be mistaken for a real one;
- no raw `<button>` outside `src/components/Button.tsx`, which is how the no-dead-buttons
  contract is kept structural rather than aspirational.

Those tests are why several decisions below look strange in isolation. The brand mark's
SVG coordinates are multiplied by one hundred; the two opacities in it live in CSS. Both
are consequences of "no decimal literal in interface code", and both are cheaper than the
exception would be.

---

## 2. Tokens

`src/styles/tokens.css` is the only file allowed to state a value. A component that needs
a colour reads a token; a token that does not exist is a design decision that has not
been made yet.

### 2.1 One set, two themes

Every token is declared once in `:root` with its light value, and the
`@media (prefers-color-scheme: dark)` block redeclares **the same names**. A colour that
exists only inside the dark block renders as nothing on the default theme, which is how a
figure disappears. `index.html` carries `<meta name="color-scheme" content="light dark">`
so the browser paints its own furniture — scrollbars, form controls, the canvas behind
the document — to match, and nothing arrives as a white flash on a dark screen.

The dark theme is **re-derived, not inverted**. The same hue that reads as confident on
white reads as neon on near-black, and the light accent (`#0f766e`) fails contrast on a
dark surface. So dark takes the brand mark's cyan (`#4dd9c0`). That is the one place the
two themes are allowed to disagree about hue, because a brand that fails contrast is not
a brand, it is a defect.

### 2.2 Surfaces and ink

| Token | Light | Dark | Use |
|---|---|---|---|
| `--ground` | `#fbfaf9` | `#0e1012` | the page |
| `--surface` | `#ffffff` | `#15181b` | a panel, the rail, the masthead |
| `--surface-sunken` | `#f4f2f0` | `#1b1f23` | a table header, an inline notice |
| `--surface-raised` | `#ffffff` | `#1b1f23` | a dialog or a sheet |
| `--line` | `#e4e1dd` | `#282d33` | a hairline between rows |
| `--line-strong` | `#cfcac3` | `#394047` | a division that has to be noticed |
| `--line-control` | `#8a847c` | `#6b7178` | a form control edge — **3:1 minimum** |
| `--ink` | `#1a1917` | `#e9ebed` | body text, every figure |
| `--ink-soft` | `#55514c` | `#aeb4bb` | secondary copy |
| `--ink-faint` | `#6b665f` | `#8d949c` | metadata. **The floor.** |

The neutrals are slightly warm. Warm because a pure-gray instrument panel reads as
clinical and this product is read for hours; warm only by a hair, because a beige fintech
reads as a brochure.

### 2.3 Contrast is a number

Every foreground token was measured against every surface it can land on:

| | worst case, light | worst case, dark |
|---|---|---|
| `--ink` | 15.73:1 | 13.87:1 |
| `--ink-soft` | 7.05:1 | 7.93:1 |
| `--ink-faint` | **5.10:1** | **5.41:1** |
| `--accent` on `--accent-wash` | 4.72:1 | 8.61:1 |
| `--pos` / `--neg` / `--warn` / `--info` on their washes | 5.60–6.79:1 | 7.50–8.61:1 |
| `--accent-ink` on `--accent` | 5.47:1 | 9.18:1 |
| `--line-control` on the lightest surface | 3.70:1 | 3.87:1 |

`--ink-faint` is the dimmest value permitted for anything a reader must read. **There is
no tier below it.** If a design needs one, the design is wrong.

### 2.4 One accent

`--accent` is used for exactly three things: the primary action, a link, and the focus
ring. A second accent would make the first one mean less. Deep teal on light, the mark's
cyan on dark.

Semantic colour is **direction, not sentiment**, and it never travels alone:

| Token | Meaning | Always paired with |
|---|---|---|
| `--pos` | gain, buy, success | a `+` sign or a word |
| `--neg` | loss, sell, failure | U+2212 MINUS or a word |
| `--warn` | caution, staleness, **refusal** | a word |
| `--info` | a neutral notice | a word |
| `--zero` | no direction — the same ink as ordinary text | — |

`+4.12%` and `−9.15%` are unambiguous in grayscale, in forced-colors mode, and to a
reader with deuteranopia. `4.12%` in green is not. It costs one character.

**Zero is not a direction.** `--zero` resolves to `--ink-soft`: never green, never red.

### 2.5 Type

IBM Plex Sans for text, IBM Plex Mono for every figure, self-hosted from
`@fontsource/ibm-plex-sans` and `@fontsource/ibm-plex-mono` at the three weights the
system actually uses (400, 500, 600 sans; 400, 500 mono; latin subset). Self-hosted
because the deployed Content-Security-Policy is `font-src 'self'` — a Google Fonts URL
would simply not load. A weight nothing asks for is a font file a customer downloads for
nothing.

Plex over Inter, which is what everybody ships and therefore reads as templated, and over
the geometric display faces, which have too much personality for a figure representing
somebody's money. Plex was drawn as an institutional voice, and its mono sibling has
genuinely good digits, which is the actual requirement.

| Token | px | Use |
|---|---|---|
| `--text-2xs` | 11 | column headers, chips, the as-of stamp — uppercase, `0.06em` tracked |
| `--text-xs` | 12 | dense cells, identifiers, metadata |
| `--text-sm` | 13 | default table cell, secondary copy |
| `--text-md` | 14 | body |
| `--text-lg` | 16 | panel titles, a field value |
| `--text-xl` | 22 | page titles |
| `--text-2xl` | 30 | **the one big figure on a page** |
| `--text-3xl` | 40 | hero only |

One big figure per screen. If two figures are competing to be the largest thing, the
screen has not decided what it is for.

**The rule that matters:**

```css
font-variant-numeric: tabular-nums slashed-zero;
font-feature-settings: "tnum" 1, "zero" 1;
```

applied to `.num`, `.figure`, `.mono-small`, `code`, `time` and numeric inputs. A column
of money where the digits do not align is a column where a wrong order of magnitude is
invisible. **This is a correctness feature, not a typographic preference.** The slashed
zero is there so `0` and `O` cannot be confused in a mint address.

There are exactly two pieces of tracking in the system: `--track-caps` (`0.06em`) for
small uppercase labels, and `--track-brand` (`0.12em`) for the wordmark and nothing else.

### 2.6 Space, radii, rows, depth

- **Space** is a 4px base: `--space-1` … `--space-12`. Every margin, padding and gap in
  the application is one of these. A 13px gap is a typo with extra steps.
- **Radii**: two. `--radius-control: 6px` and `--radius-panel: 10px`. A third radius is a
  third opinion about how soft this product is, and it does not have three.
- **Rows**: `--row-h: 40px` by default because money gets air; `--row-h-dense: 34px` for a
  market table; `--tap-min: 44px`, applied to the row itself under
  `@media (pointer: coarse)`.
- **Depth**: almost none. A hairline does the work of a shadow at a fraction of the visual
  noise. `--shadow-raised` exists for the two surfaces that genuinely float — a dialog and
  a toast. No gradients, no glow, no blobs.

### 2.7 Motion

```
--dur-fast    120ms   hover, focus, a disclosure opening
--dur-base    200ms   a panel or a route arriving
--dur-flash   600ms   the exception
--ease        cubic-bezier(0.2, 0, 0, 1)
```

Anything needing a third duration is doing something this design does not want done.

`--dur-flash` is the only animation permitted on a figure. It exists solely to say *this
value changed just now*; it is **neutral**, not green or red, because the direction is
already carried by the sign glyph; and it **decays** rather than pulses. A pulse repeats
and therefore nags. A decay states a fact once and stops.

---

## 3. The three temperatures

Nodal holds three kinds of capital that the spec forbids ever summing. The arithmetic
side of that is already solved in the data layer: there is no query hook that produces a
combined figure, so no component can render one by accident.

That does not solve the **perception** problem. A label is one word in a viewport full of
numbers, and the moment a customer is moving fast — which is exactly the moment the
distinction matters most — a label is the first thing that stops being read.

So each pot renders at its own temperature, set by a `data-temp` attribute on any
container. It cascades: a simulated agent card inside an economy page is correct and
works.

| | `real` | `economy` (default) | `simulated` |
|---|---|---|---|
| What it holds | USD and USDC. Actual money. | Credits, native assets, the creator economy. | Backtest, paper, shadow, replay. |
| Border | solid, `--line-strong` | solid, `--line` | **dashed** |
| Texture | none | none | **diagonal hatch at ~6% ink** |
| Row height | 40px | 34px | 36px |
| Motion on values | **none, ever** | flash-on-change, decaying | none |
| Marker | — | — | **a persistent SIMULATED chip** |
| Figures | as authored | as authored | `filter: grayscale(1)` |

### Why three channels and not one

Hue alone fails a colourblind reader. Texture alone fails at small sizes. A chip alone is
one word among many. All three together survive any one of those failures.

**That redundancy is the entire point and it must not be optimised away.** A future
engineer will look at the dashed border and the hatch and the chip and conclude that two
of them are redundant. They are redundant. That is deliberate.

### The rule that makes `simulated` honest

> **GHOST IS DESATURATED, NOT DIMMED.**

A simulated surface carries the same luminance contrast as the other two. Signalling
"unreal" by making text harder to read would trade an accessibility failure for a
semantic gain, and that trade is forbidden. Gain and loss remain distinguishable inside
one — by the sign glyph and by weight, not by colour.

Defence in depth: `[data-temp="simulated"] .num { filter: grayscale(1) }` means a
component that hardcodes a semantic colour still comes out gray. A component author
cannot accidentally make a replay look like a fill.

The chip takes **no prop to suppress it**. The case it exists for is a screenshot pasted
into a conversation with no surrounding context, and a suppressible marker is a marker
that will one day be suppressed.

### The rule that makes `economy` safe

> **MOTION MUST CARRY DATA.**

A flash means *this value changed just now*. A bar filling means *this proportion is what
it is*. A row re-sorting means *the ranking changed*. Motion is a channel for information;
it is never a reward. There is no confetti, no sound on a fill, no streak counter, no
toast that congratulates.

`[data-temp="real"] [data-changed="true"]` kills the animation outright. Real capital
never moves on screen.

### Reduced motion is substitution, not deletion

The flash carries information, so deleting it would remove data from the interface. Under
`prefers-reduced-motion: reduce` it is **replaced** by a static inset edge marker that
persists and then clears:

```css
@media (prefers-reduced-motion: reduce) {
  [data-changed="true"] { animation: none; box-shadow: inset 2px 0 0 0 var(--ink-soft); }
}
```

Same information, no movement. Every animation in this product degrades this way. The
global reduced-motion reset in `src/styles.css` collapses durations to `0.01ms` rather
than to zero, which keeps `transitionend` firing so nothing waiting on the event hangs.

### Forced colours

The OS discards every custom colour in `forced-colors: active`, so the temperatures have
to survive on texture and structure alone. The dashed simulated border does; the hatch and
the grayscale filter are switched off, which is exactly why the chip exists as a third
channel.

---

## 4. The five-state matrix

Every data-bearing component implements five states. A component with only a success
state is not finished.

| State | Rule | What renders it |
|---|---|---|
| **Loading** | Shape-accurate skeleton in the geometry of the final content. Never a spinner for content. Per region, so a slow panel does not block a fast one. | `Skeleton`, `SkeletonField`, or `AsyncPanel`'s `skeleton` prop |
| **Empty** | Says what would be here and how to cause it. Never an illustration with the word "Nothing". | `EmptyState` |
| **Error** | The backend's problem title and detail, the stable code, the correlation id, and a retry that is safe to press. Never a raw stack, never "Something went wrong". | `Explanation` |
| **Refused** | *Distinct from error.* A refusal is the system working. See §5. | `Refusal` |
| **Stale** | The figure stays visible and stops looking authoritative. **Never blank a number because a refresh failed.** | `AsOf` + `useStaleness` + `Figure faint` |

> **A skeleton is honest in a way a stale number is not.** Where the rules forbid implying
> a figure is current, prefer showing the skeleton.

An absent value is its own thing and is not one of the five: it renders as an em dash and
a stated reason, never as a zero and never as "N/A". A zero balance and an unreadable one
are one assertion away from each other.

---

## 5. Refusal is a first-class state

This product refuses constantly and by design: capital authority ships disabled, gates are
closed by default, eligibility is evaluated before every intent, and the risk kernel is
deterministic and unsympathetic. Most products treat a refusal as a fault. **Here a
refusal is the system working correctly, and it must look like it.**

Every refusal states four things, in this order, and the `Refusal` type makes three of
them mandatory:

1. **What was refused** — in the customer's words, not the system's.
2. **Which rule refused it** — named, with its stable code.
3. **What would change the answer** — or plainly that nothing the customer can do will.
   "Nothing you can do changes this" is a complete answer and a better one than silence.
4. **The correlation id** — always, in mono, one click from the clipboard.

Refusals use `--warn`, never `--neg`. Red is for a fault. A refusal is a boundary, and the
boundary is a feature the customer is paying for.

**A refusal is never a toast.** It is not ephemeral; it stays until the customer acts.
`Toast` documents this refusal-of-refusals in its own doc comment.

A refused capability also **never renders a zero or a dash where the figure would have
been**. A capability that is off is not a balance of nothing.

Where a whole form is gated, the pattern is to replace the form with the refusal, not to
render the form disabled. A disabled form shows a customer a machine they cannot operate
and makes them guess why.

---

## 6. Number formatting

`src/lib/format.ts`. Every figure in the product renders through it. **A number formatted
ad-hoc in a component is a defect.**

### 6.1 Why it is not `design/reference/format.ts`

The reference implementation was read and could not be adopted as written. Every rung of
its ladder converts the value to a double first — the numeric constructor, fixed-point
rounding, significant-figure rounding, the maths namespace, locale number formatting. This
repository's source guard refuses all five on sight, and each of them destroys the
exactness the ledger holds.

So **the rules were adopted and the arithmetic was rewritten.** Everything in `format.ts`
operates on decimal strings and integer strings by padding, slicing and comparing digits.
Nothing divides, rounds or converts. `money.ts` already worked this way; `format.ts`
extends it and re-uses its grouping so there is one convention for what a thousand looks
like.

### 6.2 The ladder

| Value | Renders | Note |
|---|---|---|
| absent | `—` | never `0`, never `N/A` |
| exactly `0` | `0.00` | zero is a real answer, and takes no sign |
| `< 1e-8` | `<0.00000001` | a bound, not a figure |
| `1e-8 … 1e-4` | subscript notation, 4 significant digits | threshold is **four** leading zeros |
| `1e-4 … 1` | 4 significant digits | truncated |
| `0.95 … 1.05`, stablecoins only | **3 decimals** | so a depegged USDC reads `0.999` |
| `>= 1` | 2 decimals, grouped | truncated |

**The off-by-one everyone gets wrong:** in `0.0₄52` the visible `0` is a placeholder for
the **whole** zero run. It expands to `0.000052`, not `0.0000052`. There is a test for it.

The small digit is a `<span>` at `0.8em` on the **normal baseline**, never a `<sub>`. A
`<sub>` shifts the baseline, which breaks the alignment of a numeric column — the one
thing tabular figures exist to protect. It is `aria-hidden`, and a visually-hidden span
carries the unabbreviated value, because a screen reader must never read "zero point zero
four nine seven two two".

### 6.3 Truncation, never rounding

Where the display cannot show every digit, the remaining digits are dropped. Rounding
would let a figure read *higher* than the value it stands for, and the one thing this
interface may never do is make a customer believe they have more than they have.

Every abbreviation declares itself: a `Figure` whose displayed form is not the whole value
carries `abbreviated: true`, and the component puts the exact value in `title` and in a
visually-hidden span.

### 6.4 Compact notation

`Intl`'s default is a correctness bug: two significant digits with half-expand rounding
renders `999,500` as `1M`, so a `$999,999` position and a `$1,000,000` one become the same
pixels.

Two fixes are required and both are implemented: **truncate, never round**, and cut over on
an explicit threshold. The threshold here is the unit itself (1,000 / 1,000,000 /
1,000,000,000) rather than the reference's 995 / 999,995 / 999,999,995. Those constants are
calibrated for half-expand rounding; under truncation they *understate* badly at the
boundary — 999,995 would render as `0.99M`. Truncation alone already makes `1000K`
unreachable, because 999,999 truncates to `999.9K`. **This is the one place the spec's
constant was corrected rather than copied, and the spec's stated requirement — "the output
can never read 1000K", "never round up across a threshold" — is met exactly.**

### 6.5 Counts, percentages, quantities

- **A count is never abbreviated.** A count is discrete, and a rounded count is a false
  statement about a discrete thing. `1.1K holders` cannot be checked against anything;
  `1,104 holders` can.
- **A percentage** carries two decimals below 100% and none above, and is always signed —
  with **U+2212 MINUS**, not a hyphen. A hyphen is not a minus sign. `0%` renders neutral.
  Never emit `∞%` from a zero base.
- **A quantity takes its scale from the instrument, not from the value.** An indivisible
  unit shows no decimals, because showing them implies half of one can be held. Credits
  have six.

### 6.6 Time

Absolute UTC on every snapshot, in mono: `06:22:39 UTC`, with the full instant in `title`
and rendered unrounded beside it. Relative time is permitted **only** for age, never for a
figure's currency — "updated 5 minutes ago" is not good enough for a price.

Staleness escalates on a timer: under 5s the stamp is ordinary; 5–30s it turns amber and
gains the word "ageing"; past 30s it turns red, gains the word "stale", and `Panel` adds
`.is-stale`, which takes the figures in that region faint. The timer stops once a stamp is
stale, because it cannot become anything else without a new value.

### 6.7 Identifiers

First five, ellipsis, last four: `So111…1112`. Click-to-copy, full value in `title`, full
value always reachable by assistive technology. **Never truncated inside a confirmation
step** — at the moment of an irreversible action the customer sees all of it. That is why
`Identifier` (full) and `IdentifierShort` (truncated) are two components rather than one
with a flag.

### 6.8 The one derived number

`percentOfTotal` is the only function here that computes rather than formats, and what it
computes is **geometry**: how wide a segment of a bar is drawn and how that width is
described to a screen reader. It uses `BigInt` — exact integer arithmetic, never a double
— and truncates, so a segment can never claim a larger share than it holds. The exact
figures are always in the legend beside it, unaltered. The browser formats; the backend
computes.

---

## 7. The primitives

Every one lives in `apps/web/src/components/`. No screen renders a figure, a status or a
refusal by hand.

### `Figure` — every number

```ts
kind: "money" | "units" | "percent" | "bps" | "count"
value: { decimal: string } | { base: string; scale: number } | null | undefined
signed?  compact?  stablecoin?  symbol?  absent?  big?  faint?  changed?
```

**It never takes a pre-formatted string.** A component that accepts `"$1.2M"` is a
component that will one day render a rounded number where an exact one was required.

States: present · absent (em dash + stated reason) · malformed (a stated fault, never a
number) · stale (`faint`) · loading is `Skeleton`'s job.

### `AsOf` — the timestamp on every snapshot

`at`, optional `label`, optional `frozen`. Renders `.as-of` with a `<time datetime>` in
UTC and the exact ISO instant in a `.mono-small` span beside it. Escalates through the
staleness tiers on a one-second timer. `useStaleness(at)` is exported for a caller that
needs the tier without the stamp.

### `Panel` / `PanelCard` — the temperature-aware containers

`title`, `description?`, `actions?`, `id?`, `temp?`, `asOf?`. A simulated panel renders the
chip automatically. Passing `asOf` renders the stamp and takes the panel faint when it goes
stale. `PanelCard` is the `<article class="panel-nested">` a customer acts on — one product,
one market, one agent. **Nesting stops there.**

### `DataTable` — dense, sortable, accessible

Sticky header (opt into `tall` so there is something to stick against), visually-hidden
`<caption>` that also names every risk-measure column, `scope="col"` on every header,
`aria-sort` plus a glyph, horizontal overflow inside its own focusable named container,
row height from the temperature, arrow/Home/End/PageUp/PageDown navigation and Enter to
open. Rows are focusable **only** when `onOpenRow` is supplied: a focus stop that does
nothing is a dead control with extra steps. Sorting cycles through three states, because
the third is the only way back to the order the backend returned.

`Layout.Table` remains for the pages that pass their own `<tr>` children; both scroll
inside their own container and neither widens the page.

### `SegmentedBar` — available / reserved / pending

Solid, hatched, sparse-hatched — the texture says "committed" without spending a second
hue. `role="img"` with an accessible description of the proportions; a legend carrying the
exact figures and one line saying what each segment is waiting on. **It never sums**: the
whole is a figure the caller must take from the same response the parts came from, and when
the response carries none the bar shows relative proportions and says so.

### `Skeleton` / `SkeletonField`

Shapes: `text`, `figure`, `block`, `rows`. Shape-accurate and per region. Carries a polite
live region with a written label and `aria-busy`, so a reader who cannot see the shapes
still knows something is on its way.

### `Refusal`

`what`, `rule`, `code`, `remedy`, `correlationId?`. Amber. Persistent. Never a toast. See §5.

### `Disclosure` / `DisclosureSet`

Renders strings from `lib/honesty.ts`. **Never inside a tooltip, a popover, or an accordion
that starts closed.** There is no `collapsed` prop, no `trigger` prop and no compact
variant: the shape of the component is the enforcement. A disclosure a customer has to
hover to find has not been made.

### `StatusBadge` / `SimulatedChip`

Tones `neutral | good | warn | bad | info`. **The status is a word**; the tone is a second
channel on top of it. There is deliberately no tone-only and no icon-only form. `Pill` in
`Layout.tsx` is the older name for the same component and behaves identically.

### `Dialog` / `Sheet`

Built on the native `<dialog>` element: focus trapped while open, Escape closes, the rest
of the document inert, top-layer rendering, focus restored on close. Added on top: the page
behind does not scroll, Escape is routed back through React state so the opener learns that
it closed, a backdrop click closes, and **under 768px both become a bottom sheet**, because
that is where a thumb is.

### `Toast`

`ToastProvider` + `useToast().notify(text, tone)`. **Non-financial transient notices only.**
Never a refusal — a refusal stays until the customer acts. Never a fill or any other
financial event — what happened to somebody's money belongs in the record, not in a message
that deletes itself. Never a celebration. The region is `aria-live="polite"` and never steals
focus.

### `Tabs`

WAI-ARIA tablist: roving tabindex, arrow keys wrap, Home/End jump, each panel labelled by
its tab. Tabs are not for hiding a disclosure, a refusal, or a figure a customer needs in
order to decide.

### `Field` / `FieldGrid` / `FormField`

`Field` is the read-out: a `<dt>`/`<dd>` pair inside a `<dl>`, so a screen reader says
"buying power, ten thousand dollars" rather than two unrelated strings. Every figure in the
application is inside one of these or inside a scoped table cell. `FormField` is the input:
label, hint and error bound with `aria-describedby` and `aria-invalid`, the error a
`role="alert"` rendered as text — never a red border alone, which says nothing to a reader
who cannot see the red.

### `EmptyState` (in `DataState.tsx`)

`title`, `body`, optional `action`. The original two-prop contract is unchanged.
`AsyncPanel`'s four states are unchanged too; it gained an optional `skeleton`.

### `Button` and the four shapes that live with it

`Button` keeps its typed `disabledReason` union, which makes a control that does nothing
impossible to express: it demands an action, a submit, or a stated reason for being off,
and the reason renders beside it rather than in a tooltip. `IconButton`, `CopyButton`,
`SortButton` and `TabButton` live in the same file because "the only `<button>` element in
the application" is a rule enforced by path, and each takes its action as a required prop
so none of them can be dead either.

### `AppShell`

Two shapes, chosen in JavaScript rather than hidden with CSS, so only one navigation
exists in the document at a time — a duplicate landmark that is merely invisible is still
a duplicate landmark to a screen reader.

- **768px and up**: a sticky left rail carrying every section, grouped so the three kinds
  of value stay apart (Overview · Real capital · Nodal Economy · Agents and simulation ·
  Account), with the brand lockup at its head; a sticky masthead holding the stream badge,
  the build tag, and the two actions a customer starts from.
- **Below 768px**: a masthead with a menu button and the compact mark, the full section
  list one press away in a `Sheet`, and a bottom bar with the five destinations a thumb
  reaches for at 44px each.

The grouping is the design doing the same work the disclosures do: a customer reading down
the rail is told, before they click anything, that Credits and dollars and replays live in
three different places. A single entry point would be the first step toward a single total,
and a single total across those three is true of nothing.

**What is deliberately absent**: a search field and a notification bell. There is no
endpoint behind either one, and a control that does nothing is the same defect as a button
that does nothing. They belong in the shell the day they have something to do.

### `Brand`

`Mark`, `MarkCompact`, `BrandLockup`, and `PRODUCT_NAME`. Inline SVG so `currentColor`
reaches it. **The mark is neutral in every context** — it sits above all three pots and
never adopts a temperature accent. The wordmark is live text in Plex Sans SemiBold at
`0.12em`, not an embedded `<text>` element, so it is selectable, scales with the reader's
own type size and is read correctly aloud. `public/brand/` holds the same drawings for
everything outside the application, plus a `favicon.svg` that states its colour per scheme
because a standalone SVG has no inherited `color` for `currentColor` to resolve against.

The product name is a constant in one place. "Nodal" is an internal codename and a
financial company of that name already exists, so it must never be welded into the markup
of every screen.

---

## 8. Responsive

One breakpoint changes the shape of the application: **768px**.

| Width | Shape |
|---|---|
| ≥ 768px | left rail + sticky masthead + content, content capped at 1200px |
| < 768px | single column, menu sheet, bottom bar, dialogs become bottom sheets |

Verified at **360, 375, 390, 430, 640, 767, 768, 1024, 1280 and 1440px**: zero horizontal
document overflow at every one. The only element wider than the viewport at any width is a
`<table>`, which lives inside its own scroll container.

There is deliberately **no `overflow-x: hidden` on `body`**. It would clamp `scrollWidth`
to `clientWidth` and make the end-to-end test that proves the layout reflows pass without
the layout reflowing. Overflow is prevented where it is caused: `min-width: 0` on every
flex child that can hold a long value, `overflow-wrap: anywhere` on identifiers, and a
table that scrolls inside its own container.

The shell is a flex row rather than a grid, and the reason is `position: sticky`: a sticky
grid item can only travel inside its own grid area, so a masthead in an `auto` row has
nowhere to go and simply scrolls away.

---

## 9. Accessibility

Target: **WCAG 2.2 AA, verified, not asserted.** `@axe-core/playwright` runs against every
route at the WCAG 2.1 A and AA rule sets and zero violations is the merge gate. Axe passing
is a floor, not a sign-off — keyboard-walk every new screen by hand.

- **One focus ring, everywhere, never removed.** `:focus-visible`, 2px at 2px offset, drawn
  in the accent, which holds better than 3:1 against every surface it can land on in both
  themes.
- **Every interactive target 44×44 CSS px minimum.** Where density fights this — and in a
  34px table row it does — the *row* is the target, not the text inside it, via
  `@media (pointer: coarse)`.
- **Every figure is inside a `<dl>` or a `scope`d table cell**, so a screen reader reads the
  label with the number.
- **Every table has a `<caption>`** — visually hidden is fine — that says what the table is
  and names any column that is a risk measure.
- **Full keyboard operation**, including every dialog. Focus is trapped in modals and
  restored on close; the native `<dialog>` element does both.
- **A skip link is the first tab stop** and lands on `<main id="main" tabindex="-1">`.
- **`prefers-reduced-motion` honoured by substitution**, per §3.
- **`forced-colors: active`** keeps the temperatures distinguishable via texture and border
  style, since every custom colour is discarded by the OS in that mode.
- **Live regions**: a refusal or a state change announces once via `aria-live="polite"`. A
  changing figure does **not** announce — it would make the page unusable with a screen
  reader.
- **Colour is never the only carrier**: every semantic colour is paired with a sign glyph or
  a word, and every active navigation item carries a rule down its leading edge as well as
  a tint.

---

## 10. What was adopted from `design/`, what the product goal overrode, and why

`design/WEBSITE_MASTER_GOAL.md` and `design/reference/14-ui-constraints.md` are the input
to this system. Their **rules** are adopted almost in full. Their **aesthetic** is not, and
the difference is deliberate.

### Adopted, unchanged

| From | What |
|---|---|
| PART 4 | the golden rule, and the seven prohibitions it restates |
| PART 5 | the three temperatures, the three redundant channels, "desaturated not dimmed", "motion must carry data", the cascade via a `data-temp` attribute |
| PART 7 | IBM Plex Sans and Mono; tabular figures with slashed zero on every figure; the 11/12/13/14/16/22/30/40 scale; one big figure per screen |
| PART 8 | the whole formatting ladder: em dash for absent, zero as a real answer, subscript-zero at a threshold of four with the run meaning the whole run, the stablecoin band, truncating compact notation, counts never abbreviated, signed percentages with U+2212, neutral zero, absolute UTC with the full instant, first-five-last-four identifiers with click-to-copy |
| PART 9 | colour is never the only carrier; the semantic set; the 4.6:1 floor with no tier below it |
| PART 10 | two durations and one exception; decay rather than pulse; reduced motion by substitution |
| PART 11 | 44px targets, one focus ring never removed, figures inside a `<dl>` or a scoped cell, captions on tables, keyboard operation, focus trap and restore, forced-colors, live-region policy |
| PART 12 | the five-state matrix, and "a skeleton is honest in a way a stale number is not" |
| PART 13 | refusal as a first-class state; the four-part structure; `--warn` not `--neg`; persistent, never a toast; replace a gated form rather than disabling it |
| PART 14 | the component list |
| PART 24 | the < 180KB gzipped JS budget |
| PART 26 | the definition of done |
| `14-ui-constraints.md` | A (never show / never label), B (must display), C (numeric), D (timestamps), E (staleness), G (stream never authoritative), H (accessibility), I (quality bar), J (strict TS, no unjustified `any`) |

### Overridden

**1. Dark-first → light-first with a full dark theme.**
PART 6 says "Dark-first. A light theme is a later deliverable." The product goal is a
clean, precise, credible fintech product in the spirit of Linear, Stripe, Mercury,
Robinhood, Coinbase and Kalshi — not a dark trading terminal. A customer's first
impression of a product that holds their money should not be a black screen. Both themes
are derived from one token set and switch on `prefers-color-scheme`, and the dark theme is
re-derived rather than inverted, which is what PART 6 asks of a later light theme anyway.

**2. `design/brand/tokens.css` v2 was not adopted.**
Its palette and surface treatment are the Axiom/pump.fun terminal aesthetic — high-chroma
accents on near-black, gradients and glow. This system takes its *structure* (temperatures,
semantic set, motion durations, row heights) and re-derives every value against the product
goal: warm neutrals, one deep-teal accent on light and the mark's cyan on dark, no
gradients, no glow, no blobs, at most two radii, hairlines instead of shadows.

**3. The temperature names.**
`cold` / `hot` / `ghost` became `real` / `economy` / `simulated`. The originals are
evocative but they encode a *mood*; the replacements encode *what the value actually is*,
which is the thing the reader must not get wrong. `economy` is the document default,
because most surfaces in this product are internal-economy surfaces; real capital and
simulated capital both declare themselves explicitly.

**4. Hot's amber accent.**
PART 5 gives the economy temperature an amber accent. Amber is `--warn` in this system, and
this product warns and refuses constantly — an amber accent on ordinary Credit surfaces
would make the warning colour mean nothing. The economy temperature is distinguished by
density and by the flash instead, and it is the default, so a surface that has *not* declared
itself is visibly ordinary rather than visibly amber.

**5. `design/reference/format.ts` was reimplemented, not dropped in.**
The document says "drop it in as `apps/web/src/lib/format.ts`". It could not be: every rung
of its ladder converts the value to a double, which this repository's source guard refuses
and which destroys the exactness the ledger holds. The rules were adopted; the arithmetic was
rewritten on strings. §6.1.

**6. The compact-notation thresholds.**
Adopted the rule, corrected the constant. §6.4.

**7. `<Ladder>`, `<Envelope>`, `<SplitBar>` and the `Columns` control are not built yet.**
PART 14 lists them; nothing in the current sixteen screens has data to feed them, and a
primitive with no caller is a primitive designed against an imagined API. They are named
here as work for the page-by-page rebuild.

**8. Search and notifications are not in the shell.** §7, `AppShell`.

**9. "Buy Credits" is not a shell action.**
The brief's primary actions are "Buy Credits" and "Withdraw". No endpoint in this
deployment sells Credits — that is a decision, not a bug — so the shell offers "Add funds"
and "Withdraw", both of which reach a screen that explains its own state.

---

## 11. What is left for the page-by-page rebuild

The sixteen existing screens were **not** rewritten. They were restyled by the token layer
and given temperatures where the temperature is unambiguous (Home's balance panels are
`real`; every Lab panel is `simulated`). Their logic, their copy and their disclosures are
untouched.

Still to do, per screen, against the definition of done in PART 26:

- render every figure through `Figure` rather than through `Money.tsx`, which formats
  correctly but predates the ladder in §6;
- replace one-line loading labels with shape-accurate `Skeleton` regions;
- replace the generic `Explanation` with `Refusal` wherever the backend's answer is a
  refusal rather than a fault, and delete the disabled-form pattern where one survives;
- move dense listings onto `DataTable` and give the market tables the `economy`
  temperature and the 34px row;
- put `SegmentedBar` on Home, once the backend response that carries the whole is settled;
- build `Ladder`, `Envelope` and `SplitBar` when a screen has data for them;
- add the visual-regression test PART 25 asks for — one that fails when a simulated surface
  gains chroma, or when a real-capital figure gains an animation.
