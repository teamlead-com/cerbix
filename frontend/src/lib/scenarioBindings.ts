// FR-028 stage 2 in the editor. A credential inside a synthetic scenario is a NAMED BINDING:
// the document carries `{{secret:<binding>}}`, and the project secret's NAME lives in an
// ordinary flat config key, `scenario_secret_<binding>_ref`.
//
// Every rule here is the SERVER's rule (`internal/domain/syntheticbindings.go`, enforced from
// `Monitor.Validate` on all four write surfaces). This file exists so the operator meets each
// one at the field instead of as a 400 after saving — the approved mock's second requirement —
// and it deliberately does not invent a rule of its own: a client-only refusal would be a rule
// nobody could discover from the API, and a client-only permission would be a lie.

export type ScenarioBinding = { name: string; secret: string };

/** The step shape the monitor form's builder holds. Structural only — no Vue types here. */
export type BindingStep = {
  url: string;
  headers: { k: string; v: string }[];
  body: string;
};

export const MAX_SCENARIO_BINDINGS = 16;

/** `^[a-z][a-z0-9_-]{0,39}$` — the server's grammar, character for character. */
export const BINDING_NAME_RE = /^[a-z][a-z0-9_-]{0,39}$/;

const PREFIX = "scenario_secret_";
const SUFFIX = "_ref";

// The finite set whose VALUE must be a placeholder rather than a literal. It is the only
// enforceable half of D7: a credential is not detectable by shape, so one pasted into a header
// nobody would call a credential header — or into a body — is legal and is NOT caught. The UI
// says that where it matters instead of implying a guarantee this set cannot give.
export const SECRET_CAPABLE_HEADERS: readonly string[] = [
  "authorization",
  "proxy-authorization",
  "cookie",
  "x-api-key",
  "api-key",
  "x-auth-token",
  "auth-token",
  "x-access-token",
  "access-token",
  "private-token",
];

const SECRET_CAPABLE = new Set(SECRET_CAPABLE_HEADERS);

export function isSecretCapableHeader(name: string): boolean {
  return SECRET_CAPABLE.has(name.trim().toLowerCase());
}

export function bindingRefKey(name: string): string {
  return `${PREFIX}${name}${SUFFIX}`;
}

export function bindingPlaceholder(name: string): string {
  return `{{secret:${name}}}`;
}

/** The inverse of bindingRefKey, and it rejects what the server rejects: a key that looks like
 *  a reference and does not parse is a typo, and a typo silently ignored is a binding the
 *  operator believes they declared. */
export function bindingFromRefKey(key: string): string | null {
  if (!key.startsWith(PREFIX) || !key.endsWith(SUFFIX)) return null;
  const name = key.slice(PREFIX.length, key.length - SUFFIX.length);
  return BINDING_NAME_RE.test(name) ? name : null;
}

/** Reads the declared bindings out of a stored config, sorted by name so the panel is stable
 *  across loads. A key that does not parse is surfaced by validate(), never dropped here. */
export function bindingsFromConfig(config: Record<string, string> | undefined): ScenarioBinding[] {
  if (!config) return [];
  const out: ScenarioBinding[] = [];
  for (const [key, value] of Object.entries(config)) {
    const name = bindingFromRefKey(key);
    if (name) out.push({ name, secret: value });
  }
  return out.sort((a, b) => a.name.localeCompare(b.name));
}

/** Malformed reference keys a stored config carries, so the editor can name them rather than
 *  quietly discard a declaration the operator made. */
export function malformedRefKeys(config: Record<string, string> | undefined): string[] {
  if (!config) return [];
  return Object.keys(config)
    .filter((k) => k.startsWith(PREFIX) && k.endsWith(SUFFIX) && bindingFromRefKey(k) === null)
    .sort();
}

/** Writes the bindings onto a config object. Every `scenario_secret_*` key is dropped first, so
 *  removing a binding in the editor actually removes it from the monitor — the store clears the
 *  matching `monitor_secret_refs` row on that same write. */
