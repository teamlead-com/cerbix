import createClient from "openapi-fetch";

import type { paths } from "@/api/schema";

// Same-origin: nginx (prod) / vite proxy (dev) forwards /api and /auth to the
// backend. `credentials: include` carries the cerbix session cookie.
export const api = createClient<paths>({
  baseUrl: "/",
  credentials: "include",
});
