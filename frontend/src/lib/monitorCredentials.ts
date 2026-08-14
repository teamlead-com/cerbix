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