export function applyBindings(
  base: Record<string, string>,
  bindings: ScenarioBinding[],
): Record<string, string> {
  const config: Record<string, string> = {};
  for (const [k, v] of Object.entries(base)) {
    if (!(k.startsWith(PREFIX) && k.endsWith(SUFFIX))) config[k] = v;
  }
  for (const b of bindings) {
    const name = b.name.trim();
    const secret = b.secret.trim();
    if (name && secret) config[bindingRefKey(name)] = secret;
  }
  return config;
}

/** Every placeholder a string references, in order of appearance. */
export function placeholderNames(text: string): string[] {
  if (!text.includes("{{secret:")) return [];
  return [...text.matchAll(/\{\{secret:([^}]*)\}\}/g)].map((m) => m[1]);
}

export type ScenarioBindingIssues = {
  /** Form-level messages, ordered, in the server's own words where the server has words. */
  errors: string[];
  /** `${stepIndex}:${headerIndex}` → message, for the header row that carries it. */
  headerErrors: Record<string, string>;
  /** step index → message, for the URL field. */
  urlErrors: Record<number, string>;
  /** binding name → message, for its row in the mapping panel. */
  bindingErrors: Record<string, string>;
  /** `${stepIndex}:${headerIndex}` → the D7 residual: this value is NOT protected and cannot
   *  be, so the UI offers the inventory rather than pretending to refuse. */
  residualHints: Record<string, string>;
};

const empty = (): ScenarioBindingIssues => ({
  errors: [],
  headerErrors: {},
  urlErrors: {},
  bindingErrors: {},
  residualHints: {},
});

/** A value that looks like it could be a credential someone pasted: long, no whitespace, not a
 *  placeholder and not an interpolated variable. Used ONLY to offer a hint — never to refuse.
 *  Guessing at values is the rule D7 explicitly does not have, and a false positive here costs
 *  an operator nothing but a suggestion they can ignore. */
function looksPasted(value: string): boolean {
  const v = value.trim();
  if (!v || v.length < 20) return false;
  if (v.includes("{{")) return false;
  return !/\s/.test(v);
}

/**
 * validateScenarioBindings mirrors `domain.ScenarioBindings` and adds the two things only a
 * client can see: which project secrets actually exist right now, and where in the form each
 * refusal belongs.
 *
 * `secretsLoaded` gates the dangling-reference check for the same reason
 * `isDanglingSecretRef` takes it: before the inventory arrives every reference looks dangling,
 * and a warning during load teaches operators to ignore warnings.
 */
