# cross-brand-identity — Sealed C and instance-brand consistency

> **Revision 2 — identity OWNER-APPROVED 2026-09-27; shell UX extension added 2026-09-30; implementation OWNER-APPROVED 2026-10-01 with documented skips; iter-0193 remains OPEN with commit pending.**
> This specification turns the approved identity in [`docs/brand-guidelines.md`](../brand-guidelines.md)
> into a runtime, asset, accessibility, responsive-shell, and truthful-rendering contract. The deployed
> SPA remains the visual baseline. Existing instance branding from D-0083 and D-0110 stays compatible:
> product name, custom accent and custom logo remain operator-controlled, while the default shield/check is
> replaced by Sealed C. The shell UX extension records the read-only audit findings; it improves access,
> context and state communication without replacing the approved data-first product language.

## 1. Purpose

cerbix currently has one instance-branding contract but several copies of its fallback identity:

- [`BrandMark.vue`](../../frontend/src/components/BrandMark.vue) owns the authenticated shell and login mark;
- [`PublicStatusView.vue`](../../frontend/src/views/PublicStatusView.vue) repeats the fallback glyph in its
  header and `Powered by cerbix` footer;
- [`docs/logo.svg`](../logo.svg), [`docs/logo.png`](../logo.png), and
  [`frontend/public/favicon.svg`](../../frontend/public/favicon.svg) carry static shield/check copies;
- [`branding.ts`](../../frontend/src/stores/branding.ts) changes the accent tile at runtime but leaves
  `--accent-ink` theme-dependent, so a configured light accent can make the glyph or action text unreadable.

The change establishes one approved default identity, one contrast rule, and one cross-surface test contract.
It is an identity-layer change, not a redesign of the product or a change to reliability semantics.

## 2. Relationship to other authorities

- [`docs/brand-guidelines.md`](../brand-guidelines.md) owns positioning, voice, visual meaning, logo usage,
  color roles, and the distinction between Editorial Proof and Operational Proof.
- [`docs/design/notes.md`](../design/notes.md) owns the SPA's layout, density, typography roles, themes,
  component grammar, and truthful rendering.
- D-0083 owns public instance branding and its DB/config/default precedence.
- D-0110 owns `logo_url` as the custom-image override used by sidebar, login, and public status.
- This specification owns the default Sealed C geometry, runtime custom-accent contrast behavior,
  surface reuse, static asset parity, compatibility, and required verification.

When these authorities overlap, truthful rendering and accessibility take precedence over decoration.

## 3. Requirements

### 3.1 Default identity

The default cerbix mark is **Sealed C**. The open C represents a measurement window; the short horizontal
bar closes the evidence boundary. The mark contains no shield, checkmark, dog, Cerberus illustration, or
operational status color.

The canonical glyph geometry is:

```svg
<svg viewBox="0 0 32 32" fill="none" stroke="currentColor"
     stroke-width="2.5" stroke-linecap="square" stroke-linejoin="round"
     aria-hidden="true">
  <path d="M21.4 9.5a8.5 8.5 0 1 0 .1 12.9" />
  <path d="M19.9 16h4.9" />
</svg>
```

The same two path values and stroke contract are used by the Vue glyph, light-context SVG, dark-context
SVG, and favicon. A consumer may change size, tile color, or glyph color but not geometry.

### 3.2 Runtime component ownership

A new presentational component named `BrandGlyph`, colocated with `BrandMark.vue`, owns the Vue copy
of the canonical paths.

Its public interface is:

```ts
interface BrandGlyphProps {
  size?: number; // default: 16 CSS pixels
}
```

Its root SVG:

- uses `viewBox="0 0 32 32"`;
- uses `currentColor`;
- is marked `aria-hidden="true"` because the adjacent wordmark or page title names the product;
- exposes `data-brand-glyph="sealed-c"` for component and browser tests.

`BrandMark.vue` remains the owner of the custom-image decision and preserves its existing `tile` and
`glyph` props. It renders:

1. the configured `logoUrl` image when one exists; or
2. an accent tile containing `BrandGlyph` when no custom image exists.

