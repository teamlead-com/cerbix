# cerbix Brand Guidelines v1.0

Status: approved brand design. The production assets and UI changes described here are OWNER-APPROVED and integrated in implementation commit `6ab380397a96b388308bdf25f1e88eeebeb76d57`; iter-0193 is CLOSED with documented skips.

Date: 2026-09-27

This document is the source of truth for the cerbix brand across the embedded SPA, public status pages, documentation, repository assets, presentations, and the separate marketing site. Product UI behavior and data-rendering rules remain governed by [the approved design language](design/notes.md). If a visual branding rule conflicts with truthful rendering, accessibility, or an operational status contract, the product rule wins.

## Quick reference

- **Name:** `cerbix`, lowercase.
- **Category:** provable reliability platform.
- **Primary audience:** SRE and Platform Engineering teams.
- **English tagline:** **Reliability you can prove.**
- **Russian tagline:** **Надёжность, которую можно доказать.**
- **Operating principle:** **Evidence before confidence.**
- **Voice:** precise, calm, candid, operational.
- **Brand accent:** iris, separate from all status colors.
- **Default mark:** **Sealed C**, an open measurement window closed by an evidence seal.
- **Product relationship:** cerbix is an independent brand. TeamLead appears in legal, repository, and footer attribution rather than competing with cerbix in primary messaging.

## 1. Strategic foundation

### Mission

Help engineering teams make reliability decisions from sufficient evidence rather than assumptions.

### Positioning

cerbix is a self-hosted provable reliability platform for SRE and Platform Engineering teams. It connects versioned reliability definitions, first-party checks, and operational response, and withholds a reliability figure when the available evidence cannot defend it.

cerbix is not an observability platform or an external telemetry store. It measures reliability from its own checks and makes the boundary between measured, provisional, missing, and unverifiable data explicit.

### Brand promise

A reliability figure from cerbix has a visible basis. When that basis is missing, cerbix says so instead of replacing uncertainty with a reassuring number.

### Message pillars

1. **Do not hide uncertainty.** `GOOD`, `BAD`, `UNKNOWN`, `no_data`, `not-stored`, provisional ranges, and withheld values are distinct states.
2. **Measure inside the operator's environment.** cerbix is self-hosted and can run checks from private networks and remote regions.
3. **Turn evidence into action.** Reliability definitions, SLOs, error budgets, incidents, on-call escalation, status pages, and release decisions belong to one operational path.

### Brand architecture

cerbix stands on its own. Use TeamLead attribution in legal copy, repository ownership, package metadata, and restrained footer references. Do not use `cerbix by TeamLead` as the primary lockup or hero message unless brand architecture is deliberately revised later.

## 2. Messaging architecture

### Primary message

**Reliability you can prove.**

### Supporting messages

| Need | Message | Evidence to show |
|---|---|---|
| Trustworthy SLO reporting | A number appears only when the evidence supports it. | Sealed windows, explicit coverage, withheld values, and revision references. |
| Control over checks and data | Measure from your own environment. | Self-hosted deployment, private-network checks, and regional agents. |
| A complete operating loop | Move from definition to measurement to response. | SLOs, error budgets, incidents, escalation, status pages, and gates. |
| Honest uncertainty | Missing evidence is a state, not a hidden failure. | Neutral `UNKNOWN`/`no_data`, hatch or gap rendering, and a textual reason. |

### Short descriptions

**One line**

> cerbix is a self-hosted platform that reports reliability only when it has enough evidence to defend the result.

**Short pitch**

> Define what reliable means for a service, measure it with checks from your own environment, and act on the result. cerbix keeps `UNKNOWN` visible and withholds figures that the available evidence cannot support.

### Preferred calls to action

- See how evidence becomes a reliability decision.
- Get started.
- Review the reliability definition.
- Inspect the evidence window.
- Resolve missing coverage.

Avoid vague calls to action such as “Unlock reliability”, “Transform operations”, or “Get full control”.

