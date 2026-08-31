// Compile-time assertions about the PUBLISHED wire schema.
//
// `schema.d.ts` is generated from `openapi.yaml`, so a loosened — or wrongly tightened — schema
// silently changes every client type with it and no runtime test can see the difference: the
// values still arrive. These assertions fail `vue-tsc`, which the build runs, so a regeneration
// that breaks a guarantee stops the build instead of shipping.
//
// They live HERE and not in a `.spec.ts` because `tsconfig.json` excludes `src/**/*.spec.ts` from
// type-checking: an assertion of this kind placed in a spec is never evaluated at all. That is how
// the first attempt at review [14]'s "contract assertion" was written, and it passed against a
// deliberately broken schema.
import type { components } from "./schema";

/** The keys of T that a value MUST carry — optional ones resolve to never and drop out. */
type RequiredKeys<T> = { [K in keyof T]-?: object extends Pick<T, K> ? never : K }[keyof T];

/** Fails to compile unless the argument is exactly `true`. */
type Assert<T extends true> = T;

/** Set equality, in both directions. `[X] extends [never]` so a union does not distribute. */
type SameKeys<Got, Want> = [Exclude<Want, Got>] extends [never]
  ? [Exclude<Got, Want>] extends [never]
    ? true
    : false
  : false;

type ApiToken = components["schemas"]["ApiToken"];

// FR-025 D12 and `domain.ApiToken`: a token read always carries these six and nothing else is
// guaranteed. `TestApiTokenAlwaysSerializesTheseKeys` marshals a zero token and fixes the same set
// on the server, so the two ends are pinned to one list.
//
// The equality is deliberately EXACT, in both directions, which the first version was not. It
// asserted only that the six were included, so a schema that additionally marked `project_id` or
// `last_used_at` required — fields the server omits when empty — would have satisfied it while the
// client was promised a key that does not arrive. Review [17] caught that the guarantee written in
// the commit message and the acceptance map was stronger than the guarantee actually enforced.
type ApiTokenAlwaysPresent = "id" | "org_id" | "name" | "role" | "actions" | "created_at";

export type ApiTokenRequiredIsExactlyTheServersSet = Assert<
  SameKeys<RequiredKeys<ApiToken>, ApiTokenAlwaysPresent>
>;