Both branches expose `data-testid="brand-mark"`. `AppShell.vue` and `LoginView.vue` continue consuming
`BrandMark`; their layout, wordmark and size choices do not change.

### 3.3 Custom accent contrast

A valid six-digit custom accent continues to override these inline properties:

- `--accent`;
- `--accent-2`;
- `--accent-weak`.

It additionally sets `--accent-ink` to whichever of the following produces the higher WCAG contrast
against the configured accent:

- light ink: `#FFFFFF`;
- dark ink: `#0B0B0F`.

The comparison uses WCAG relative luminance with sRGB linearization:

```text
channel <= 0.04045
  ? channel / 12.92
  : ((channel + 0.055) / 1.055) ^ 2.4

L = 0.2126 R + 0.7152 G + 0.0722 B
contrast = (lighter + 0.05) / (darker + 0.05)
```

A pure function named `accentInkFor(hex)` owns this choice and returns exactly `#ffffff` or `#0b0b0f`.
The branding store applies the result; UI components do not recalculate it.

Clearing or invalidating the custom accent removes all four inline properties, including `--accent-ink`,
so the active light/dark stylesheet defaults become authoritative again. The branding store therefore
applies the accent function on every load, including an empty value; it does not skip the clear path.

Custom accent never changes `--up`, `--down`, `--degraded`, `--maint`, `--pending`, or their weak variants.

### 3.4 Instance-brand state matrix

| `logo_url` | `accent_color` | Mark | Tile/action color | Glyph/action ink |
| --- | --- | --- | --- | --- |
| empty | empty | Sealed C | active theme defaults | active theme `--accent-ink` |
| empty | valid dark color | Sealed C | configured accent | `#FFFFFF` when it has higher contrast |
| empty | valid light color | Sealed C | configured accent | `#0B0B0F` when it has higher contrast |
| custom image | empty | custom image | active theme defaults for actions | active theme `--accent-ink` |
| custom image | valid color | custom image | configured accent for actions | contrast-selected `--accent-ink` |

A custom image is never tinted, cropped into the Sealed C geometry, or replaced by the fallback mark.

### 3.5 Public status reuse

The public status header uses `BrandMark` at its current visual size instead of maintaining its own
custom-image/fallback branch. This preserves custom logo behavior and removes one fallback copy.

The existing `Powered by cerbix` footer keeps its current visibility and wording. Its inline shield is
replaced by the shared `BrandGlyph` without a tile. Changing white-label visibility is outside this spec
and requires a separate owner decision.

No status-page title, status vocabulary, incident composition, subscription flow, feed link, footer text,
or support URL behavior changes.

### 3.6 Static assets

The repository carries these outputs:

| Asset | Context | Tile | Glyph | Size contract |
| --- | --- | --- | --- | --- |
| `logo.svg` under `docs/` | light/neutral external context and README source | `#5854F2` | `#FFFFFF` | `viewBox 0 0 32 32`, nominal `128×128` |
| `logo-dark.svg` under `docs/` | dark-only external context, including the separate landing | `#7D79FF` | `#0B0B0F` | `viewBox 0 0 32 32`, nominal `128×128` |
| `favicon.svg` under `frontend/public/` | browser tab with no reliable theme negotiation | `#5854F2` | `#FFFFFF` | `32×32` |
| `logo.png` under `docs/` | README and raster-only consumers | rendered from light `logo.svg` | rendered from light `logo.svg` | transparent `256×256` PNG |

The PNG is rendered deterministically from the SVG through the repository's pinned Playwright toolchain.
The renderer adds no new package dependency. The README keeps its existing centered image structure and
uses the approved line **Reliability you can prove.**

The dark SVG is the immutable cross-repository asset supplied to the dark-only landing site. Consumers
copy it from a reviewed cerbix commit, not from a dirty working tree.

### 3.7 Visual review matrix

Before source replacement is accepted, the exact canonical paths are reviewed in this matrix:

