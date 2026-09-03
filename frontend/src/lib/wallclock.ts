// The ONE time-rendering mechanism (func-truthful-rendering §8, FR-031 / NFR-025a, D-0235).
//
// IDENTITY IS UTC; PRESENTATION IS LOCAL. Every canonical grain, every requested range, every
// bucket and cell identity and all arithmetic stay UTC — a local-day grain would stop the cells
// decomposing the number printed above them, and two viewers in different zones would read
// different per-cell figures for one published availability. What is local is only what a human
// reads, and it always names the offset it was rendered in.
//
// TWO NAMED FUNCTIONS, NOT ONE FORMATTER (ruled at party [143]). There is deliberately no generic
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
