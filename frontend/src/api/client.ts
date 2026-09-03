import createClient from "openapi-fetch";

import type { paths } from "@/api/schema";

// Same-origin by construction: in prod the cerbix binary serves the SPA, /api and /auth
// itself (no nginx anywhere — see internal/web/web.go); in dev the vite proxy forwards them.
// `credentials: include` carries the cerbix session cookie.
export const api = createClient<paths>({
  baseUrl: "/",
  credentials: "include",
});