| Case | Background | Tile | Glyph | Required result |
| --- | --- | --- | --- | --- |
| light default | `#FAFAFB` | `#5854F2` | `#FFFFFF` | C opening and seal remain separate |
| dark default | `#0B0B0F` | `#7D79FF` | `#0B0B0F` | C opening and seal remain separate |
| dark custom accent | surrounding active theme | `#17324D` | `#FFFFFF` | higher-contrast ink selected |
| light custom accent | surrounding active theme | `#F4E36A` | `#0B0B0F` | higher-contrast ink selected |

Each case is inspected at `16px`, `24px`, `32px`, and `128px`. At `16px`, the mark must still read as an
open C plus a distinct horizontal seal rather than a broken circle, minus sign, or checkmark.

This matrix lives here; implementation does not create a second standalone design document for it.

### 3.8 Responsive shell navigation

The desktop shell preserves the approved `240px` sidebar and `56px` topbar. At the existing responsive
breakpoint where the sidebar is hidden, navigation remains available through an explicit menu trigger and
focus-managed drawer; hiding the sidebar must not remove the only route to product surfaces.

The mobile drawer:

- exposes the same labelled organization/project context and navigation items as the desktop sidebar;
- has a keyboard-operable trigger with `aria-expanded` and `aria-controls`;
- moves focus into the drawer on open and returns focus to the trigger on close;
- closes on `Escape`, outside activation, and successful route navigation;
- prevents background interaction while open without trapping the user after close;
- preserves active-route semantics and visible labels rather than replacing navigation with unlabeled icons;
- is tested at `320px`, `375px`, `768px`, and `900px` in both keyboard and pointer flows.

The desktop shell does not gain decorative navigation chrome as part of this work. The change exists to
preserve access to the existing product surfaces on narrow screens.

### 3.9 Atomic organization/project context transitions

Changing organization is a context transition, not two independent visual assignments. The workspace store
and shell must not expose a project list belonging to the previous organization after the new organization is
visibly selected.

The transition contract is:

1. mark the transition pending and disable organization/project selection that would race it;
2. clear or quarantine the previous project list and project selection;
3. load the new organization's projects under a request generation/fence;
4. select a valid project only after the new list is available, or expose the empty-project state;
5. publish the new organization/project context atomically to dependent views;
6. close the switcher after success and return a specific, recoverable error on failure;
7. ignore late responses from generations older than the current transition.

Dashboard, detail views, and mutations must not render a stale project as if it belonged to the new
organization. A failed project read is an error state, not an empty organization.

### 3.10 Overlay, dialog, and focus integrity

Workspace menus, account menus, create dialogs, and the responsive navigation drawer share one interaction
contract. Each overlay has an accessible name, an explicit relationship to its trigger, a predictable open
state, and a focus lifecycle.

Required behavior:

- menu triggers expose `aria-expanded` and `aria-controls`;
- dialogs expose `aria-labelledby` and, when explanatory text exists, `aria-describedby`;
- opening moves focus to the first meaningful control or the dialog's declared initial target;
- `Escape` closes the topmost overlay;
- modal dialogs and drawers keep focus within the active surface while open;
- outside activation closes non-modal menus and does not submit or activate background controls;
- closing restores focus to the opener when it still exists;
- every keyboard-focusable control retains a visible `:focus-visible` indicator with sufficient contrast;
- reduced-motion mode disables decorative transitions without disabling state changes.

A shared overlay primitive is preferred over three locally divergent implementations. The primitive must not
change authorization, route ownership, or mutation validation.

### 3.11 Search combobox integrity

The global SearchBox is a command/search surface, not a visually styled text field. It exposes combobox
semantics and protects the result list from stale asynchronous responses.

The input must provide:

- `role="combobox"`, an accessible label, `aria-expanded`, and `aria-controls`;
- a result container with `role="listbox"` and results with `role="option"`;
- stable option IDs, `aria-selected`, and `aria-activedescendant` for keyboard navigation;
- announced loading, no-results, and request-error states through a polite live region;
- Arrow-key navigation, Enter activation, Escape close/clear behavior, and focus restoration.

Each request belongs to an input generation. An `AbortController` or equivalent sequence guard prevents a
response for query A from replacing the visible results for a later query B. A stale response cannot cause
navigation to a stale organization, project, monitor, service, or other result.

