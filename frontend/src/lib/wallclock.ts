// The ONE time-rendering mechanism (func-truthful-rendering §8, FR-031 / NFR-025a, D-0235).
//
// IDENTITY IS UTC; PRESENTATION IS LOCAL. Every canonical grain, every requested range, every
// bucket and cell identity and all arithmetic stay UTC — a local-day grain would stop the cells
// decomposing the number printed above them, and two viewers in different zones would read
// different per-cell figures for one published availability. What is local is only what a human
// reads, and it always names the offset it was rendered in.
//
// A NAMED RENDERER PER SUBJECT, NEVER ONE FORMATTER (ruled at party [143]). There is deliberately no generic
// "format a date" here that could be handed a UTC bucket and produce the viewer's calendar day:
// an instant and a UTC cell's extent are different objects and get different functions. A UTC day
// shown to a viewer at UTC+05 begins at 05:00 their time, so its extent is rendered as
// `start → end` and never as their `01.09` — labelling a bucket with a local calendar day is a
// boundary lie of exactly the kind this whole requirement exists to remove.
//
// THE OFFSET IS RESOLVED AT THE INSTANT, never from a cached current offset (ruled at party
// [143]). A 30-day window in late March or late October crosses a DST boundary, so this is the
// ordinary case rather than an edge, and a cached offset mislabels every cell on the far side of
// it. `Intl.DateTimeFormat.formatToParts` is called with the instant itself, which is what makes
// the answer per-instant.
//
// The `zone` parameter exists so the DST property can be TESTED at a named zone; `Intl` takes a
// time zone anyway. NO PRODUCT CALL SITE PASSES IT, and `wallclock.spec.ts` asserts that against
// the source rather than trusting it.

/** What every formatter here returns when it is handed something it cannot render. */
const ABSENT = "—";

type Parts = Record<string, string>;

function partsAt(d: Date, zone: string | undefined, opts: Intl.DateTimeFormatOptions): Parts {
  const f = new Intl.DateTimeFormat("en-GB", { ...opts, timeZone: zone, hourCycle: "h23" });
  const out: Parts = {};
  for (const p of f.formatToParts(d)) out[p.type] = p.value;
  return out;
}

/**
 * The numeric offset in force AT this instant, as `UTC+05:00` / `UTC-03:30` / `UTC+00:00`.
 * Half-hour and three-quarter-hour zones keep their minutes; UTC itself reports `+00:00`, which
 * `longOffset` renders as a bare `GMT`.
 */
function offsetAt(d: Date, zone?: string): string {
  const name = partsAt(d, zone, { timeZoneName: "longOffset" }).timeZoneName ?? "GMT";
  const m = /^GMT([+-]\d{2}:\d{2})$/.exec(name);
  return "UTC" + (m ? m[1] : "+00:00");
}

function parse(iso: string | null | undefined): Date | null {
  if (!iso) return null;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? null : d;
}

const dmy = (p: Parts) => `${p.day}.${p.month}.${p.year}`;
const dm = (p: Parts) => `${p.day}.${p.month}`;
const hm = (p: Parts) => `${p.hour}:${p.minute}`;

/**
 * An INSTANT a human reads — a heartbeat's timestamp, a change mark, a save. Rendered in the
 * viewer's zone to the second, with the offset that was in force then.
 *
 *   instantLabel("2026-09-03T15:04:31Z")  ->  "03.09.2026 20:04:31 (UTC+05:00)"
 */
export function instantLabel(iso: string | null | undefined, zone?: string): string {
  const d = parse(iso);
  if (!d) return ABSENT;
  const p = partsAt(d, zone, {
    day: "2-digit", month: "2-digit", year: "numeric",
    hour: "2-digit", minute: "2-digit", second: "2-digit",
  });
  return `${dmy(p)} ${hm(p)}:${p.second} (${offsetAt(d, zone)})`;
}

/**
 * A UTC CELL's EXTENT — a bucket, a rollup step, a segment range. Rendered as the cell's real
 * local start and end, so a UTC day is never presented as the viewer's calendar day.
 *
 *   utcCellExtentLabel("2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z")
 *     ->  "01.09 05:00 → 02.09 05:00 (UTC+05:00)"
 *
 * The offset shown is the one in force at the cell's START; when a cell spans a DST change its
 * two ends carry different offsets, and the label then names both rather than pretending one
 * covered the whole cell.
 */
