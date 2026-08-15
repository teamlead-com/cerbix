// Shared formatting for the service-reliability screens.
//
// `sealed_through` is the one number an operator has to read correctly, so both halves of it
// live here rather than being re-derived per view: the timestamp itself, and how far behind
// the wall clock it is. A healthy service is normally a couple of minutes behind — bucket 60s
// plus the late-arrival grace — so the lag is only worth showing once it exceeds that.

/** Below this the lag is the normal seal cadence, not something to point at. */
const HEALTHY_LAG_MS = 5 * 60 * 1000;

export function sealedLabel(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z");
}

/** Human lag, or "" while the service is sealing at its normal cadence. */
export function lagLabel(iso: string, now: Date = new Date()): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const ms = now.getTime() - d.getTime();
  if (ms < HEALTHY_LAG_MS) return "";
  return humanDuration(ms);
}

/** Exact lag, always — used where the delta itself is the subject. */
export function lagExact(iso: string, now: Date = new Date()): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const ms = now.getTime() - d.getTime();
  if (ms <= 0) return "0s";
  return humanDuration(ms);
}

export function humanDuration(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}