### 3.12 Truthful dashboard loading and no-data rendering

Dashboard data has at least three distinct presentation states: loading, loaded with evidence, and loaded
without evidence. Initial placeholder values must not look like measured facts.

While the first dashboard request is pending:

- KPI values use a skeleton, `—`, or another explicit loading placeholder;
- `0 / 0` is not presented as an observed monitor count;
- availability strips identify loading rather than drawing an empty strip as a result;
- dependent cards do not announce an empty tenant before the read completes.

When data is loaded:

- `null`/missing availability buckets remain neutral and are excluded from uptime arithmetic;
- the legend or accessible description explicitly names `No data`/`UNKNOWN`;
- no-data is not recolored as operational, down, degraded, or zero;
- the visual pattern and text label together distinguish no-data from a measured state;
- a failed read is an error with an actionable message, not an empty dashboard.

This requirement changes only the rendering of loading and absent evidence. It does not change reliability
formulas, bucket geometry, status semantics, or the source facts returned by the API.

### 3.13 Token contrast and semantic text roles

The light and dark token sets require a contrast review for every pair used by meaningful small text,
controls, focus indicators, and status pills. `--ink-3` is reserved for genuinely tertiary metadata; labels,
scope, search state, and other text necessary to operate the product use a role that meets the applicable
contrast target.

The contrast matrix covers:

- `--ink`, `--ink-2`, and `--ink-3` on `--bg`, `--surface`, `--surface-2`, and `--inset`;
- `--accent`/`--accent-ink` on action backgrounds in both themes and valid custom accents;
- each status foreground on its `*-weak` background;
- visible `:focus-visible` indicators against their surrounding surfaces;
- disabled and pending states where text remains actionable or explanatory.

Changing a token value is allowed only when the semantic role, hue meaning, and instance-branding contract
remain intact. Operational status colors are not replaced with the brand accent to solve contrast.

### 3.14 Topbar context and announcements

The shell communicates the current route and scope without requiring the operator to infer it from a page
body:

- breadcrumb navigation has an accessible landmark label and marks the current item with `aria-current`;
- project-scoped views expose organization/project context consistently, including detail and control views;
- breadcrumb links are real navigation links where a parent route exists;
- the theme control communicates its current or next state through an accessible name/state;
- instance announcements map their configured level to an appropriate live-region policy, with critical
  messages announced without turning ordinary informational banners into interruptions.

Route metadata is the preferred source for breadcrumb labels and active navigation. This work does not add
new routes or change authorization for existing routes.

## 4. Preserved product contracts

The following remain behaviorally unchanged unless the implementation requires an import-only adjustment:

- desktop SPA shell geometry: `240px` sidebar and `56px` topbar;
- dashboard KPI row, availability strip, monitor-card grid, desktop density and reliability formulas;
- cards, tables, forms, sparklines, error-budget bars and truthful-rendering patterns;
- light/dark theme token values other than reviewed contrast-role adjustments and runtime inline `--accent-ink`;
- all operational status colors and meanings;
- public branding API, database schema and validation shape;
- `product_name`, `accent_color`, `logo_url`, `footer_text`, `support_url`, and announcement ownership;
- custom logo priority;
- authentication, tenancy, permissions and audit behavior;
- public status data, privacy and incident semantics.

The responsive shell may add a navigation drawer at widths where the sidebar is hidden. Loading, no-data,
focus, breadcrumb, and announcement presentation may improve under §§3.8–3.14 without changing the facts,
formulas, route ownership, or status meanings behind them.

No API, OpenAPI, migration, Go domain, scheduler, worker, agent or storage change belongs to this work.

## 5. Implementation seams

The implementation is divided by ownership rather than by screen:

1. **Glyph owner.** `BrandGlyph` contains the canonical Vue paths and no tile or branding-store logic.
2. **Mark owner.** Existing `BrandMark.vue` chooses custom image versus default tile and sizes both branches.
3. **Color owner.** A new pure the brand-color module helper computes contrast ink; existing `branding.ts` owns CSS
   property mutation and removal.
