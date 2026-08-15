// One place that turns a secrets-API error code into something an operator can act on.
//
// The panel used to repeat a near-identical ternary chain at each call site, which is how
// two codes the server actually returns — `secret_quota` and `not found` — ended up
// unmapped: an operator who hit the per-project limit was shown the bare string
// "secret_quota" and left to guess. A message that names the rule but not the remedy is
// only marginally better than the raw code.

export type SecretAction = "add" | "update" | "delete";

const FALLBACK: Record<SecretAction, string> = {
  add: "Could not add the secret.",
  update: "Could not update the secret.",
  delete: "Could not delete the secret.",
};

/** Plural-aware "N monitor(s)", used by the in-use guards. */
export function monitorCount(n: number): string {
  return `${n} monitor${n === 1 ? "" : "s"}`;
}

export function describeSecretError(
  action: SecretAction,
  error: { error?: string; count?: number } | undefined,
  name: string,
  newName?: string,
): string {
  const code = error?.error;
  const count = error?.count ?? 0;
  switch (code) {
    case "feature_disabled":
      return "The secret inventory is disabled on this instance (secrets.enabled).";
    case "secret_exists":
      return `A secret named "${newName || name}" already exists in this project (secret_exists).`;
    case "secret_quota":
      // The limit is per project and deliberately low; the remedy is always the same, so
      // say it rather than making the operator find it.
      return "This project has reached its secret limit (secret_quota). Delete a secret you no longer reference to make room.";
    case "not found":
      return `Secret "${name}" no longer exists — someone else may have deleted it. Refresh to see the current list.`;
    case "secret_in_use":
      return `Cannot delete "${name}": it is referenced by ${monitorCount(count)} (secret_in_use). Re-point or remove those monitors first.`;
    case "secret_renamed_in_use":
      return `Cannot rename "${name}": ${monitorCount(count)} reference it from file-managed bundles (secret_renamed_in_use). Rename it in the file source instead.`;
    default:
      return code || FALLBACK[action];
  }
}