export function utcCellExtentLabel(
  fromIso: string | null | undefined,
  toIso: string | null | undefined,
  zone?: string,
): string {
  const a = parse(fromIso);
  const b = parse(toIso);
  if (!a || !b) return ABSENT;
  const pa = partsAt(a, zone, { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit" });
  const pb = partsAt(b, zone, { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit" });
  const oa = offsetAt(a, zone);
  const ob = offsetAt(b, zone);
  const tail = oa === ob ? `(${oa})` : `(${oa} → ${ob})`;
  return `${dm(pa)} ${hm(pa)} → ${dm(pb)} ${hm(pb)} ${tail}`;
}

/**
 * The same rule as `instantLabel` at MINUTE precision, for a list or a table cell where seconds
 * are noise. It still names the offset — that is the part NFR-025 is about, and a shorter
 * rendering does not get to drop it.
 *
 *   instantLabelShort("2026-09-03T12:55:00Z")  ->  "03.09.2026 17:55 (UTC+05:00)"
 */
export function instantLabelShort(iso: string | null | undefined, zone?: string): string {
  const d = parse(iso);
  if (!d) return ABSENT;
  const p = partsAt(d, zone, {
    day: "2-digit", month: "2-digit", year: "numeric", hour: "2-digit", minute: "2-digit",
  });
  return `${dmy(p)} ${hm(p)} (${offsetAt(d, zone)})`;
}

/**
 * A window between two INSTANTS — an escalation override, a silence, a maintenance range. This is
 * not `utcCellExtentLabel`: that one describes a UTC bucket and exists to stop a bucket being
 * called the viewer's calendar day, while this describes two instants an operator chose.
 *
 * The offset is named ONCE when both ends share it, and TWICE when they do not, because a window
 * that crosses a DST change genuinely has two — and repeating an identical offset in one cell is
 * noise, not honesty. The date collapses the same way.
 *
 *   same day, one offset:   "03.09.2026 17:55 → 18:55 (UTC+05:00)"
 *   across midnight:        "03.09.2026 23:55 → 04.09.2026 00:55 (UTC+05:00)"
 *   across a DST change:    "29.03.2026 01:55 (UTC+01:00) → 29.03.2026 03:55 (UTC+02:00)"
 */
export function instantRangeLabel(
  fromIso: string | null | undefined,
  toIso: string | null | undefined,
  zone?: string,
): string {
  const a = parse(fromIso);
  const b = parse(toIso);
  if (!a || !b) return ABSENT;
  const oa = offsetAt(a, zone);
  const ob = offsetAt(b, zone);
  if (oa !== ob) return `${instantLabelShort(fromIso, zone)} → ${instantLabelShort(toIso, zone)}`;
  const opts: Intl.DateTimeFormatOptions = {
    day: "2-digit", month: "2-digit", year: "numeric", hour: "2-digit", minute: "2-digit",
  };
  const pa = partsAt(a, zone, opts);
  const pb = partsAt(b, zone, opts);
  const head = `${dmy(pa)} ${hm(pa)}`;
  const tail = dmy(pa) === dmy(pb) ? hm(pb) : `${dmy(pb)} ${hm(pb)}`;
  return `${head} → ${tail} (${oa})`;
}

/**
 * The UTC instant itself, for the second line of a readout: an engineer correlating a cell with
 * logs needs the canonical time, and the facts are UTC.
 */
export function utcInstantLabel(iso: string | null | undefined): string {
  const d = parse(iso);
  return d ? d.toISOString().replace(".000Z", "Z") : ABSENT;
}

/** The UTC extent, for the same second line when the subject is a cell rather than an instant. */
export function utcExtentLabel(
  fromIso: string | null | undefined,
  toIso: string | null | undefined,
): string {
  const a = parse(fromIso);
  const b = parse(toIso);
  if (!a || !b) return ABSENT;
  return `${utcInstantLabel(fromIso)} → ${utcInstantLabel(toIso)}`;
}

// ── UTC subjects (NFR-025c) ─────────────────────────────────────────────────────────────────────
//
// Everything above renders an instant in the VIEWER's zone and names the offset. What follows
// renders subjects whose identity IS UTC — a ledger day, a gate window's date, the change
// timeline's clock — and the honest treatment of those is to SAY UTC rather than convert them
// (`func-truthful-rendering.md` §9, AC-NFR-025c). Converting a UTC day to the viewer's calendar
// day is the boundary lie this whole requirement exists to remove; leaving it unlabelled, which
// is what thirteen files did, is the same lie with the evidence removed.
//
// TWO FORMS, and every UTC rendering is in exactly one of them — asserted in `wallclock.spec.ts`
// by calling each one, not promised here:
//
//   a PHRASE a person reads   ends in ` UTC`   (utcDayLabel, utcClockLabel, utcCompactInstantLabel …)
//   a CANONICAL instant       ends in `Z`      (utcInstantLabel, utcExtentLabel, utcSecondsLabel,
//                                               utcMillisLabel — `2026-08-28 14:03:02Z`)
//
// The second form is not an exception being tolerated: those four exist so an engineer can paste
// the value into a log query, and `Z` is the designator that form uses. What is NOT allowed is a
// third answer — a rendering that names no zone at all, which is what `changes.ts` did to five of
// its six.
//
// This comment said "one suffix, ` UTC`, decided once" and carved out only `utcInstantLabel`.
// That was false the moment `utcSecondsLabel` and `utcMillisLabel` were added beside it, and the
// sentence outlived them the same way the zone-argument guard outlived its own list (party [199]).
// The rule is a test now, so a new renderer cannot join without landing in one bucket or the other.
//
// The DATE FORMAT is `dmy`, the same one `instantLabel` uses. The legacy sites wrote
// `2026-09-05` (an ISO slice) and `05.09.2026` (hand-padded) in roughly equal numbers; one
// mechanism gets one format.

/**
 * A UTC calendar day shown as a fact — a token's creation, a secret's rotation, a subscriber's
 * sign-up, a gate objective's last change.
 *
 *   utcDayLabel("2026-09-05T12:04:31Z")  ->  "05.09.2026 UTC"
 */
export function utcDayLabel(iso: string | null | undefined): string {
  const d = parse(iso);
  if (!d) return ABSENT;
  const p = partsAt(d, "UTC", { day: "2-digit", month: "2-digit", year: "numeric" });
  return `${dmy(p)} UTC`;
}

/**
 * A range of UTC calendar days — a reliability segment's extent. The suffix is named ONCE: both
 * ends are UTC by construction, and repeating it is noise rather than honesty.
 *
 *   utcDayRangeLabel("2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z")
 *     ->  "01.09.2026 → 02.09.2026 UTC"
 */
export function utcDayRangeLabel(
  fromIso: string | null | undefined,
  toIso: string | null | undefined,
): string {
  const a = parse(fromIso);
  const b = parse(toIso);
  if (!a || !b) return ABSENT;
  const pa = partsAt(a, "UTC", { day: "2-digit", month: "2-digit", year: "numeric" });
  const pb = partsAt(b, "UTC", { day: "2-digit", month: "2-digit", year: "numeric" });
  return `${dmy(pa)} → ${dmy(pb)} UTC`;
}

/**
 * The UTC clock of an instant, minute precision — a status page's "updated at", a change phase
 * anchored beside an incident.
 *
 *   utcClockLabel("2026-09-05T14:05:12Z")  ->  "14:05 UTC"
 */
export function utcClockLabel(iso: string | null | undefined): string {
  const d = parse(iso);
  if (!d) return ABSENT;
  return `${hm(partsAt(d, "UTC", { hour: "2-digit", minute: "2-digit" }))} UTC`;
}

/**
 * A window between two UTC clocks on the compare view — one side's range.
 *
 *   utcClockRangeLabel("2026-09-05T13:05:00Z", "2026-09-05T14:05:00Z")  ->  "13:05 → 14:05 UTC"
 */
export function utcClockRangeLabel(
  fromIso: string | null | undefined,
  toIso: string | null | undefined,
): string {
  const a = parse(fromIso);
  const b = parse(toIso);
  if (!a || !b) return ABSENT;
  return `${hm(partsAt(a, "UTC", { hour: "2-digit", minute: "2-digit" }))} → ${hm(partsAt(b, "UTC", { hour: "2-digit", minute: "2-digit" }))} UTC`;
}

/**
 * A UTC day-and-clock without the year — a change phase written relative to the phase before it,
 * where the year is never the thing that changed.
 *
 *   utcDayClockLabel("2026-08-28T16:40:00Z")  ->  "28.08 16:40 UTC"
 */
export function utcDayClockLabel(iso: string | null | undefined): string {
  const d = parse(iso);
  if (!d) return ABSENT;
  const p = partsAt(d, "UTC", { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit" });
  return `${dm(p)} ${hm(p)} UTC`;
}

/**
 * The canonical UTC instant to the SECOND, spaced rather than `T`-joined, as a seal is quoted.
 *
 *   utcSecondsLabel("2026-08-28T14:03:02.417Z")  ->  "2026-08-28 14:03:02Z"
 */
export function utcSecondsLabel(iso: string | null | undefined): string {
  const d = parse(iso);
  if (!d) return ABSENT;
  return d.toISOString().replace(/\.\d+Z$/, "Z").replace("T", " ");
}

/** The same with MILLISECONDS kept, for a snapshot instant: "2026-08-28 14:03:02.417Z". */
export function utcMillisLabel(iso: string | null | undefined): string {
  const d = parse(iso);
  if (!d) return ABSENT;
  return d.toISOString().replace("T", " ");
}

/**
 * The change timeline's COMPACT instant: as much date as the reader needs relative to `now`, and
 * never less zone than the rest of the app.
 *
 *   same UTC day as now:  "16:40 UTC"
 *   same UTC year:        "27.08 16:40 UTC"
 *   otherwise:            "27.08.2025 16:40 UTC"
 *
 * `now` is a parameter rather than a captured clock so the three branches are testable, and it is
 * compared in UTC because the subject is.
 */
export function utcCompactInstantLabel(
  iso: string | null | undefined,
  now: Date = new Date(),
): string {
  const d = parse(iso);
  if (!d) return ABSENT;
  const opts: Intl.DateTimeFormatOptions = {
    day: "2-digit", month: "2-digit", year: "numeric", hour: "2-digit", minute: "2-digit",
  };
  const p = partsAt(d, "UTC", opts);
  const n = partsAt(now, "UTC", opts);
  if (dmy(p) === dmy(n)) return utcClockLabel(iso);
  if (p.year === n.year) return utcDayClockLabel(iso);
  return `${dmy(p)} ${hm(p)} UTC`;
}

/**
 * The zone an operator is TYPING IN, for a `<input type="datetime-local">`.
 *
 * The HTML format is local and offset-free, so the control's VALUE may not name a zone
 * (`localDatetimeInputValue` in `lib/datekeys.ts` builds it). That is a fact about the format and
 * not a licence for the SURFACE to stay silent: an operator scheduling maintenance, lifting a gate
 * for a bounded time, or backfilling history is choosing an instant, and a field labelled only
 * "Starts" tells them nothing about which clock it is read against.
 *
 * `func-truthful-rendering.md` §9 recorded that exemption with the justification "the surface that
 * owns the input says which zone it is typing in". **No surface did.** Six controls across five
 * views carried nothing but "Starts", "Ends", "Until", "from" — including the one the
 * specification named as the documented case — and the only mention of the zone anywhere near them
 * was a source comment an operator never sees. Reviewer P1 on the NFR-025 contract audit.
 *
 * THE OFFSET IS RESOLVED AT THE TYPED INSTANT, not at `now`, which is the same rule the rest of
 * this module obeys: a maintenance window entered for a date on the far side of a DST change is
 * genuinely in the other offset, and a hint taken from the current one would mislabel exactly the
 * window an operator is most likely to get wrong. An empty or half-typed control has no instant to
 * resolve against and falls back to now, which is the honest answer for a field with no value yet.
 *
 *   localInputZoneHint("2026-09-05T14:05")  ->  "local time (UTC+05:00)"
 *   localInputZoneHint("")                  ->  "local time (UTC+05:00)"   // resolved at now
 */
export function localInputZoneHint(value: string | null | undefined, zone?: string): string {
  const typed = value ? new Date(value) : null;
  const at = typed && !Number.isNaN(typed.getTime()) ? typed : new Date();
  return `local time (${offsetAt(at, zone)})`;
}

/**
 * The zone a `<input type="date">` is READ IN, where its value is a UTC calendar day.
 *
 * `lib/gateLedger.ts` defines these values as UTC days — the ledger's partition unit — and sends
 * them to the API as such. So an operator picking "From 05.09.2026" at UTC+05 is not asking for
 * their own day: they are asking for a UTC one, which began at 05:00 their time. The control
 * cannot carry a suffix, and the LABEL must therefore say it.
 *
 * There is no instant to resolve an offset at, and that is the point rather than a limitation: the
 * value IS a UTC day, so the answer is the same for every viewer and every date. It is a function
 * rather than a literal so the wording has one owner and the guard has something to find.
 *
 * Reviewer P1 on the NFR-025 contract audit, raised alongside the `datetime-local` surfaces: the
 * two are the same missing contract — a control whose value carries no zone must be labelled with
 * the one it is read in.
 */
export function utcDayInputHint(): string {
  return "UTC days";
}
