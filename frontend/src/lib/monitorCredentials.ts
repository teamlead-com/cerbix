export type CredentialSelection =
  { mode: "value"; value: string } | { mode: "ref"; ref: string };

// applyCredentialSelection owns the exactly-one-of wire shape used by the monitor form.
// Delete both credential keys first so a type/mode switch can never retain a value from
// a previously-built config object.
export function applyCredentialSelection(
  base: Record<string, string>,
  selection: CredentialSelection,
): Record<string, string> {
  const config = { ...base };
  delete config.password;
  delete config.password_ref;
  if (selection.mode === "ref") {
    if (selection.ref) config.password_ref = selection.ref;
  } else if (selection.value) {
    config.password = selection.value;
  }
  return config;
}

// isDanglingSecretRef reports that a monitor points at a secret NAME the project no longer
// has. The reference is a name, and the delete guard only refuses while a reference is
// visible to it — a restore from an older bundle, or a project moved between environments,
// leaves one behind. It lives here rather than in the view because the form's <select>
// binds a value that is then absent from its options: the field simply renders blank, so
// without an explicit check the operator is told nothing at all.
//
// `loaded` is required: before the secrets list arrives every reference looks dangling, and
// warning during load would be noise that teaches operators to ignore the warning.
export function isDanglingSecretRef(
  mode: "value" | "ref",
  ref: string,
  secrets: { name?: string }[],
  loaded: boolean,
): boolean {
  if (mode !== "ref" || !ref || !loaded) return false;
  return !secrets.some((s) => s.name === ref);
}
