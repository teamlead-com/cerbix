// FR-043 (func-recovery-threshold.md §8, approved mock docs/design/mock-recovery-threshold.html,
// 2026-11-16): what the monitor list, the monitor page and the form say about a monitor's two runs —
// the failures being confirmed on the way down and the successes being confirmed on the way up.
// Written once so the list and the page header read the same, and the form's warning is the mock's.
//
// Both labels are as fresh as the last load of the view (D13): the run advances without a status
// change and nothing is published without one.

/** The read-model fields the labels need; a full Monitor satisfies it. */
export interface MonitorRunState {
  status?: string;
  consecutive_failures?: number;
  failure_threshold?: number;
  consecutive_successes?: number;
  recovery_threshold?: number;
}

/**
 * "Confirming N/M…" — an up monitor with failures counted and no verdict yet. Today's rule and text,
 * moved here unchanged from MonitorsView so the monitor page can show it too (mock §5).
 */
export function confirmingLabel(m: MonitorRunState): string | undefined {
  const fails = m.consecutive_failures ?? 0;
  const threshold = m.failure_threshold ?? 1;
  if (m.status === "up" && fails > 0 && fails < threshold) return `Confirming ${fails}/${threshold}…`;
  return undefined;
}

/**
 * "recovering N/M" — a down monitor with a run of successes in progress. NOT gated on N < M: after
 * the threshold is lowered during a run (D10) the monitor is still down with a live run, and the
 * label reads e.g. "3/2" until the next check (§8).
 */
export function recoveringLabel(m: MonitorRunState): string | undefined {
  const run = m.consecutive_successes ?? 0;
  if (m.status !== "down" || run <= 0) return undefined;
  return `recovering ${run}/${m.recovery_threshold ?? 1}`;
}

/** The chip's tooltip, in the mock's words. */
export function recoveringTooltip(m: MonitorRunState): string | undefined {
  if (!recoveringLabel(m)) return undefined;
  const run = m.consecutive_successes ?? 0;
  const threshold = m.recovery_threshold ?? 1;
  if (run >= threshold) {
    return `${run} consecutive successful checks; the threshold was lowered to ${threshold} during the run, so the next successful check confirms the recovery. As of page load.`;
  }
  return `${run} of ${threshold} consecutive successful checks. The monitor stays down until ${threshold} in a row succeed. As of page load.`;
}

/** The form warns once announcing a recovery takes this long after the first success (D4). */
export const SOAK_WARN_AT = 600;

/** A duration in the largest whole units: "45 s", "10 min 30 s", "2 h". */
export function formatSoakDuration(seconds: number): string {
  if (seconds < 60) return `${seconds} s`;
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  return [h && `${h} h`, m && `${m} min`, s && `${s} s`].filter(Boolean).join(" ");
}

/**
 * The long-soak warning: shown when interval × (M − 1) reaches 600 s. Checks are not accelerated on
 * the way up (D4), so the M−1 checks after the first success run at the ordinary interval.
 */
export function soakWarning(intervalSeconds: number, recoveryThreshold: number): string | undefined {
  const more = recoveryThreshold - 1;
  const soak = intervalSeconds * more;
  if (more < 1 || soak < SOAK_WARN_AT) return undefined;
  return (
    `Recovery will be announced about ${formatSoakDuration(soak)} after the first successful check ` +
    `(${more} more check${more === 1 ? "" : "s"} × ${formatSoakDuration(intervalSeconds)}). ` +
    "Checks are not sped up during recovery."
  );
}

/**
 * The dependency hint: only with a parent AND a threshold above 1 — at 1 no parent can have a lower
 * threshold, so the sentence could never apply (owner's decision on the mock, 2026-11-16; spec §8).
 */
export function dependencyHintShown(dependsOn: readonly string[] | undefined, recoveryThreshold: number): boolean {
  return (dependsOn?.length ?? 0) > 0 && recoveryThreshold > 1;
}

/** push and composite carry no recovery threshold (D8): no field, no row, nothing sent. */
export function hasRecoveryThreshold(type: string | undefined): boolean {
  return type !== "push" && type !== "composite";
}