4. **Surface reuse.** Existing shell and login keep `BrandMark`; public status replaces duplicated fallback
   SVGs with `BrandMark`/`BrandGlyph`.
5. **Asset owner.** Static SVG variants contain the same paths; the pinned Playwright renderer derives PNG.
6. **Delivery owner.** `make spa-snapshot` regenerates the committed embedded SPA after frontend changes.
7. **Responsive-shell owner.** `AppShell` and its navigation primitive own drawer state, route-close behavior,
   active navigation, and mobile focus restoration.
8. **Workspace-transition owner.** The workspace store owns request generations, pending/error state, and the
   atomic publication of organization/project context; views consume the published state.
9. **Overlay owner.** Shared menu/dialog behavior owns labeling, initial focus, focus containment, Escape,
   outside activation, and restore focus.
10. **Search owner.** `SearchBox` owns combobox semantics and request cancellation/sequence fencing.
11. **Truthful-dashboard owner.** Dashboard view models distinguish loading, loaded evidence, no-data, and
   failed reads before presentation; arithmetic helpers remain the source of reliability calculations.
12. **Context and contrast owner.** Route metadata supplies breadcrumb context, while semantic token tests
   own contrast-pair coverage.

These boundaries are mandatory. The geometry is not copied into another Vue view, contrast math is not
implemented in components, and a view does not infer tenant context from stale local state.

## 6. Acceptance invariants

1. With no custom logo, sidebar, login and public status header render Sealed C; no maintained fallback
   renders the old shield/check paths.
2. With a custom `logo_url`, every `BrandMark` consumer renders that image and no Sealed C glyph in its place.
3. `BrandGlyph` renders exactly two canonical paths, uses `currentColor`, is hidden from assistive technology,
   and respects the requested CSS size.
4. `#000000`, `#17324D`, and `#5854F2` select light ink; `#FFFFFF` and `#F4E36A` select dark ink.
5. A valid custom accent sets all four brand properties. Clearing it removes all four inline properties and
   restores theme defaults without a reload or stale color.
6. Custom accent never mutates any operational status token.
7. Public status contains the shared default glyph in the header and footer while preserving custom image,
   page title, support, footer text and status behavior.
8. The Vue glyph, light SVG, dark SVG and favicon carry identical path data and stroke geometry.
9. The light and dark static variants use the exact tile/glyph colors in §3.6.
10. The raster logo is a transparent `256×256` PNG generated from the light SVG; README renders it at the
    existing display size and carries the approved tagline.
11. The old shield paths are absent from maintained frontend source, public assets, docs SVGs and the refreshed
    embedded SPA snapshot.
12. Existing dashboard layout and status colors have no brand-driven redesign; the responsive and state-
    communication changes permitted by §§3.8–3.14 preserve desktop composition, data formulas and semantics.
13. At the responsive breakpoints, the existing navigation remains reachable through a labelled drawer with
    active-route semantics, keyboard operation, Escape, outside close, route close and focus restoration.
14. Organization/project changes never expose a stale project under a new organization; late responses from
    older context generations cannot overwrite the current scope.
15. Menus, dialogs and drawers have correct accessible relationships, initial focus, focus containment where
    modal, Escape behavior, outside behavior and focus restoration; every control has visible focus.
16. SearchBox exposes combobox/listbox semantics, announces loading/no-results/errors, supports keyboard
    navigation, and cannot publish or navigate from stale asynchronous results.
17. Dashboard loading does not present `0 / 0` or an empty strip as measured evidence; loaded no-data remains
    neutral, named and excluded from uptime arithmetic; failed reads remain errors.
18. The light and dark token matrix covers meaningful text, status weak-background pairs, custom accent ink,
    action controls and focus indicators at the applicable contrast target.
19. Breadcrumbs expose route context and current item semantics; project-scoped views show organization and
    project context where the route has a parent scope; theme and announcement controls expose their state.
20. The refreshed `internal/web/dist/` is produced by `make spa-snapshot` and still passes the embedded web
    tests.
21. No production deployment, restart, settings mutation or data change is part of implementation acceptance.