## 3. Voice and tone

### Voice traits

| Trait | cerbix is | cerbix is not |
|---|---|---|
| Precise | Specific about state, window, revision, and cause. | Bureaucratic or overloaded with jargon. |
| Calm | Direct during both healthy and failing states. | Alarmist, celebratory, or emotionally flat. |
| Candid | Explicit about missing data, uncertainty, and limitations. | Apologetic, evasive, or falsely reassuring. |
| Engineering-led | Technically correct and testable. | Performatively technical. |
| Operational | Oriented toward the next useful action. | Prescriptive without explaining why. |

### Message structure

Prefer:

> **State → evidence → action**

Example:

> SLI withheld. Three of five required observations are present in the selected window. Inspect the unavailable monitors.

Do not write:

> Something went wrong. Try checking your configuration.

### Tone by context

| Context | Tone | Example |
|---|---|---|
| Marketing | Confident, concise, evidence-led. | “Reliability you can prove.” |
| Documentation | Sequential and explicit about prerequisites and expected output. | “Create the secret file with mode `0600`, then start the services.” |
| Product status | State first, cause second, action third. | “No data. The monitor has not reported in this window.” |
| Incident communication | Factual about scope and current action. | “Checkout probes are failing in `eu-central`. Investigation is in progress.” |
| Success | Restrained and specific. | “Definition rev 7 validated.” |
| Error | Clear and accountable. | “The result was not calculated because `checkout-http` stopped reporting.” |

### Language rules

- Write `cerbix` in lowercase, including the wordmark. At the beginning of prose, rewrite the sentence when practical rather than capitalizing the name.
- Use established technical terms such as SLO, SLI, error budget, self-hosted, and on-call when they are more precise than an artificial translation.
- Keep ordinary verbs natural in the surrounding language. Do not turn every phrase into mixed-language jargon.
- Keep sentences short and give each sentence one primary claim.
- Attach a time window, definition revision, or evidence reference to reliability figures when context requires it.
- Treat `UNKNOWN` as a first-class state, not a softer spelling of failure.
- Do not claim certainty beyond what the product can show.

### Prohibited claims and phrases

Do not use these without a narrowly supported and reviewable meaning:

- single source of truth;
- complete control;
- guaranteed reliability;
- always reliable;
- AI-powered;
- revolutionary;
- seamless;
- enterprise-grade;
- all your telemetry;
- full observability.

## 4. Visual identity

### 4.1 Product-led Proof

The product is the visual source of truth. The brand has two density modes, not two separate identities:

- **Operational Proof:** the existing SPA, login, settings, and status-page grammar. Dense, measured, and optimized for decisions.
- **Editorial Proof:** the marketing and documentation expression. Larger type and more space, while retaining the same tokens, type roles, borders, data motifs, and status semantics.

The marketing site must learn from the product. The SPA is not redesigned to resemble a marketing concept.

### 4.2 Color system

The runtime semantic tokens in `frontend/src/style.css` remain canonical.

#### Light theme

| Role | Value |
|---|---:|
| Background | `#FAFAFB` |
| Surface | `#FFFFFF` |
| Surface 2 | `#F5F5F8` |
| Inset | `#F0F0F4` |
| Border | `#E9E9EF` |
| Strong border | `#DADAE4` |
| Primary ink | `#17171F` |
| Secondary ink | `#55556A` |
| Muted ink | `#8A8A9D` |
| Brand accent | `#5854F2` |
| Secondary accent | `#7A77FF` |
| Operational | `#12A05C` |
| Down | `#E0393F` |
| Degraded | `#B97800` |
| Maintenance | `#3A7DE5` |
| Pending / unknown | `#8A8A9D` |

#### Dark theme