export function validateScenarioBindings(
  steps: BindingStep[],
  bindings: ScenarioBinding[],
  secretNames: string[],
  secretsLoaded: boolean,
  malformedKeys: string[] = [],
): ScenarioBindingIssues {
  const out = empty();

  for (const key of malformedKeys) {
    out.errors.push(`"${key}" is not a valid binding reference; a binding name is ${BINDING_NAME_RE.source}`);
  }

  const declared = new Map<string, ScenarioBinding>();
  const seenNames = new Set<string>();
  for (const b of bindings) {
    const name = b.name.trim();
    if (!name) {
      out.errors.push("A binding needs a name.");
      continue;
    }
    if (!BINDING_NAME_RE.test(name)) {
      out.bindingErrors[name] = `"${name}" is not a valid binding name: lower-case letters, digits, dash and underscore, starting with a letter.`;
      continue;
    }
    if (seenNames.has(name)) {
      out.bindingErrors[name] = `Binding "${name}" is declared twice.`;
      continue;
    }
    seenNames.add(name);
    if (!b.secret.trim()) {
      out.bindingErrors[name] = `Binding "${name}" has no project secret selected.`;
      continue;
    }
    if (secretsLoaded && !secretNames.includes(b.secret.trim())) {
      out.bindingErrors[name] =
        `Secret ${b.secret.trim()} no longer exists in this project. This monitor cannot dispatch until you pick an existing secret.`;
    }
    declared.set(name, b);
  }
  if (declared.size > MAX_SCENARIO_BINDINGS) {
    out.errors.push(`At most ${MAX_SCENARIO_BINDINGS} secret bindings are allowed.`);
  }

  const used = new Set<string>();
  steps.forEach((step, si) => {
    if (placeholderNames(step.url).length > 0) {
      out.urlErrors[si] = `Step ${si + 1} must not reference a secret in its URL: a URL reaches proxy logs, access logs and error text.`;
    }

    const seenHeader = new Map<string, number>();
    step.headers.forEach((header, hi) => {
      const at = `${si}:${hi}`;
      const canonical = header.k.trim().toLowerCase();
      if (!canonical) return;
      const prev = seenHeader.get(canonical);
      if (prev !== undefined) {
        out.headerErrors[at] = `Step ${si + 1} declares header "${canonical}" twice; one header is one location.`;
        return;
      }
      seenHeader.set(canonical, hi);

      const names = placeholderNames(header.v);
      if (isSecretCapableHeader(canonical)) {
        if (names.length !== 1 || header.v.trim() !== bindingPlaceholder(names[0])) {
          out.headerErrors[at] =
            `Step ${si + 1} header "${canonical}" must be exactly {{secret:<binding>}} — a credential is never a literal here.`;
          return;
        }
      } else if (looksPasted(header.v)) {
        out.residualHints[at] =
          "cerbix cannot tell a credential from data in this header, so nothing refuses it. Prefer a binding.";
      }
      for (const name of names) {
        if (!declared.has(name)) {
          out.headerErrors[at] = `Step ${si + 1} header "${canonical}" references binding "${name}", which nothing declares.`;
          continue;
        }
        used.add(name);
      }
    });

    for (const name of placeholderNames(step.body)) {
      if (!declared.has(name)) {
        out.errors.push(`Step ${si + 1} body references binding "${name}", which nothing declares.`);
        continue;
      }
      used.add(name);
    }
  });

  for (const name of declared.keys()) {
    if (!used.has(name)) {
      out.bindingErrors[name] =
        `Binding "${name}" is declared and never used — a reference nobody sends is a permission the monitor holds for nothing.`;
    }
  }

  return out;
}

/** True when the form has any binding issue at all, in any position. */
export function hasBindingIssues(issues: ScenarioBindingIssues): boolean {
  return (
    issues.errors.length > 0 ||
    Object.keys(issues.headerErrors).length > 0 ||
    Object.keys(issues.urlErrors).length > 0 ||
    Object.keys(issues.bindingErrors).length > 0
  );
}

/** The first issue, for the form-level error line that blocks submit. */
export function firstBindingIssue(issues: ScenarioBindingIssues): string {
  return (
    issues.errors[0] ??
    Object.values(issues.bindingErrors)[0] ??
    Object.values(issues.urlErrors)[0] ??
    Object.values(issues.headerErrors)[0] ??
    ""
  );
}

/**
 * The D10 contract, said at the button. A scenario carrying declared bindings is NOT testable
 * before it is saved: that endpoint builds an unsaved monitor and sends it in an ordinary job,
 * so a placeholder would travel to the target as the literal text `{{secret:name}}`. Returns
 * the reason to show, or "" when the scenario is testable.
 */
export function testBeforeSaveBlockedReason(bindings: ScenarioBinding[]): string {
  const names = bindings.map((b) => b.name.trim()).filter(Boolean);
  if (!names.length) return "";
  const list = names.length === 1 ? `binding ${names[0]}` : `bindings ${names.join(", ")}`;
  return `Save the monitor before testing it: the ${list} ${names.length === 1 ? "is" : "are"} resolved from the project inventory when a check is dispatched, and this path has no envelope to carry ${names.length === 1 ? "it" : "them"}.`;
}