## 7. Required test matrix

| Layer | Required evidence |
| --- | --- |
| Glyph component | exact paths, viewBox, hidden accessibility state, requested size |
| Mark component | default Sealed C branch; custom-image priority branch |
| Pure color helper | black, white, default iris, dark custom accent and light custom accent |
| Branding store | sets four properties; clears all four; preserves status properties |
| Public status component | shared header/footer glyphs; no old inline shield |
| Static assets | path parity across light SVG, dark SVG and favicon; exact theme colors |
| PNG renderer | PNG signature, transparent output, `256×256` dimensions |
| Frontend suite | all Vitest tests, Vue type check and Vite build |
| Live browser | configured light custom accent renders dark glyph on login; product name/footer/support round-trip and restore |
| Responsive shell | drawer is reachable at `320px`, `375px`, `768px`, and `900px`; route close, Escape, outside close and focus restoration work |
| Workspace transition | changing organization clears/quarantines old projects, publishes one coherent context, handles failure, and ignores late generations |
| Overlay accessibility | menu/dialog/drawer labels, initial focus, focus containment, Escape, outside behavior and restore focus |
| SearchBox | combobox/listbox roles, keyboard navigation, live loading/no-results/error, and stale-response rejection |
| Dashboard state | loading placeholders, no-data legend/readout, neutral missing buckets, failed-read message and unchanged uptime arithmetic |
| Token contrast | light/dark text pairs, status weak backgrounds, focus, actions and custom accent ink |
| Topbar context | breadcrumb landmark/current item, organization/project scope, theme state and announcement live-region policy |
| Embedded web | refreshed SPA snapshot and `internal/web` tests |
| Documentation | docs unit/reference gate and whitespace check |
| Visual review | §3.7 at four sizes, both default themes and both custom accents, plus mobile drawer and no-data states |

The live browser test uses a disposable development stack, reuses the shared authenticated session, opens a
fresh anonymous context for login, and restores the prior branding settings in `finally`. It never logs out
the shared session and never targets production.

## 8. Delivery and evidence

An implementation is not complete until all of the following are recorded in the owning iteration report:

- exact changed surfaces and assets;
- RED and GREEN evidence for component, color and store contracts;
- full frontend unit/type/build result;
- `make spa-snapshot` result and embedded web test result;
- live instance-branding browser result, or an explicit reason it was skipped;
- docs/reference check result;
- visual review of §3.7 and the responsive/no-data states;
- keyboard-only and screen-reader-oriented checks for shell, overlays, search and announcements;
- confirmation that dashboard/layout and status semantics did not drift;
- confirmation that no production action occurred.

The implementation decision record must state the default-mark replacement, custom-logo priority,
contrast-selected custom-accent ink, status-color independence, static geometry parity, and absence of an SPA
redesign. Iteration and decision identifiers are allocated when implementation starts; this long-lived spec
does not reserve temporary numbers.

## 9. Rollback

Rollback restores the prior fallback component and static assets, reapplies the previous frontend snapshot,
and removes the custom `--accent-ink` override logic. If the shell UX extension has shipped, rollback may also
restore the prior sidebar/drawer, overlay, SearchBox, breadcrumb, and dashboard-loading presentation as one
frontend snapshot; it must not restore stale tenant data or remove the underlying API facts. It does not alter
stored branding settings, database rows, API payloads, custom logo URLs, or configured accent values. A rollback
therefore returns rendering to the previous behavior without a data migration.

## 10. Non-goals

- replacing the SPA's data-first visual language, desktop shell geometry, dashboard composition, login card, public status composition, or navigation information architecture;
- changing operational colors, status meanings or truthful-rendering vocabulary;
- changing the branding API, settings schema, validation limits or precedence;
- changing `Powered by cerbix` visibility or white-label policy;
- introducing gradients, glow, 3D effects, a mascot, a dog/Cerberus illustration, a shield or a checkmark;
- adding an icon library, UI kit, charting library or runtime asset service;
- implementing the separate marketing-site visual migration inside this repository;
- deployment, DNS, certificate, reverse-proxy or production settings work.