| Role | Value |
|---|---:|
| Background | `#0B0B0F` |
| Surface | `#141419` |
| Surface 2 | `#1A1A20` |
| Inset | `#101015` |
| Border | `#262630` |
| Strong border | `#34343F` |
| Primary ink | `#F2F2F6` |
| Secondary ink | `#A4A4B6` |
| Muted ink | `#6C6C7E` |
| Brand accent | `#7D79FF` |
| Secondary accent | `#928FFF` |
| Operational | `#35C67F` |
| Down | `#FF5F64` |
| Degraded | `#E0A53A` |
| Maintenance | `#5C9BFF` |
| Pending / unknown | `#6C6C7E` |

#### Color rules

- Iris identifies cerbix, selected navigation, primary actions, links, and keyboard focus.
- Green means an observed operational state. It is not a decorative marketing accent.
- Red means down or failed.
- Amber means degraded, not unknown.
- Blue means maintenance or its existing product-specific semantic role.
- Neutral gray means pending, unknown, no data, or unavailable evidence where the product contract specifies it.
- Every status includes text, a symbol, a pattern, or another non-color cue.
- Marketing examples use status colors only when depicting a truthful, explicitly labelled product state.

### 4.3 Typography

Use the existing product stacks across both product and marketing surfaces.

```css
--font-sans: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto,
  "Helvetica Neue", Arial, sans-serif;
--font-mono: ui-monospace, "SF Mono", "JetBrains Mono", Menlo,
  Consolas, monospace;
```

- **Sans:** navigation, prose, controls, help, and long-form documentation.
- **Mono:** measurements, percentages, latency, identifiers, timestamps, revisions, status metadata, and the `cerbix` wordmark.
- Use tabular numerals for data.
- Preserve the product hierarchy: compact labels, restrained headings, and data-led emphasis.
- Editorial surfaces may increase scale and spacing but must not introduce a separate font identity.

### 4.4 Logo: Sealed C

The approved default mark is **Sealed C**.

#### Meaning

- The open `C` is a measurement window that has not yet been assumed complete.
- The closing bar is the seal applied when the evidence boundary is explicit.
- The form refers to cerbix without using a security shield, literal checkmark, dog, or Cerberus illustration.

#### Construction and variants

- Primary lockup: Sealed C tile plus the lowercase mono wordmark `cerbix`.
- Icon-only: Sealed C tile.
- Monochrome light and monochrome dark variants are required.
- Default light-theme tile: light-theme iris.
- Default dark-theme tile: dark-theme iris.
- The glyph is one high-contrast color; it does not use operational green.
- Minimum icon size: `24px`.
- Minimum clear space around the mark: half the tile width.

The selected browser concept is directionally approved, but the final production SVG still requires optical tuning at `16px`, `24px`, `32px`, and large sizes before replacing existing assets.

#### Instance branding

When `logo_url` is present, the custom image has priority and Sealed C is not shown in its place.

When an instance changes `accent_color` without supplying a logo:

1. Keep the Sealed C geometry.
2. Use the configured accent for the tile.
3. Choose the light or dark glyph color by whichever produces the higher WCAG contrast against that accent.
4. Do not derive or replace operational status colors from the custom accent.

This updates the current accent-tile behavior rather than removing instance branding.

#### Incorrect use

Do not:

- restore a shield/check fallback after Sealed C ships;
- add a green seal to the logo;
- use gradients, glow, shadows, bevels, or 3D effects;
- rotate, stretch, crop, or rearrange the mark;
- add dog heads, paws, flames, or security imagery;
- use status colors as alternate brand editions;
- place the mark on a background that obscures its silhouette.

### 4.5 Shape and layout

- Preserve the existing `8px` layout grid.
- Use `8px`, `6px`, and `4px` radii according to the product hierarchy.
- Use cool hairline borders and restrained shadows.
- Keep the uptime-signal motif as the bold data element; surrounding UI stays quiet.
- Prefer rectangular evidence records, revision labels, sealed windows, timeline gaps, and precise dividers over decorative illustration.

## 5. Surface profiles

### 5.1 Operational Proof: SPA

The current deployed SPA is the baseline.

