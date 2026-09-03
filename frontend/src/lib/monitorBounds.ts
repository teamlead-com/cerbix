// FR-030 (D-0234): the one bound the description form mirrors. Counted as CODE POINTS on both sides —
// `[...s].length` here, `utf8.RuneCountInString` on the server — and asserted against the server's own
// published value in monitorBounds.spec.ts so the two cannot drift.
export const MAX_MONITOR_DESCRIPTION = 200;

/** Length as the server counts it: Unicode code points, not UTF-16 units and not bytes. */
export const descriptionLength = (s: string): number => [...s].length;
