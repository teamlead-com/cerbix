// Compile-time assertions about the PUBLISHED wire schema.
//
// `schema.d.ts` is generated from `openapi.yaml`, so a loosened schema silently loosens every
// client type with it and no runtime test can see the difference — the values still arrive. These
// assertions fail `vue-tsc`, which the build runs, so a regeneration that drops a guarantee stops
// the build instead of shipping.
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

type ApiToken = components["schemas"]["ApiToken"];

// FR-025 D12: a token read ALWAYS carries `actions` — null when the role decides, a list when it
// narrows. Absent is not a value the contract permits, and `TestApiTokenAlwaysSerializesTheseKeys`
// proves the server never omits it. Review [14].
export type ApiTokenActionsIsRequired = Assert<"actions" extends RequiredKeys<ApiToken> ? true : false>;

// The rest of the always-serialized set, from the same Go test, so a regeneration cannot quietly
// make any of them optional either.
export type ApiTokenIdentityIsRequired = Assert<
  "id" extends RequiredKeys<ApiToken>
    ? "org_id" extends RequiredKeys<ApiToken>
      ? "name" extends RequiredKeys<ApiToken>
        ? "role" extends RequiredKeys<ApiToken>
          ? "created_at" extends RequiredKeys<ApiToken>
            ? true
            : false
          : false
        : false
      : false
    : false
>;