Keep:

- the `240px` sidebar and `56px` topbar model;
- breadcrumbs, search, action, theme, and account placement;
- four-up KPI row where viewport space allows;
- three-column monitor-card grid where viewport space allows;
- existing cards, tables, forms, sparklines, availability strips, error-budget bars, and status pills;
- light and dark themes;
- current density and responsive behavior;
- truthful rendering for gaps, unknown data, provisional ranges, and withheld results.

Brand adoption inside the SPA changes the default mark and related static assets. It does not trigger a dashboard or component redesign.

### 5.2 Editorial Proof: marketing site

The marketing site uses the same system with more space and larger hierarchy.

- Lead with the provable-reliability promise, not a feature inventory.
- Use real product tokens and type stacks.
- Use iris for primary calls to action.
- Use proof-board examples that follow the real data vocabulary.
- Mark all synthetic data as example data.
- Prefer current, verified product screenshots over invented dashboard compositions.
- Use availability strips and evidence gaps to explain product behavior, not as decorative texture.
- Keep operational green inside an actual status or measured example.
- Avoid marketing-only cards, colors, or typography that disappear after sign-in.

### 5.3 Public status pages

- Keep the wider, calmer public-status composition.
- Preserve the status-page vocabulary, including neutral `no_data`.
- Do not add marketing calls to action inside incident or maintenance flows.
- Apply instance branding consistently where the current public-branding contract allows it.
- Review `Powered by cerbix` separately for white-label expectations; do not let it compete with the instance identity.

### 5.4 Documentation and repositories

- Use the same Sealed C asset in repository headers, favicons, documentation, and release materials.
- Screenshots must represent a current supported UI state and identify synthetic/example data when present.
- Commands and technical claims take precedence over decorative brand language.
- Keep diagrams schematic and data-led. Do not use generic cloud, shield, or observability imagery.

## 6. Imagery, diagrams, and data visualization

### Screenshots

- Prefer real product screens with representative but non-sensitive data.
- Do not reconstruct a dashboard when a current screen is available.
- Crop to the claim being made; retain enough context to show that the state is real.
- Do not recolor status data for composition.

### Diagrams

- Use flat, outlined geometry with the shared neutral and iris tokens.
- Use operational colors only when the diagram represents operational states.
- Label every state and connection; do not rely on color alone.
- Use hatching, outlines, gaps, or dashed boundaries for absent and unverifiable ranges.

### Charts

- Follow the product's hand-rolled SVG grammar: true time widths, visible gaps, restrained grid lines, and explicit endpoints.
- Never interpolate across a gap unless the underlying ledger proves continuity.
- Do not display `UNKNOWN` as zero or healthy.
- Keep legends close to the data and use the product's established labels.

## 7. Examples

### Marketing rewrite

Avoid:

> Complete control over the reliability of every service.

Use:

> Define what reliable means. cerbix measures it with your checks and shows whether the evidence is sufficient for a conclusion.

### Healthy state

Avoid:

> Everything is working perfectly.

Use:

> The SLO is met for the last 28 days. All required monitors supplied sufficient evidence.

### Missing evidence

Avoid:

> Reliability is 0%.

Use:

> SLI withheld. Required observations are missing from the selected window.

### Error

Avoid:

> Something went wrong.

Use:

> The result was not calculated because `checkout-http` stopped reporting 42 minutes ago.

## 8. Governance

- This document owns brand strategy, voice, logo rules, and cross-surface consistency.
- `docs/design/notes.md` owns the Vue SPA's component, layout, theme, and truthful-rendering language.
- Product specifications own domain behavior and exact status semantics.
- The marketing-site profile may narrow these rules for its implementation but may not redefine the core brand.
- A change to the logo concept, category, primary tagline, or status-color separation requires explicit product-owner approval.
- Before shipping a new logo asset, review it at favicon size, in both themes, with default and custom accents, and with a custom `logo_url` configured.

