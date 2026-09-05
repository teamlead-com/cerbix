// Date VALUES that are not renderings (`func-truthful-rendering.md` §9, AC-NFR-025c).
//
// NOTHING HERE IS SHOWN TO A HUMAN AS A TIME. These are map keys, comparison keys, wire values
// and HTML control values — the three uses that legitimately have no zone label, and the reason
// NFR-025c is a set of decisions rather than a rename. If you want a human to read a time, use
// `lib/wallclock.ts`; there is deliberately nothing here that could be mistaken for a formatter.
//
// They live in ONE module so the guard in `wallclock.spec.ts` can be absolute: outside
// `wallclock.ts` and this file, no product file builds a date out of a `Date` by hand. An
// allow-list of thirteen files with a reason each was the previous state, and an allow-list is
// what rots.

/**
 * A UTC calendar day as `YYYY-MM-DD` — a 90-day strip's map key, a same-day comparison, and the
 * `from`/`to` the ledger API takes, which are UTC days because that is the partition unit.
 *
 * NOT a rendering. The strip's tooltip shows this day to a reader, and it goes through
 * `utcDayLabel` to do it.
 */
export function utcDayKey(d: Date | string): string {
  const t = typeof d === "string" ? new Date(d) : d;
  return Number.isNaN(t.getTime()) ? "" : t.toISOString().slice(0, 10);
}

/**
 * The RFC 3339 instant the API takes. A WIRE value: it goes into a request body or a query
 * string and no human reads it. It lives here rather than at ~20 call sites so that
 * `toISOString` can be banned outright everywhere else — which is what closes the gap that let
 * `const s = d.toISOString(); s.slice(11, 16)` render an unlabelled clock for a year without the
 * NFR-025 ratchet ever counting it.
 */
export function isoInstant(d: Date): string {
  return d.toISOString();
}

/**
 * The instant `daysAgo` UTC days before `now` — the 90-day strips' cursor. UTC arithmetic, never
 * displayed: the three strips each wrote `dt.setUTCDate(today.getUTCDate() - i)` inline, which is
 * the same idiom the guard bans and the same duplication three times over.
 */
export function utcDayBefore(now: Date, daysAgo: number): Date {
  const d = new Date(now);
  d.setUTCDate(now.getUTCDate() - daysAgo);
  return d;
}

/** Midnight UTC of the day an instant falls in, in epoch ms — day arithmetic, never displayed. */
export function utcDayStart(d: Date): number {
  return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate());
}

/**
 * The value an `<input type="datetime-local">` holds: `YYYY-MM-DDTHH:mm` in the VIEWER's zone
 * and offset-free, because that is what the HTML format is. Labelling it would make the control
 * reject it.
 *
 * The zone is not hidden from the operator — the surface that owns the input says which zone it
 * is typing in, and what the API stores is RFC 3339.
 */
export function localDatetimeInputValue(d: Date): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}