## 9. Implementation boundary

This guide approves the brand direction; it does not authorize a code change by itself.

The normative cerbix runtime, asset, compatibility, and verification contract is
[`docs/specs/cross-brand-identity.md`](specs/cross-brand-identity.md). The separate marketing repository
owns its Editorial Proof migration while consuming the reviewed dark Sealed C asset by immutable cerbix
commit. Implementation evidence belongs in the iteration that performs the change, not in another plan file.

## 10. Machine-readable brand summary

This compact section follows the schema used by the local brand-context tooling. The detailed rules above remain authoritative.

### Primary Colors

| Name | Hex | Usage |
|---|---:|---|
| Iris Light | `#5854F2` | Brand and actions in the light theme. |
| Iris Dark | `#7D79FF` | Brand and actions in the dark theme. |

### Secondary Colors

| Name | Hex | Usage |
|---|---:|---|
| Iris Light Secondary | `#7A77FF` | Supporting light-theme emphasis. |
| Iris Dark Secondary | `#928FFF` | Supporting dark-theme emphasis. |

### Neutral Palette

| Name | Hex | Usage |
|---|---:|---|
| Light Background | `#FAFAFB` | Light page background. |
| Light Surface | `#FFFFFF` | Light cards and panels. |
| Light Ink | `#17171F` | Primary light-theme text. |
| Light Muted | `#8A8A9D` | Muted light-theme text. |
| Dark Background | `#0B0B0F` | Dark page background. |
| Dark Surface | `#141419` | Dark cards and panels. |
| Dark Ink | `#F2F2F6` | Primary dark-theme text. |
| Dark Muted | `#6C6C7E` | Muted dark-theme text. |

### Semantic Colors

| State | Light | Dark |
|---|---:|---:|
| Operational | `#12A05C` | `#35C67F` |
| Down | `#E0393F` | `#FF5F64` |
| Degraded | `#B97800` | `#E0A53A` |
| Maintenance | `#3A7DE5` | `#5C9BFF` |
| Pending / unknown | `#8A8A9D` | `#6C6C7E` |

### Font Stack

```css
--font-heading: "-apple-system";
--font-body: "-apple-system";
--font-mono: "ui-monospace";
```

Use the complete fallback stacks defined in [Typography](#43-typography), not only the first family extracted here.

### Brand Personality

| Trait | Description |
|---|---|
| **Precise** | Names the state, evidence boundary, and relevant context. |
| **Calm** | Communicates failures without drama or false reassurance. |
| **Candid** | Makes uncertainty and missing evidence explicit. |
| **Engineering-led** | Uses technically correct, testable language. |
| **Operational** | Leads from a fact to a useful next action. |

### Prohibited Terms

| Avoid | Reason |
|---|---|
| Single source of truth | Too broad unless narrowly defined and demonstrated. |
| Complete control | Implies authority the product cannot guarantee. |
| Guaranteed reliability | Contradicts evidence-led positioning. |
| AI-powered | Unsupported product claim. |
| Revolutionary | Promotional language without evidence. |
| Seamless | Hides meaningful operational boundaries. |
| Enterprise-grade | Vague and untestable. |
| Full observability | cerbix is not an observability platform. |

### Core Attributes

| Attribute | Description |
|---|---|
| **Evidence first** | Confidence follows sufficient evidence. |
| **Truthful rendering** | Missing and provisional data remain visible. |
| **Operator control** | Checks and deployment remain in the operator's environment. |
| **One operating loop** | Definition, measurement, and response stay connected. |

### Visual Don'ts

| Avoid | Reason |
|---|---|
| Security shields | They misclassify cerbix as a security product. |
| Decorative status colors | Operational colors carry fixed semantic meaning. |
| Invented dashboards | Product evidence must match the real interface. |
| Cyberpunk glow and glass | They conflict with quiet, precise product minimalism. |
| Dog or Cerberus mascots | They make the name literal and weaken the evidence concept. |
