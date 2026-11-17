# Changelog

All notable changes to **cerbix** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.4.0] - Unreleased

### ⚠️ Read before upgrading

- **PostgreSQL monitors now use TLS as configured.**
  - **What changed.** Until this release the PostgreSQL probe connected in plaintext whatever `sslmode` said, and `verify-ca`/`verify-full` verified nothing. It now does what the setting says.
  - **What goes DOWN.**
    - **`require`, `verify-ca`, `verify-full` against a server without TLS.** This includes every postgres monitor saved since v0.1.5-beta.1 without an explicit choice, which was stored as `require`. The monitor's message reads `the server does not offer TLS, and sslmode is "require": set sslmode to "disable" if plaintext is intended`.
    - **`verify-ca` and `verify-full` against a certificate the system roots do not trust**, such as a private CA. Under `verify-full`, also a certificate not valid for the monitor's target name or IP. There is no custom-CA setting yet; use `require` (encrypted, not verified) meanwhile.
    - **Any other value**, which only a v0.1.0 beta or a direct database write could store: DOWN with `bad config: unsupported sslmode`.
  - **What does not change.** `prefer` or no value (monitors from the v0.1.0 betas) keeps working: TLS when the server offers it, plaintext otherwise.
  - **Before upgrading,** run the runbook's query 1 ("PostgreSQL monitor TLS"). It lists every postgres monitor with its target, region, `sslmode`, enabled flag and whether a monitoring-as-code file owns it.
    - Check TLS from each monitor's region.
    - Fix a file-managed monitor in its file.
    - Retired monitors are not listed. Disabled ones are, because they go DOWN when re-enabled.
  - **Rolling upgrades.** Upgrade the core roles (`api`, `scheduler`, `all`) first. Wait at least the pause the runbook's query 4 gives (at most one day; 0 when no monitor lacks the keys), then upgrade `worker` and `agent`.
    - **Why.** A new executor refuses a job that omits `sslmode` or `tls` on a body-bound carrier, and only a new core states those keys for old monitors.
    - **If you skip the pause,** those monitors show `probe_error` `unsupported_version` until their old jobs expire. Nothing else is affected.
- **Alert delivery and pull agents follow no redirect.**
  - **Channels and webhooks.** A webhook, Slack or Telegram channel, or an org webhook, whose URL answers a redirect now fails with `non-2xx status 30x`; it is retried and dead-lettered. Set the URL to the redirect's `Location`. To check a URL beforehand: `curl -sS -o /dev/null -w '%{http_code}' -X POST <url>`.
  - **Duplicates while one channel fails.** While one channel of a monitor fails, the others receive the same alert again on each retry (up to 10 attempts). That is how any failing channel behaves today: fix the URL or disable the channel.
  - **Pull agents.** A pull agent whose `--server` URL redirects (for example `http://` behind a proxy that upgrades to `https://`) stops polling until the URL is the one core answers on. Its log says so with the status: `agent_heartbeat_failed`, `agent_test_claim_failed`.
- **Alert mail: one-line encoded subjects, and mail addresses are checked.**
  - **Subjects.** The subject of every mail is its first line, at most 200 characters, encoded; the full text is in the body.
  - **Startup.** An instance whose YAML has `mail.smtp_host` with an empty `mail.from`, or a `mail.from` such as `Cerbix <alerts@example.com>`, will not start after the upgrade. The error names `mail.from`.
    - Today the first case silently runs with mail off, and the second fails every send at the server.
    - **Fix:** set `mail.from` to a bare address (`alerts@example.com`), or remove the `mail` block if mail is configured in Settings → Mail.
    - This applies even where Settings → Mail is saved and the YAML block is no longer used.
  - **Addresses.** They must be single bare ASCII addresses of at most 254 characters, and an email channel must name at least one recipient. The subscribe form, the email channel form and Settings → Mail answer 400 to any other. Addresses already stored are not changed.
  - **Before upgrading,** run the runbook's query 2. It lists subscribers, email-channel `from`/`to` elements and the Settings → Mail `from` that will be refused. Fix them where they live:
    - **Subscriber:** delete it from the status page's subscriber list (`DELETE /api/v1/status-pages/{pageID}/subscribers/{subscriberID}`); the person subscribes again with a valid address.
    - **Email channel:** edit the channel.
    - **Instance sender:** Settings → Mail.
- **Delivery failures no longer quote channel URLs.**
  - **What remains from before the upgrade.** Log lines and outbox `last_error` text written before the upgrade may hold channel URLs and Telegram bot tokens. Run the runbook's query 3 once after the upgrade to blank the stored text.
  - **Rotate** any token whose logs left your control.
- **Incident impact is now derived, and some service incidents open lower than `major`.**
  - **Two behaviour changes (D6).** A service that pages on `degraded` now opens its new incidents as `minor`, not `major`. A service with `page_on_unknown` now opens an incident for `unknown` as `none` ("Impact not specified"), not `major`. To keep `major`, set the service's incident impact after the upgrade (Service → Alerting); the runbook's "Incident impact" section has the query that lists these services. Incidents already open at the upgrade keep their impact and never rise (D12).
  - **Extra subscriber mail while versions overlap (D16).** While old and new processes run side by side, or after a rollback with rise events still queued, a process that predates this version emails status-page subscribers on every impact rise, with today's subject, not only on a rise to `critical`. The roles that send it are `all`, `api` and `scheduler`.
  - **Rollback and Monitoring-as-Code.** An older binary refuses a Monitoring-as-Code bundle that declares `alerting.impact_by_state`. Remove the key from your bundles before rolling back.

### 🔒 Security

- **A bearer JWT no longer signs in as a person.** An OIDC token that another application obtained for a user was accepted on the API's bearer path as that user, with their memberships. The JWT bearer path now accepts only client-credentials tokens of the clients you list under the new **Machine clients** OIDC setting (Settings → Authentication, or `oidc.machine_clients` in the YAML). Tokens carrying a user-session marker (`nonce`, `sid`, or an Okta `cid` other than `sub`) are refused, and a token never authenticates a person's account. Users now have a kind, `human` or `machine`, shown as a badge in the member lists. A machine cannot be a global admin. If a person signs in to an identity a machine token created, that identity becomes theirs and everything granted to the machine is removed and audited (D-0276).
- **A password reset ends every session of the account.** Before, sessions that were open before the reset stayed valid. Changing your password still keeps your current session and ends the others, and the change and the sign-out now happen together or not at all. API tokens are not affected by either.
- **Bounded authentication input.** Passwords longer than 1024 bytes are refused. Sign-in and reset requests are limited to 4 KiB and other API requests to 1 MiB. Sign-in and password reset are rate-limited per address (checked before the request is read), per address and email, and per email after repeated failures. The password-reset confirmation step was not rate-limited before.
- **Probe targets can no longer smuggle bytes onto the wire.** A WebSocket target containing a line break could inject headers into the handshake, and a RabbitMQ management `path` such as `@10.0.0.9:8080/x` could send the request, with its Basic-Auth credentials, to another host. Every probe target is now parsed with its protocol's rules when it is saved (API, test-connection, Monitoring-as-Code) and again before it is dialled (D-0277).
- **WebSocket monitors verify TLS.** The handshake over `wss://` or `https://` used to accept any certificate. It now verifies it unless the monitor opts out with `tls_skip_verify` ("Skip certificate verification" in the monitor form).
- **Redirects no longer carry credentials across origins.** When a probe followed a redirect to another host, port or scheme, headers it was given could reach that host. This included a two-hop redirect that leaked an async canary's binding-backed headers. Now every header the first request carried is dropped once the chain leaves its origin, `Referer` is never sent, and `https → http` downgrades, credentials in a `Location` and non-http schemes are refused. A 307/308 that would carry a request body to another origin is refused. For the async canary this applies only when the body holds a secret.
- **A token can no longer create or revoke API tokens.** Creating or revoking a service-account token now needs a person's session: a token, or an OIDC machine identity, gets 403. The tooltip on a machine's disabled "Grant admin" now says that global administration is for people and that automation gets organization or project rights.
- **Status pages follow the token's allow-list, and an unlisted page's secret link goes to organization admins only.** Reads of an organization's status pages now need the new `status_page:read` action. Every role holds it, project-only members included, so people see no change. An API token with an allow-list that does not name it now gets 403. Before, any token of the organization could read every page, including the secret link of an unlisted one. Only `org:manage` now receives `unlisted_token`. Other readers get `has_unlisted_token: true`, and the status-page list opens such a page through the members-only preview. The adjacent reads now honour the allow-list too: project, project list, organization, organization list, live events and `/me`. Before, these routes answered 500 to an API token on `/me`, `/organizations` and `/events` (D-0278).
- **Cross-origin requests can no longer change anything with your session.** A browser request from another origin — including a sibling host such as `wiki.company.com`, which `SameSite=Lax` let through — that tries to change state is refused with 403 before it reaches cerbix's handlers, on the API and on sign-in, sign-out and password reset. Requests with an `Authorization: Bearer` token are not affected, and a request that presents one is now authenticated by it alone, never by a cookie sent alongside. JSON bodies must be sent as `application/json`. `GET /auth/logout` no longer signs you out.
- **An unlisted status page's link no longer leaks through `Referer`.** Public status pages and their feeds are served with `Referrer-Policy: no-referrer`, the rest of cerbix with `strict-origin-when-cross-origin`, and the support link opens with `noreferrer`.
- **More token boundaries.** An API token now gets 403 on `POST /api/v1/me/password`, `/me/totp/enable` and `/me/totp/disable` (two of them answered 500 before) and on `GET /api/v1/search` when its allow-list omits `project:read`. The cross-origin check no longer exempts `/api/v1/agent/` on instances without pull agents, and it decides which requests are API requests by asking the router itself — the same matching, escaped characters and `%61pi`-style spellings included — rather than by comparing strings.
- **Subscribers of a page made internal no longer receive incident mail.** Making the page public or unlisted again resumes it; the subscriptions are kept.
- **Enabling two-factor authentication can no longer race a disable or a new enrollment.** If 2FA was disabled, or enrollment restarted, while a confirmation was in flight, the confirmation could switch 2FA on with an empty secret — whose codes anyone could compute — or with a secret the person's app never saw, and an enrollment could switch a just-enabled 2FA off. Now 2FA is enabled only with the exact secret the code was checked against, and an enrollment never replaces the secret of an enabled 2FA.
- **A two-factor code signs in only once.** A TOTP code was accepted again for as long as it stayed valid (up to 90 seconds), so a code someone saw could be reused. The sign-in now records the code's time step and refuses it, or any older code, afterwards — also when two sign-ins arrive at the same moment. The code that confirms 2FA enrollment can still sign in once, and enrollment itself can be confirmed only once (D-0279).
- **Security headers on every page.** cerbix now sends `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` (`SAMEORIGIN` on public status pages, which only cerbix's own pages may frame) and a Content-Security-Policy. In this release the policy is **report-only**: browsers report violations in the console and block nothing. The next release enforces it. The theme script that ran inline before the page paints is now a file, `/theme-init.js`.
- **The post-logout URL and the branding support link must be real links.** The OIDC `post_logout_url` must be a local path or an `http(s)` URL with a host, and branding's `support_url` an `http(s)` URL with a host or a `mailto:` address — anything else, such as a `javascript:` URL, is refused with 400 when saved. A value saved before this release that breaks the rule is not served: the sign-out goes to `/` and the support link is hidden.
- **Probes no longer read without limit from the target.** The SSH, Redis and WebSocket probes read at most 4 KiB (16 KiB for the WebSocket handshake), the PromQL probe 1 MiB of the answer, and the PostgreSQL and MySQL probes 1 MiB per connection, and then report the target as down; an SSH banner counts only as a complete line, instead of buffering whatever a server sends until the timeout. The HTTP server now drops a client that takes longer than 5 seconds to send its headers and closes keep-alive connections after 60 seconds idle; live updates and pull agents are not affected.
- **`/metrics` and `/readyz` can leave the public port.** The new `server.ops_listen` setting serves them on a second address; the application port then answers them 404, and `/healthz` stays on both. The production example and compose file use it, on the host's loopback (`CERBIX_OPS_BIND`, port 9091). `/readyz` now answers with a reason code instead of internal error text, and counts the message broker: an `api`, `scheduler` or `worker` that has lost RabbitMQ answers 503 `broker_unreachable`, and `cerbix_ready` agrees with `/readyz` in every state.

- **PostgreSQL monitors encrypt and verify as `sslmode` says, and nothing outside the monitor reaches the connection.** The probe now asks for TLS under `require`, `verify-ca`, `verify-full` and `prefer`, verifies the certificate under `verify-*`, and builds its connection from the monitor alone: no `PG*` environment variable, `~/.pgpass`, service file or client certificate of the prober's user changes where, how or as whom it connects, and an `sslmode` outside the list is refused before any connection (D-0280).
- **A credentialed job can no longer be downgraded in transit.** Removing `sslmode` or `tls` from a sealed postgres, mysql, redis or RabbitMQ-management job kept its envelope valid and made the probe send the credential in plaintext. Such a job is now refused, as `unsupported_version`, without affecting the executor's readiness.
- **Delivery never follows a redirect**, so a receiving endpoint can no longer forward an incident with its signature to another host, collect the channel URL from `Referer`, or have a 301/302/303 counted as delivered. The pull agent's token no longer reaches a redirect to another port or scheme on the same host.
- **A failed delivery names a reason, never its URL.** Telegram bot tokens, Slack webhook paths and tokened webhook URLs no longer appear in the outbox's `last_error`, the log or the dead-letter list.
- **A name or title can no longer add headers to mail.** A monitor, service or project name, an incident title (including an Alertmanager `summary`) or a status-page title with a line break could add headers to, or replace the body of, mail sent from your instance's sender. Every mail now has a fixed header set and a one-line, encoded subject.
### ✨ Added

- **Recovery threshold: a monitor stays down until it has really recovered.** A monitor's new **Recovery threshold** (1–10, default 1) is how many checks in a row must succeed before a down monitor is up again. Set it in the monitor form (under Confirmations), as `recovery_threshold` in the API, or in a Monitoring-as-Code bundle. Until the run completes the monitor stays down: no "recovered" notification, the auto-incident stays open, no "still down" reminder, and no escalation step fires. A failed check starts the count again. So a target that answers every other request no longer sends a false recovery and opens a fresh incident minutes later. The monitor list and the monitor page show `recovering N/M` beside the Down pill, and the monitor page now also shows `Confirming N/M…` on the way down. A recovery that took more than one success says so in the notification and in the incident's resolving update: `Recovered: 3/3 consecutive checks passed; first OK at 2026-11-16T14:04:05Z`. The email subject and the webhook JSON keys are unchanged. Checks are not sped up during a recovery, so with a threshold above 1 "recovered" arrives later, about interval × (threshold − 1) after the first good answer; the form warns when that reaches 10 minutes. The threshold changes no check result. With the confirm phase off it changes no SLA, burn rate, service reliability, gate decision or status-page bar; with the confirm phase on, a monitor still held down is not checked at the faster confirm rhythm. Push and composite monitors take only 1 (D-0281).
- **Incident impact is declared by the service, and only rises.** A service's new **Incident impact** setting (Service → Alerting, `impact_by_state` in the API and in a Monitoring-as-Code bundle's `alerting` block) says what each paging state means for the people who use the service: by default `degraded` is Minor, `down` Major and `unknown` Not specified. The service's incident opens at that impact, and if the service gets worse while the incident is open the impact rises — never falls — with a public "⬆ Impact raised" note on the timeline, an `incident.updated` webhook carrying `impact_raised`, and a mail to status-page subscribers when it reaches Critical. The incident keeps the mapping it opened with, so editing the mapping changes only later incidents, and the incident page states why the impact is what it is ("declared by service payments for state down", "project default for monitor incidents", …). Monitor incidents open at the project's new **Impact of monitor incidents** (Settings → Project → Incidents, `PATCH /api/v1/projects/{projectID}`; default Major, as before). Nobody can change an automatic incident's impact by hand. Impact `none` now reads "Not specified" / "Impact not specified" everywhere in the web app, and a service incident's opening note names the state it opened for. Impact changes no SLA, budget, burn rate, gate decision, delegation or escalation (D-0282).

### 🩹 Fixed

- **Synthetic monitors can be paused and resumed again.** Pause/Resume, and any edit that did not resend the scenario, answered `synthetic: config.scenario is required`.
- **Pausing or renaming a monitor no longer rewrites its stored settings.** A partial edit used to re-validate and rewrite the stored config: a PostgreSQL monitor from the v0.1.0 betas was silently switched to `sslmode: require`, or the edit failed with an internal error when it held `prefer`. With the secret inventory switched off, a monitor that uses an inventory secret still cannot be edited at all, as before.
- **SLA report and other multi-line alert mail has a one-line subject.** The whole report used to be the subject.
- **The production example config starts again.** `docker/config.prod.yaml` mentioned `${VAR}` in a comment, and because the config loader expands variables everywhere in the file, comments included, startup failed with `undefined environment variable(s): VAR`. The comment is now escaped. `docker/config.example.yaml` had the same comment (`${ENV_VAR}`), so `cerbix serve --config docker/config.example.yaml` failed the same way; it is fixed too.
- **The new-monitor form no longer resets a timeout or interval you typed.** The instance's monitor defaults arrive a moment after the form opens; when they arrived after you had typed, they replaced your values. They now fill only the fields you have not changed.

### Upgrade notes

- **Client-credentials callers get 401 until you list their clients.** With **Machine clients** empty (the default), the JWT bearer path is off. Every refused token is logged as `oidc_bearer_refused` with the client it names and a reason, and counted in `cerbix_oidc_bearer_refused_total{reason}`. Add those client ids to the setting. Each listed client must be service-account-only at your identity provider. On Keycloak, keep "Use refresh tokens for client credentials grant" off, or its tokens carry `sid` and are refused. cerbix API tokens are unaffected.
- **A service account whose token carries a real email address** (for example a Google Cloud service account) was filed as a person by the upgrade and is refused with `reason=subject_is_human`. Delete that user in Settings → Users and let its next token re-create it as a machine, then grant its memberships again.
- **Password resets now sign the account out everywhere**, including the browser that started the reset.
- **Migration 00112** runs on startup. It adds `users.kind` and fills it in: an identity a client-credentials token created (a never-signed-in `<sub>@clients` account with no password that is not a global admin) becomes `machine`, and every other user stays `human`. It also adds `oidc_settings.machine_clients`, which starts empty.
- **Request bodies on the authenticated API are capped at 1 MiB**, except the Alertmanager receiver (`POST /api/v1/projects/{id}/alerts/alertmanager`), which accepts up to 16 MiB for grouped webhooks.
- **`POST /api/v1/me/password` answers 500 when it cannot sign out your other sessions**, and the password is then left unchanged. It used to answer 204 with the old sessions still open.
- **API responses carry new fields.** Users and members now include `kind` (`human` or `machine`), and the OIDC settings include `machine_clients`. `PATCH /api/v1/admin/users/{id}` answers 400 to a request that makes a machine identity a global admin.
- **Startup refuses invalid local-login settings:** a negative `local.login_rate_limit_per_minute`, a `local.min_password_length` above 256, or a `security.admin_password` longer than 1024 bytes.
- **Migration 00113** keeps existing WebSocket monitors working. One with a `wss://` or `https://` target gets `tls_skip_verify: "true"`, because it never verified before. One without a scheme gets the `ws://` it was dialled with. A tab or line break before the scheme no longer makes it look scheme-less.
- **Monitoring-as-Code bundles must declare `tls_skip_verify`.** A WebSocket monitor with a self-signed endpoint needs `settings: { tls_skip_verify: "true" }` in its bundle. Otherwise the next apply resets the value the migration set, and the monitor goes DOWN with a `tls:` message.
- **Probe targets are validated strictly.** A monitor that breaks these rules is refused when it is saved (API, test-connection, Monitoring-as-Code), with a message that names the rule, and fails at the probe with `bad target`. The rules:
  - **Every type:** no control character (a line break, tab, NUL or DEL) anywhere in the target.
  - **WebSocket:** new targets need a `ws://`, `wss://`, `http://` or `https://` scheme.
  - **SSH, Redis and RabbitMQ AMQP:** `host` or `host:port`, with an optional port.
  - **HTTP:** an `http://` or `https://` URL with a host, and a port between 1 and 65535 if one is written. A target that does not parse is refused; before, it was stored, and one carrying `user:password@` slipped past the credential check. The HTTP probe now checks its target too, so a refused one fails with `bad target` before any request. A failed request never puts the URL in the message.
  - **PromQL:** the Prometheus base URL, with no `?query` or `#fragment`. A path prefix such as `https://gw/prometheus` is kept, including one that ends in an escaped slash such as `/vhosts/%2F`. The RabbitMQ management base URL keeps it the same way.
  - **RabbitMQ management target:** `host[:port]`, or an `http://` or `https://` URL (any letter case) with a host, a port between 1 and 65535 if one is written, and no credentials, query or fragment. With `tls: "true"` it must be `https://`; that used to fail only at the probe.
  - **RabbitMQ management `path`:** must start with a single `/`. A relative path such as `api/overview` is refused instead of being given a `/` silently. It must use valid percent-encoding and reaches the server exactly as written, so `/api/aliveness-test/%2F` (the default vhost) works as before.
- **Migration 00114** writes the `/` into every existing RabbitMQ monitor whose `path` is relative, so the request it sends is unchanged.
- **Migration 00115** removes spaces, tabs and line breaks from the start and end of WebSocket, SSH, Redis, RabbitMQ, PostgreSQL and MySQL targets. Those probes used to trim them, so the monitors keep working and can still be edited. A control character inside a target is left alone; no probe ever accepted one. The one exception is an HTTP target with a line break, tab or DEL after its `#`. That target worked, because the fragment is never sent, so 00115 removes those characters from the fragment.
- **Monitoring-as-Code bundles are not migrated.** A bundle with a relative RabbitMQ `path`, a WebSocket target without a scheme, or a target with a line break is refused at the next apply (`domain_invalid`, naming the rule) until you edit it.
- **Redirect-dependent probes may change state.** A monitor that relied on a credential following a redirect to another origin now meets that origin without the credential. A synthetic step that POSTed through a 307 to another origin is now refused with `redirect refused: request body to another origin`. Point the step's `url` at the final origin.
- **Automation that issued or revoked API tokens with a token now gets 403.** Do it from a person's session.
- **Tokens with an allow-list need `status_page:read` to read status pages.** A token issued with an `actions` list before this release cannot list the new action and gets 403 on the organization's status pages; issue a new token whose list names it. Tokens without a list are unaffected. A token whose list names neither `org:read` nor `project:read` now gets 403 on `GET /api/v1/organizations/{id}` and an empty `GET /api/v1/organizations`, and one without `project:read` gets 403 on projects and on `GET /api/v1/events`. `GET /api/v1/me` answers 403 to every token: it describes a person.
- **403 and 415 where there used to be 200.** A state-changing request a browser marks as cross-origin is 403 on `/api/*` and `/auth/*`, unless it carries a Bearer token. A request body that is not `Content-Type: application/json` is 415 on every API route that reads one and on `/auth/local/login`, `/auth/local/reset/request` and `/auth/local/reset/confirm`. **Scripts that call `/auth/local/login` without the header must add `-H 'Content-Type: application/json'`.** The refused request does not count against the sign-in rate limit. A request with a cookie and a Bearer header is now judged by the Bearer alone: a stale or wrong Bearer beside a valid session is 401.
- **Tokens get 403 on the account routes and, without `project:read`, on search.** `POST /api/v1/me/password` and `/api/v1/me/totp/*` describe a person; `GET /api/v1/search` needs `project:read` in a token's list. A gate request whose body is only whitespace must still be sent as `application/json` (415 otherwise); a request with no body needs no type, including one sent chunked or over HTTP/2 without a length.
- **`GET /auth/logout` is gone** (405). Sign out with `POST /auth/logout`, as the web app does. A link or bookmark to the GET route no longer signs anyone out.
- **Behind a reverse proxy that replaces `Host`, plain-HTTP sign-in fails with 403.** Symptom: the login form, and every other change, answers 403 `cross-origin request refused`. Browsers send `Sec-Fetch-Site` only over HTTPS (and to `localhost`); without it cerbix compares the browser's `Origin` with the `Host` it receives, and a proxy that rewrites `Host` to its upstream name (for example nginx `proxy_pass http://cerbix:8080;` without `proxy_set_header Host`) makes them differ. Fix: pass the browser's Host through (nginx: `proxy_set_header Host $http_host;`; Traefik and Caddy do this by default), or serve cerbix to browsers over HTTPS. See the runbook, "Request forgery, the Bearer rule and `Referer`".
- **A proxy that forwards its own `Authorization: Bearer` token to cerbix breaks the web app with 401.** A request that carries a Bearer header is now authenticated by it alone and the session cookie is not read, so an authenticating proxy that passes its token upstream (oauth2-proxy `--pass-authorization-header`, or `--set-authorization-header` with nginx copying the header) logs every page out. Fix: do not forward the header to cerbix — turn those options off, or `proxy_set_header Authorization "";`. A proxy's `Basic` or `Negotiate` header is not affected.
- **`POST /auth/logout` answers 200 with JSON instead of a 302 redirect.** The body is `{"redirect_to": "…"}` — the configured post-logout URL, or `/`. The web app navigates there itself. A script or third-party client that followed the redirect must read `redirect_to` instead; the session is ended either way.
- **The Content-Security-Policy is report-only in this release and will be enforced in the next one.** Before upgrading to that release, open the web app and a public status page with the browser's developer console open: any `[Report Only]` Content-Security-Policy message is something enforcement will block. The usual causes are a branding logo served over `http://` (serve it over `https://`), or a proxy or extension that injects a script. Embedding a public status page in another site's `<iframe>` is refused now (`X-Frame-Options: SAMEORIGIN`).
- **`server.ops_listen` moves `/metrics` and `/readyz`.** It is off by default, so nothing changes until you set it. `docker/config.prod.yaml` now sets `ops_listen: ":9091"` and `docker-compose.prod.yml` publishes it as `127.0.0.1:9091` (`CERBIX_OPS_BIND` in `.env`). If you deploy from those files, point Prometheus and readiness checks at port 9091, because port 8080 now answers 404 on both paths. It must differ from `server.listen`, or startup fails.
- **`/readyz` reports a code, and the broker.** A 503 body is now `{"status":"not_ready","error":"<code>"}` with `starting`, `shutting_down`, `database_unreachable`, `broker_unreachable`, `credential_unavailable`, `service_wedged` or `alert_evaluation_stalled`; the details are in the log line `not_ready`. A check that matched the old text (`database unreachable`) must match the code. Distributed `api`, `scheduler` and `worker` processes now fail readiness while RabbitMQ is unreachable, and recover when it reconnects.
- **`POST /api/v1/me/totp/enroll` answers 400 while 2FA is enabled, and `/me/totp/enable` answers 400 when the pending enrollment changed meanwhile** ("start enrollment again"). Disable 2FA before enrolling again, as the web app already requires.
- **Startup refuses two more config mistakes.** An `oidc.post_logout_redirect_url` that is not a local path or an `http(s)` URL with a host (it used to be ignored at sign-out), and a `server.ops_listen` on `server.listen`'s port written another way (`0.0.0.0:8080`, `[::]:8080`, `127.0.0.1:8080` or `:08080` against `:8080`; `[::ffff:127.0.0.1]:8080` against `127.0.0.1:8080`; `Localhost` against `localhost`), with surrounding spaces, or without a port. `server.listen` must be `host:port` too, as the server always needed. In both, the port must be a number from 1 to 65535 — `:0`, `:99999` and a service name such as `:http` now stop startup — and a host in brackets must be an IP address.
- **Outside Docker, write `ops_listen: "127.0.0.1:9091"`.** The production example's `":9091"` listens on every interface inside the container; only the compose file's `CERBIX_OPS_BIND` keeps it on the host's loopback. With `network_mode: host` or a plain binary, nothing does.
- **PostgreSQL and MySQL monitors whose probe query returns more than 1 MiB now fail.** The probe reads at most 1 MiB per connection; use a query with a small answer (the default is `SELECT 1`).
- **Migration 00116** adds `users.totp_last_step`. It is empty for everyone, so every current code still works once.
- **Saving branding or the OIDC settings with an invalid `support_url` or `post_logout_url` now answers 400.** Fix the value in Settings → Branding or Settings → Authentication.
- **Migration 00117** adds `recovery_threshold` (1 for every existing monitor), `consecutive_successes` and `recovery_started_at` to `monitors`, with CHECKs. Nothing behaves differently until you raise a threshold. Monitor responses carry the three new fields. A bundle that omits the key or sets it to 1 is not re-applied by the upgrade.
- **Migration 00118** adds a trigger on `monitors`: when an older binary (a role not yet upgraded, or after a rollback) moves a monitor out of down, a recovery run in progress is ended instead of the write being refused. Under an older binary the recovery threshold does not apply — a monitor goes up on its first success — and no check result is lost; back on this release the threshold applies again. A failed check recorded by an older binary keeps the monitor down and leaves a run in progress as it was, so back on this release the next success continues it. Nothing needs to be set back before rolling back.
- **Migration 00119** adds `services.impact_by_state` (empty means the default mapping), `projects.default_incident_impact` (`major` for every existing project) and `incidents.impact_basis` (empty for every existing incident), with CHECKs. Project responses carry `default_incident_impact`, the service alerting responses `impact_by_state`, and the incident detail `impact_basis`. A service alerting PATCH that sends `impact_by_state` must send all three keys (`degraded`, `down`, `unknown`), or it answers 400.

## [v0.3.9] - 2026-11-03

### 🔒 Security

- **Go toolchain and `golang.org/x/net` patched.** `govulncheck` found 13 vulnerabilities reachable from cerbix's code in the Go standard library (`net/http`, HTTP/2, `crypto/tls`, `crypto/x509`, `html/template`, `net/textproto`) and in `golang.org/x/net`. Release binaries now build on Go 1.26.10 and the Docker image on Go 1.27.3, both of which fix all of them; `golang.org/x/net` is 0.62.0 and `golang.org/x/crypto` 0.58.0. Binaries and images of earlier versions, including the Go 1.27.0 image, carry these vulnerabilities — upgrading replaces them (D-0275).
- **`vite` 8.3.5.** Clears a high-severity advisory set in `vite` 8.2.0–8.2.2 and `launch-editor` flagged by `npm audit` on the production dependency tree. The embedded SPA is rebuilt with it.

### ✨ Added

- **A Project Admin manages the members of their own project.** Settings → Project → **Members** (visible to Org Admins, global admins and the Project Admin of the current project) lists the project's members — change a role, remove someone, invite by email with a project role (Project Admin, Editor or Viewer) — and shows, read-only, who reaches the project through the organization. New endpoints `GET/POST /api/v1/projects/{id}/members` and `PATCH/DELETE /api/v1/projects/{id}/members/{membershipId}`. A Project Admin cannot touch organization-wide memberships, grant an organization role or reach another project. API tokens and client-credentials service accounts are refused on these endpoints (403); the `project:manage` action still grants a token nothing (D-0273).

### ⚡ Performance

- **Dashboard and status pages load faster.** Every availability figure that leaves out maintenance windows — monitor and project SLA, burn alerts, the status page's 90-day uptime and strips, the daily rollup — used to check each heartbeat against the maintenance table separately; on a live install that check was nearly all of the database work behind a status page. The windows that apply are now collected once per SQL statement, so that work no longer grows with the number of heartbeats. The figures are unchanged (D-0271). How much faster a page gets depends on the install; on the pilot, prototypes of the new statements measured about 3× faster for the status page's uptime query (not an end-to-end page benchmark).
- **The dashboard asks only for what it shows.** `GET /monitors/{id}/sla` and `GET /projects/{id}/sla` accept `?window=` (for example `?window=30d`) and then compute only those windows; without it they return all four, as before. The dashboard requests `30d`.

### 🩹 Fixed

- **"Today" no longer depends on the database session's time zone.** The live "today" figure in the daily availability strips (status page, project and monitor) started at midnight in the PostgreSQL session's `TimeZone` instead of UTC midnight. On a database whose time zone is east of UTC, today also counted the last hours of yesterday — already counted in yesterday's bar; west of UTC, today lost its first hours. It now starts at UTC midnight, like the daily bars. Installs whose PostgreSQL time zone is UTC are unaffected.
- **"Last active" no longer forgets users.** The instance user list and the organization member list showed "never" for almost everyone: the column was derived from the user's current sessions, which a sign-out, the 24-hour session expiry or the expired-session cleanup removes. Users now carry their own last-seen time, set when they sign in and refreshed while they use the UI (at most every 5 minutes). Service-account tokens and client-credentials JWTs do not count as activity (D-0272).
- **Changing or removing a member that was removed a moment earlier answers 404, not 500.** The organization member endpoints returned an internal error when the membership disappeared between the check and the write (for example deleted together with its project). Adding a project member now tells a duplicate (409) from an unknown user (400) instead of one generic 400. Audit entries of project-scoped memberships name the project.

### Upgrade notes

- **One schema migration, `00111`**: adds a nullable `users.last_seen_at` column (no table rewrite; applied automatically when any role starts, under a brief exclusive lock on `users`) and fills it from each user's newest remaining session. Users whose sessions were all removed before the upgrade show "never" until they next sign in — that history no longer exists.
- No configuration change.
- **Building from source needs Go 1.26.10 or later** (was 1.25.13). With the default `GOTOOLCHAIN=auto` an older Go downloads 1.26.10 by itself.

---

## [v0.3.8] - 2026-10-08

### ✨ Added

- **Incident history on status pages.** A status page now lists its ten most recent past incidents, and a **View incident history** link opens a history page that walks the last 90 days month by month (calendar months in UTC), up to 50 incidents at a time with **Show more**. Unlisted pages keep their token through the link. The page no longer grows with every resolved incident: what a page or history response carries is bounded, its incident lists stop early on the new index, and uncached public history requests are limited (HTTP 429 with `Retry-After` beyond the limit). The per-month counts still read the 90-day window (D-0270).

### Changed

- **API: `recent_incidents` on a status page render holds at most the 10 newest past incidents**, with a new `recent_incidents_more` flag; the rest are served by `GET /api/v1/public/status-pages/{slug}/history` and `GET /api/v1/status-pages/{pageID}/history`. A client that read `recent_incidents` as "every incident resolved in the last 90 days" should page through the history instead. Feeds, webhooks and subscriptions are unchanged.

### 🩹 Fixed

- **Reliability readouts are no longer cut off by their card.** Hovering a bucket near either edge of a service's reliability strip or segment lane showed only part of its readout, because the card clips its contents. The readout is no longer clipped by the card and stays inside the window whenever it fits there, moving in from the edge, or above or over the strip when it has to.
- **Response-time readouts are steady.** A recorded check on the monitor page can be hovered from a few pixels away instead of only exactly on its dot (the nearest check wins), and the card no longer grows and shrinks as the readout appears and disappears, however long the readout is.
- **The escalation-policy row on a service's paging card lines up with the rest of the card** instead of running to its edge in larger type.

### Upgrade notes

- One schema migration (00110): an index on resolved incidents, applied automatically on startup (or by `cerbix migrate`). It is built without `CONCURRENTLY`, so writes to `incidents` wait while it builds — a fraction of a second at typical sizes; reads are unaffected and no role needs stopping. The index changes no data, so rolling back to v0.3.7's binary is safe. No configuration change.

---

## [v0.3.7] - 2026-10-07

### 🩹 Fixed

- **Service reliability no longer stalls on services with a long history.** The sealed-through watermark was recomputed over every bucket since the service's era began, on every scheduler slice; once that took longer than the slice's commit reserve (about 50 000 buckets — roughly five weeks of a service's history), every slice rolled back and materialization stopped for good, for every service, since the stuck one was always picked first. The recompute now starts at the current watermark and looks at most one day ahead. A stalled installation resumes on its own after upgrading (D-0269).

### Upgrade notes

- No schema migration, no configuration change.

---

## [v0.3.6] - 2026-10-06

### 🔒 Security

- **Frontend dependencies patched.** `vue` 3.5.43 fixes GHSA-g2v6-rqmx-r4w6 in `@vue/server-renderer` (XSS through an attribute name containing a carriage return) and `source-map-js` 1.2.2 fixes GHSA-68fv-2mgg-jv7q (event-loop denial of service); both were flagged as high by `npm audit` on v0.3.5's dependencies. The embedded SPA is rebuilt with them.
- **`google.golang.org/grpc` 1.83.2.** Clears GO-2026-6443 (in an imported package; govulncheck finds no call path from cerbix).

### 🩹 Fixed

- **The Security workflow scans again.** `govulncheck` is pinned to v1.7.0, the newest release that runs on the repository's Go 1.25 toolchain; `@latest` (v1.8.0) needs Go 1.26 and had stopped installing, so nothing was being scanned. Three `golang.org/x/crypto` advisories (GO-2026-6355, GO-2026-6354, GO-2026-5932) remain until the Go 1.26 upgrade; govulncheck reports none of them as called.

### Upgrade notes

- No schema migration, no configuration change.

---

## [v0.3.5] - 2026-10-06

### ✨ Added

- **Structured Cobra CLI.** The command tree now provides grouped top-level help and detailed command-specific help. Documented commands, canonical flags, defaults, environment variables and gate/change result exit codes remain unchanged. Leaf commands reject previously ignored positional arguments and unsupported shorthand clusters with usage exit code 2 before side effects; cluster-looking string-flag values remain literal. A version JSON writer failure now reports `version:` and exits 1 instead of the native parser's silent exit 0; successful JSON still exits 0. Viper is not used.

- **Cross-brand identity and shell UX.** cerbix now uses the Sealed C default mark with custom-logo priority and contrast-safe instance accents. The SPA adds responsive navigation access, accessible overlay
  and focus behavior, atomic organization/project transitions, a stale-response-safe SearchBox, truthful Dashboard loading/no-data/error states, and explicit breadcrumb, theme and announcement semantics. Desktop
  layout, status colors, reliability formulas and backend/API contracts remain unchanged.

### Changed

- **Service burn rules need a short window of at least 300 s.** A service burn window ends at the sealed watermark, which trails now by the 120 s late-arrival grace plus up to a bucket, so a shorter short window could never — or only intermittently — contain sealed time. The API now refuses it with 400; monitor burn rules keep their 60 s floor. Rules stored earlier are not migrated and keep their behaviour until they are next saved (D-0267).

### 🔒 Security

- **Remote CLI URL errors redact credentials.** `gate check` and `change record` no longer echo a rejected `CERBIX_URL` value or parser detail that could contain embedded userinfo or query credentials.

### 🩹 Fixed

- **Cobra parser boundaries fail closed.** Commands must occupy their canonical raw positions; undocumented shorthand clusters (including `-hh=false` and `-hhh=false`) now fail with usage exit 2 before an executor or config/network access, while registered string-flag values remain literal. Version positionals, including `""`, take priority over unsupported clusters and malformed help before JSON encoding. Help assignments accept only exact lowercase `true`/`false`, not pflag aliases such as `1` or `t`; string-flag values remain literal. Ordinary positional and compound-help path errors precede any help policy; only then does any canonical false-help assignment win for ordinary commands regardless of repeated flag order; `version` keeps false-help as a compatibility no-op while any parseable true-help wins; compound help is idempotent; hidden completion protocols are unreachable; and the Windows mousetrap is disabled.

- **Architecture and operator documentation now follows the current runtime.** HTTP-pull diagrams use the real `/api/v1/agent/*` routes; heartbeat storage is described as adaptive and the PostgreSQL 15+ floor is separated from the repository's PostgreSQL 16 images; README build and deploy/change boundaries match the supported workflow; and the top-level configuration table includes the current service, audit, and expected-run ledger sections.

- **Wave 2 documentation closes the remaining domain and operator gaps.** Architecture/overview/README now cover the current-domain partial ERDs, operator recovery CLI, build/toolchain contexts, and shipped shell/brand behavior; docs-check adds guards against stale routes, queues, storage and heartbeat-ERD claims.

- **Instance branding and detail reads are fenced.** Custom accents choose readable `--accent-ink` values and clear all inline accent properties when removed. Incident Detail rejects stale-generation and
  foreign-project responses before publishing incident data or actions.

- **Service reliability materialization no longer globally sorts retained heartbeat history for
  carry-in.** The historical sample-and-hold lookup now performs one bounded, index-backed latest
  prior observation lookup per declared monitor, preserving the exact carry-in semantics while
  preventing a large retained history from consuming the Service materialization slice. This is a
  data-compatible change and requires no database migration.

- **SLA windows longer than raw heartbeat retention cover the whole window.** With the default 30-day `heartbeats.retention_days`, the 90-day SLA (and any window longer than a lowered retention) silently covered only the retained days. Availability now adds the daily rollup for days whose raw heartbeats were purged — on monitor and project SLA, the weekly SLA report, the 90-day uptime of a monitor-backed status-page component, and monitor burn windows. A window with no data older than its start still shows its number and says where the data starts: the UI marks it "since DD.MM.YYYY UTC", and the API adds `data_from`, `latency_from` (latency stays raw) and the public status-page field `uptime_since` (D-0268).

- **SLA "budget left" reads the budget, not all time.** An untouched 99.9 % budget showed 0 % and At risk because the view rendered `remaining_ratio` (a fraction of all time); it now shows 100 % and Meeting (D-0267).

- **Late heartbeats correct every sealed bucket they reach.** A heartbeat arriving behind the service watermark now repairs every sealed bucket up to the monitor's next observation, not only its own minute, and the repair runner makes progress on long ranges instead of retrying one oversized batch; a statement timeout at the tail of a slice no longer counts as a failure (D-0267).

- **A re-enabled push monitor is not instantly BAD in service reliability.** A ping from before the re-enable is no longer evidence, matching the monitor's own dead-man timer (D-0267).

### 🧪 Tests

- **Gate fixtures now match schema 109.** Test rows and assertions were aligned with the persisted
  policy source/owner fields, all-window fields, and the schema-109 payload limits. PostgreSQL 16
  declarative-partition and TimescaleDB normal/race `internal/store` suites pass.

### Upgrade notes

- This release contains no schema migration and does not require an offline database upgrade.
  Existing installations retain their current data and watermarks.
- Saving a service burn rule whose short window is below 300 s now returns 400; widen it to 300 s or
  more when you next edit that target.
- On plain PostgreSQL (no TimescaleDB), long SLA windows of large projects read noticeably slower
  than before (measured locally: a 500-monitor project's four windows about 2.1–2.5 s instead of
  about 1.1 s); TimescaleDB is unaffected in the same measurement (D-0268).

---

## [v0.3.0] - 2026-09-22

This minor release combines the status-page service-component repair from `iter-0181` with the
alert-routing tenant-boundary hardening from `iter-0182`, bounded audit retention from `iter-0184`,
project-level inherited release-gate policy from `iter-0185`, and worst-of-all-windows gate evaluation
from `iter-0186`, the evidence-driven onboarding journey from `iter-0187`, and the service-first public
status-page incident experience from `iter-0189`, hardened in `iter-0190`, plus the incident-aware
overall-status composition from `iter-0191`, hardened in `iter-0192`.

### ✨ Added

- **Bounded audit-log retention (iter-0184, FR-033 / NFR-027).** Strict instance-wide retention
  removes only database-clock-expired organization and global audit rows in fenced, bounded batches.
  The scheduler, metrics, alerts, recovery guidance, and migration index ship together; audit APIs
  and retained-history semantics do not change.

- **Project-level inherited release-gate policy (iter-0185, FR-034 / NFR-028).** Projects can
  define one versioned gate policy. A service resolves its effective document in explicit service →
  project → not-configured order, records the immutable source tuple in decisions and overrides, and
  exposes the result through API, CLI-compatible schema, metrics, and the Settings / service SPA.
  Editing a project policy revokes only overrides that inherited that project revision.

- **Worst-of-all-windows release-gate evaluation (iter-0186, FR-035 / NFR-029).** A schema-v2
  project or service policy can evaluate every configured standard SLO target in one snapshot. The
  decision keeps healthy and unhealthy windows together, applies BLOCK → unavailable → WARN
  precedence without averaging or early exit, persists complete per-window evidence, and renders
  mode switching, inventory, inherited policy, UNKNOWN and override states in the SPA.

- **Evidence-driven onboarding (iter-0187, FR-036 / NFR-030).** The Dashboard now offers a
  non-blocking organization → project → monitor → first persisted heartbeat guide. Progress is
  recomputed from tenant-scoped server facts, monitor creation stays in the existing typed form,
  push credentials remain on monitor detail, and UP or DOWN is accepted as the first useful result
  without mislabeling a failed target as healthy. Existing installations keep their normal KPI,
  availability and monitor-card Dashboard; manual re-entry opens only a compact setup summary.
  Automatic-dismissal preference is browser-local and scoped by user and selected tenant context.

- **Service-first public status pages (iter-0189, FR-037 / NFR-031).** Current service/component
  state now precedes incident detail. Active incidents render as compact, keyboard-operable
  full-row accordions with deterministic impact grouping only above eight rows, one lifecycle badge,
  one impact badge, truthful opened/updated metadata, a two-line latest-update preview, and the real
  timeline on expansion. Page-local affected-component relations support direct incident focus
  without exposing internal monitor, service, project, actor, update, or postmortem identifiers.

### 🩹 Fixed

- **Active status-page incidents can no longer coexist with a false green all-clear (iter-0191,
  hardened in iter-0192; FR-038 / NFR-032).** Public and authenticated-preview heroes compose
  measured component health with the existing public active-incident list in one pure frontend
  helper. Explicit `summary_state` owns the component truth and all-clear eligibility; the legacy
  summary fallback runs only when state is absent. Measured impairment or maintenance keeps its
  headline, while otherwise active incidents lead with count and order-independent worst impact.
  Major and Minor render warning attention, Critical renders danger attention, and impact `none`
  remains neutral rather than green. Complete supporting copy retains no-data, empty-page and
  unmeasured-component disclosure. Component rows, incident lifecycle, API/OpenAPI, persistence,
  cache, metrics, alerts, SLA/SLO and timestamp semantics remain unchanged.

- **Status-page post-close hardening is isolated in iter-0190.** The scheduled-maintenance title is
  restored to the logical heading hierarchy without changing its visual style. Backend incident
  projection now reuses one ordered page-local monitor/service index, and the SPA reuses one computed
  component-to-incident map for counts and navigation instead of repeatedly scanning both lists.
  The closed iter-0189 report remains an immutable delivery snapshot.

- **The scheduler readiness race gate is deterministic again.** Its live-leader test no longer
  cancels the scheduler before assertions or mistakes the startup fail-closed `ready=0` state for the
  intended evaluator-lag verdict. Cancellation and goroutine joining now happen in test cleanup, and
  the wait requires the specific `lagging` reason. Production scheduler behaviour is unchanged.

- **Status-page components can be bound to services again.** The API contract and SPA already sent
  `service_id`, and the store already persisted it, but the HTTP request decoder did not accept the
  field. Because unknown JSON fields are rejected, every valid service-backed component request
  failed as `400 invalid JSON body`. The handler now accepts `service_id`, validates both binding ID
  formats at the transport boundary, and returns the created component with its derived `service`
  source.

- **Binding failures now keep their correct HTTP meaning.** A missing or unauthorized monitor or
  service returns the same non-oracular `400 binding not found`; a status page deleted during the
  create transaction remains a real `404`; and database or infrastructure failures are logged and
  returned as `500` rather than being mislabeled as caller errors.

- **The live status-page regression cleans up after partial failures.** Its service fixture is now
  registered for cleanup immediately, so a page-create or response-decoding failure does not leave
  test data behind.

### 🔒 Security

- **Status-page binding ownership is now enforced transactionally by the store.** Monitor and
  service bindings must belong to the page's organization and, for project-scoped pages, to the
  page's project. A retained monitor/service pair must resolve to one compatible source project.
  HTTP and non-HTTP callers therefore use the same validation owner, without a separate
  monitor-only preflight that could drift or expose cross-tenant existence.

- **Alert-routing references are now same-project persistence invariants.** Monitor/channel links,
  escalation-policy targets, on-call schedule participants and updates, override channels, and
  frozen service-escalation snapshots are validated at store and schema boundaries. Updates match
  both object ID and project ID, and runtime escalation resolution carries the incident project
  through policy, schedule, and channel reads so a pre-existing malformed reference cannot deliver
  across projects.

- **Missing and foreign routing IDs share one generic client refusal.** The API maps the store-owned
  refusal to `400` without revealing whether an identifier exists in another tenant. No target ID is
  added to metrics or logs as a label.

### ⚙️ Upgrade notes

- Migration `00106_alert_routing_tenancy.sql` backfills project identity for relational routing
  edges, adds composite tenant foreign keys, and installs JSONB tenant guards for policies,
  schedules, and frozen snapshots. It deletes no heartbeat, incident, audit, or historical routing
  data and performs no silent repair.

- **The migration intentionally fails fast if deployed data already contains a cross-project
  routing reference.** Diagnose the offending row read-only, repair it explicitly, and rerun the
  migration; the runbook contains the operator procedure. Valid existing routing data requires no
  manual action.

- No configuration key or public API schema is removed. The status-page change makes the server
  honor the already-published `service_id` contract; valid same-project alert-routing behavior is
  unchanged.

<sub>`iter-0181` / D-0249 and `iter-0182` / D-0250 · full Go tests, targeted race, vet, build,
documentation checks and configured lint green · live status-page Playwright regression included in
71 passed / 1 skipped / 0 failed · PostgreSQL 16 migration and direct-SQL tenant regressions green,
including the full DB-backed `internal/store` package. `iter-0184` completed its delivery gates;
`iter-0185` and `iter-0186` pass their PostgreSQL migration/snapshot regressions, full Go and SPA
suites, generated-schema parity, Dockerized lint, build, docs checks, and the scoped live gate UI
Playwright suite. `iter-0191` / `iter-0192` pass 56 focused helper/component tests, 764 frontend tests,
full Go/race/build/vet/lint, generated API and embedded-SPA parity, and rebuilt-stack public/preview
Playwright at desktop and 430 px with exact copy, accessible heading and no horizontal overflow.</sub>

---

## [v0.2.0] - 2026-09-08

One defect, found by an independent reviewer about an hour after `v0.1.9` was published, and
repaired under its own iteration rather than left for the next one.

**Why a minor bump for one fix, and why this section was numbered twice.** It was released as
`v0.1.9.1` first, and that tag and release were withdrawn by the owner: a four-component version is
not semantic versioning, so tooling that orders by semver — GitHub's own "latest" among it — would
never have ranked it. The content is unchanged; only the number is. `v0.2.0` rather than `v0.1.10`
is the owner's choice, and the fix it carries is a behaviour restoration rather than an addition.

### 🩹 Fixed

- **An `async_canary` that declared a credential binding could not run at all.** Every dispatch was
  refused at the executor gate and the monitor reported nothing else.

  The scheduler nominated monitors for credential materialization by asking whether the monitor's
  TYPE carries a static credential schema. An `async_canary` has none — its bindings are declared
  inside the workflow document — so a canary with a declared binding took the plain branch and was
  stamped with a generation that carries no envelope, while the expected-field set named the
  binding's field anyway. The executor then refused the job, correctly, for a field the producer had
  never been asked to supply. **If you declared a binding on a canary in `v0.1.9`, it never ran**;
  after this release it runs, with no change to the monitor.

  The predicate the nomination asks is now a monitor-level one rather than a type-level one. The
  schema half of it is unchanged on purpose: the tempting definition — "expects at least one envelope
  field" — reads false for `promql`, which has a schema and no required field, and would have
  repaired one type by breaking another.

- **And Test Connection on such a canary now says what to do instead of failing far away.** The same
  substitution was made on the test path: it builds a generation-1 job with no envelope for any type
  whose credential requirement is decided by its schema, and a canary's is not. The pre-save refusal
  that a SYNTHETIC scenario binding has had since FR-028 — "save the monitor before testing it" —
  had no canary twin, so pressing Test on a canary with a binding produced a message about a missing
  envelope field from the far side of the dispatch. It now names the binding and the way forward,
  before any probe is attempted.

- **And the envelope could not be BUILT for such a canary either**, one level below the nomination.
  The digest that binds a credential to the exact execution asks for the canonical value of every
  binding key; for a canary those keys are the workflow document, the run key and each reference,
  and the value lookup had arms for a synthetic scenario and none for a canary — so it fell through
  to the credential-schema resolver, which refuses a type that has no schema. Sealing failed and the
  monitor was refused with a reason naming decryption, which points an operator at a key problem
  that does not exist. **This was found by writing the store/materializer/gate regression an independent
  reviewer refused to release without**; the nomination repair alone would have shipped the feature still
  broken.

- **No migration, no configuration change, no API change.** A canary that declares no binding, and
  every other monitor type, dispatches exactly as it did. The new refusal is a 400 on a path that
  previously reached a prober and failed there.

### Why no test caught it

Every test passed on the defective tree, including the full `-race` suite, the browser suite and the
geo topology suite. None of them dispatches a canary that declares a binding: the canary suites use
bindingless workflows and the credential suites use schema types. It is stated here rather than
quietly fixed because the gap is the useful part — a green gate is only evidence about what it
actually covers.

<sub>iter-0180, `D-0248` · full `-race` 33 packages exit 0 (`internal/store` 763.4 s) · `make dev-test`
70 passed / 1 skipped on an image rebuilt from this candidate tree · `make docs-check` OK · THREE defects,
one substitution: a type-level predicate answering a monitor-level question, in the nomination, in the pre-save test refusal and in the canonical value lookup the digest depends on · the third was found only by `TestACanaryBindingIsSealedIntoAGenerationThreeEnvelope`, which runs against the real materializer and the real executor gate and proves generation 3 + `EnvelopeV2` + the credential actually used — a store/materializer/gate seam, not an end-to-end one: it neither publishes nor probes · four regressions, each with its killing mutation — the nomination loop, and
the pre-save refusal whose canary arm was missing · the sibling was found by reading every product
call site of the type-level predicate against what it decides: eighteen sites, fifteen correct, two
already unions, one carrying the same defect · the nomination regression with its killing
mutation — restoring the old predicate fails the declared-binding case by name and leaves both
negative cases passing · what is NOT covered is recorded in `iter-0180` §4c and not softened: no single process holds the
scheduler, the store and the prober at once, and the live cases that exist today do not demonstrate a successful canary run</sub>

---

## [v0.1.9] - 2026-09-07

Everything since `v0.1.8`, in four parts and one set of fixes: the **typed external canary** and
the audit trail that were prepared under this number once before, the **truthful-rendering**
package, the **expected-run ledger**, and the **fifty-two repairs** an independent two-axis review
of this whole range then found in it.

**This heading was `[v0.1.9] - 2026-09-03` before, and it was withdrawn.** The tag had been created
and then deleted by hand before the review that gates it closed (`8ee023c docs(v0.1.9): the tag is
dropped, not moved, until the review closes`), while the dated heading outlived it and announced a
release that did not exist. It is back by the owner's decision, with the later work folded in and
the date moved to when that decision was taken.

**Why that history is kept here:** a version heading can make a claim falsely, and this one did.
The withdrawn heading announced a release whose tag had been deleted by hand, and the file went on
saying so for days. The record of that stays; the sentence that reported the repository's tag state
does not, because THIS SECTION IS THE RELEASE BODY — `.github/workflows/build.yml` publishes it
verbatim for the matching tag — and a release announcing that its own tag does not exist would be
the same defect wearing the opposite sign.

### Part 1 — the canary work, prepared under this number once before

Four things in one release, in the order the owner set: a **typed external canary** for async API
journeys, an **audit trail for writes that change something** — incidents and now monitors — the
**dependency sweep** that had been waiting behind both, and one small improvement that came last, an
**optional description on a monitor**. The canary is the largest single feature since FR-021 and it
ships **complete**: all six phases, including the capability announcement and the typed UI form that an
earlier draft of these notes listed as missing. No breaking change. Five migrations, all additive.

### ⚠️ Upgrade Notes

- **The order of a canary rollout no longer matters.** A canary reaches only an executor that
  ANNOUNCED it can run one, so declaring a canary before a region is upgraded is safe: the run is
  refused with one bounded DOWN per attempt — `no_capable_runner` when nothing there announced a canary
  runner, `capability_mismatch` when what is there speaks another version — and it starts working by
  itself when the executors arrive. An earlier draft of these notes told you to upgrade the region
  first; that instruction is retired, not merely relaxed.
- **An AMQP `worker` older than this release never receives a canary at all.** Canaries ride their own
  queue (`checks.canary.<kind>@<version>.<region>`), which only a current binary binds, and on the
  HTTP-pull transport a job carries the capability it requires while a claim carries what the agent
  declared. That is the point: in a mixed fleet the old executor cannot take a job it would fail.
- **A repeated incident acknowledgement is now a genuine no-op.** It used to rewrite the incident's
  `updated_at` on every retry. The acknowledger and the instant stay the FIRST ones, and the
  modification time no longer moves. If anything of yours sorts incidents by `updated_at` and relied on
  a re-acknowledgement bumping them, it will not any more.
- **Two identical Alertmanager deliveries that arrive at the same moment now both answer HTTP 200** —
  one reporting `opened: 1` (or `resolved: 1`) and the other `ignored: 1`. The loser of that race used
  to answer 500, while the sequential retry a millisecond later answered 200. If you alert on 5xx from
  the receiver, that source of noise is gone.
- **The `golang` build image moves to 1.27.0** and `docker/setup-buildx-action` to 4.3.0. Neither
  changes the language version in `go.mod`.

### ✨ Added

**A typed external canary for async API journeys (FR-029 / NFR-024)**

A new monitor type, `async_canary`, runs ONE asynchronous transaction end to end and reports it as an
ordinary monitor: submit, take a correlation id, await a terminal outcome, assert the declared fields,
validate the cleanup boundary — and return ONE heartbeat carrying up/down, total latency, the failed
stage and a bounded code class. It is declared in a Monitoring-as-Code bundle as a nested, typed
`workflow:` block with closed unions and no free-form field anywhere: no `settings` map, no JSON string,
and a key the schema does not name refuses the whole bundle by name.

- **The contract is a type, not a convention.** Two submit kinds (`http_json`, `multipart_fixture`), two
  completion kinds (`sse`, `poll_json`), two correlation sources, three assertion kinds, two restricted
  grammars, and every bound with a number. `{{ correlation_id }}` is legal in exactly one field — the
  completion URL — because nothing has produced an id at submit time.
- **Credentials are bindings, never values.** A binding is declared once under `workflow.secrets` and
  referenced by name at each position; the value is resolved at dispatch, delivered in the existing
  credential envelope, held in memory for one execution and wiped. A credential-bearing header accepts a
  binding and nothing else. **What is not protected, stated plainly:** a credential pasted into an
  ordinary header or under an innocuous body key is not detectable and is not refused — the same
  residual FR-028 named.
- **The URL policy is strict and has no off switch.** HTTPS only; loopback, link-local, private ranges
  and cloud metadata are refused AFTER DNS resolution and re-validated on every redirect hop. There is
  no setting that relaxes it, in v1 or as a hidden flag, because a flag reachable in production is the
  policy's own bypass — tests reach a local fixture through an injected dialer at the seam, never
  through a product option. The executor also drops EVERY binding-backed header on a cross-host
  redirect, which `net/http` does not do: it strips `Authorization` and has never heard of `x-api-key`.
- **One run per monitor, four per region**, decided by the scheduler at dispatch. A refused run writes
  one ordinary DOWN with a bounded reason (`region_saturated`, `already_in_flight`, `no_capable_runner`,
  `capability_mismatch`) and the monitor's own
  `failure_threshold` decides the flip, so the sample counts as unavailable in the SLI like any other
  DOWN. Every submit carries an `Idempotency-Key` derived from the monitor, its execution revision and
  the scheduled window — whether a second submit with that key creates a second task is the target's
  contract, not cerbix's.
- **Nothing it touches leaks.** No heartbeat, log line, error or metric label carries a URL, a body, a
  header value, a secret, the correlation id or the result object path.
- **New metrics** `cerbix_canary_stage_total` and `cerbix_canary_dispatch_refused_total`, both
  low-cardinality and carrying no monitor id, URL or correlation id.

**An executor receives a canary only if it announced it can run one** (the last of the six phases).
The announcement is a set of `<kind>@<version>` tokens, and an executor makes it from the runner it
actually has: a pull agent in its heartbeat and again on every claim, an AMQP worker by consuming
`checks.canary.[v3.]<token>.<region>`, and `--role=all` by construction. The scheduler dispatches only
into a region that announced the token the DOCUMENT needs, and a region that announced nothing gets
`no_capable_runner` while a version skew gets `capability_mismatch` — two reasons because the fix
differs: start a runner, or finish the upgrade. The filter is not treated as the whole barrier, for the
reason the credential envelopes already established: a capability CHECK does not stop a consumer from
consuming, so an incapable executor is made unable to RECEIVE the job — a queue it does not bind on
AMQP, and a per-claim capability filter on the pull transport.

**A canary is declared in the SPA, in a bundle, or through the API.** The typed form is typed only:
no JSON editor on the create view or the read view, the five stages laid out as stages, bindings as
stage 0, and every refusal met AT THE FIELD rather than as a 400 after Create. A saved canary reads
back into the same form, with the binding halves recombined from the document's marker and the flat
reference key's name.

**A monitor says what it is for.** A monitor carries an optional `description` — plain text, at most
200 characters counted as Unicode code points, so a Cyrillic sentence is as long as a Latin one to the
person writing it. It appears under the name on the monitor list (one line, the whole text as its
tooltip), as the beginning of a line on a dashboard panel, and in full on the monitor's own page; it is
editable on the create and edit forms with a live count, and declarable in a Monitoring-as-Code bundle.
Every monitor that exists reads back with an empty description and every surface renders exactly as it
did — the absence of an element is asserted per surface, not assumed. It is deliberately absent from
public status pages, notification payloads and search.

### 🔒 Security

**An incident write says who wrote it (FR-026 / NFR-021)**

FR-022 promised, as its invariant 14, that "every write is audited with actor and tenant, in the
mutating transaction". That was false of the PRODUCT rather than merely unimplemented: an incident
write left **no** `audit_logs` row, for a monitor incident or a service one. A member could resolve
someone else's incident, publish a postmortem or acknowledge a page, and the organization's audit trail
said nothing.

- **Every incident mutation made by a PRINCIPAL now writes exactly one audit row in the same
  transaction** — the manual create, the Alertmanager receiver's create and its resolve, a status
  change, a note, the first acknowledgement, and a postmortem create or update. A committed change
  without its row cannot exist, and a rolled-back one leaves none.
- **The vocabulary is five words** — `incident.create`, `incident.status`, `incident.note`,
  `incident.acknowledge`, `incident.postmortem` — and the target carries ids and both ends of a
  transition and **never a body**: no note text, postmortem text or alert annotation reaches the trail.
- **Machine writes are excluded by decision and enumerated**: the reconciler's auto-open and
  auto-resolve, the service auto-incident and its resolve, the `⚡ Context:` note, both `⏸ Suppressed:`
  writers, `🚀 Changes:` and `🕸 Impact:`. Their record is the incident's own timeline, which names
  `system` as the author. Auditing them would bury a tenant's log under a flapping service's heartbeat.
- **No read is added.** The rows appear in the organization's existing audit listing and never in the
  instance one. `docs/runbook.md` now answers "who resolved this incident" directly.
- Two AST guards hold the door surface, each driven by a fixture that contains the violation. The first
  version of one guard exempted the Alertmanager receiver by name — and the exemption hid the exact
  defect the requirement exists to prevent, because Alertmanager posts with a project-write token and a
  token is a principal.

**And a monitor write says who wrote it, by the same rules**

Creating, editing or deleting a monitor through the API or the SPA left no audit row either. It now
writes exactly one, in the mutating transaction, with three words — `monitor.create`,
`monitor.update`, `monitor.delete`.

- **The target names the document and never its contents**: the monitor's id, slug, type and region;
  `enabled true→false` when the write was a pause or a resume, because that is the edit an operator
  asks about; and, for an async canary whose workflow declares `cleanup.kind: none` with
  `acknowledged: true`, the clause `cleanup=none acknowledged` — which is what makes that
  acknowledgement visible in the trail, as its requirement promised. No config value, credential
  reference, target URL, scenario or workflow body appears in a target, for any type.
- **What the row says is read from the row the writer HOLDS**, not from what the caller passed: the
  `FOR UPDATE` statement for a delete, the returned row for an update. Independent review found the
  first version taking a whole monitor from the caller — a concurrent edit could make the audited region
  stale, and a careless caller could file the row under another project's organization. The delete door
  takes an id now, so there is nowhere to put a foreign project.
- **A monitor applied from a Monitoring-as-Code bundle writes no row**: it is a machine write, and its
  record is the bundle. Deleting a project leaves the organization's trail intact.

### 🔧 Changed

- **A pull monitor with a timeout past 30 seconds is no longer re-claimable mid-probe.** The agent's
  claim lease was hardcoded at 30 s, so any pull monitor slower than that could be handed to a second
  agent while the first was still working. The lease is now per job. This is older than the canary; the
  canary is what made it visible.
- `async_canary` joins the `monitors_type_check` database constraint. Every Go test passed while the
  real table refused the type, because no store test writes to it — a live E2E found it, and the type
  vocabulary is now asserted against the database itself so the next new type cannot repeat it.
- **A red CI job now names the tests that failed**, as annotations the check-runs API serves without
  repository-admin rights. Two failures that had been unreadable from outside turned out to be tests
  measuring the MACHINE rather than the product: one read a legitimate deferral of a partition drop —
  correct behaviour while any older transaction is open — as a crash that never happened, and one gave a
  repair slice two seconds and failed when the closing write missed it, where production keeps the range
  under a sixty-second lease and resumes. Both tests are corrected; no product code changed.

### 📦 Dependencies

Seven of the eight open dependabot branches, verified one group and one major at a time (iter-0170).

- go-modules: `rabbitmq/amqp091-go` 1.13.0 → 1.14.0, `google.golang.org/grpc` 1.83.0 → 1.83.1
- github-actions: `docker/setup-buildx-action` 4.2.0 → 4.3.0 (SHA-pinned)
- docker: the `golang` build image 1.26.6-bookworm → **1.27.0-bookworm**
- frontend: `@types/node` → 26.4.1, `eslint` → 10.9.1, `vue-tsc` → 3.3.11, and three MAJORS —
  `jsdom` 26 → **30**, `vite` 6.4 → **8.2**, `vue-router` 4.6 → **5.3**. The last two are one decision,
  not two: vue-router 5 declares `peerOptional vite@"^7.3.0 || ^8.0.0"` and will not install on Vite 6.
- **Not taken: TypeScript 7.** `vue-tsc` 3.3.11 cannot drive the native compiler port —
  `npm run type-check` dies with `ERR_PACKAGE_PATH_NOT_EXPORTED` before checking a file. Upstream of
  this repository; the bump waits for a `vue-tsc` that supports it.

The committed SPA snapshot (`internal/web/dist`) is regenerated with this: every asset filename hash
moved, because Vite 8 hashes differently.

### API · Metrics · Schema

- **API:** `Monitor.description` on read, create and update (`maxLength: 200`, counted as code points;
  omitted on update leaves it unchanged, `""` clears it, and a longer value is 400 naming the field).
  The monitor `config` description documents the `async_canary` `workflow` document and its
  `canary_secret_<binding>_ref` keys; the server canonicalizes the document on write, so an
  API-created canary and a bundle-declared one store byte-identical documents. The agent protocol
  gains one capability declaration in both directions: `workflow_kinds` in the heartbeat and
  `X-Cerbix-Workflow-Kinds` on every job claim, bounded and grammar-checked before it reaches a
  column.
- **Metrics:** `cerbix_canary_stage_total` and `cerbix_canary_dispatch_refused_total`, both
  low-cardinality and carrying no monitor id, URL or correlation id. The refusal metric's reasons are
  the four bounded ones a heartbeat can carry.
- **Schema:** five additive migrations — `00095` the canary in-flight lease, `00096` the per-job pull
  claim lease, `00097` `async_canary` in `monitors_type_check`, `00098` `pull_jobs.workflow_kind` (NULL
  for every job any agent may run), `00099` `monitors.description` (`NOT NULL DEFAULT ''`, which is the
  whole compatibility promise in one default). No index added: nothing reads by a description, and
  search over it was declined.

<sub>Decisions D-0218 … D-0234 · FR-029, NFR-024, FR-026 (+ its §10 monitor amendment), NFR-021 and
FR-030 DONE · iterations iter-0168 … iter-0173, all CLOSED, every range approved by the independent
reviewer · full `-race` suite green (33 packages), vitest 46 files / 514 tests, browser suite 66 passed
/ 1 skipped, geo topology suite 12 passed · hosted CI green on the pushed head, all seven jobs,
including `Backend (timescaledb hypertables)` — the job whose two failures D-0225 could not read until
the annotations step made a red run legible</sub>

### Part 2 — a rendering claims no more than the facts behind it (FR-031 / NFR-025)

Three surfaces drew two different states identically, so a reader could not tell them apart — and
one of them quoted availability over a range it had never stored.

- **The reliability timeline is on CLOCK TIME.** Cells come from the requested range at the rollup
  grain, so a step with nothing stored occupies its own real width in its own encoding instead of
  vanishing and letting its neighbours stretch over it. Five encodings that cannot be confused:
  `unknown` is a solid neutral slice (a decided verdict, never a status hue), `not-stored` is a
  hatch with an outline, and opacity means `provisional` and nothing else. Height carries QUANTITY.
  **A problem is never hidden:** a slice too small to draw is MARKED non-geometrically and named in
  the readout rather than being silently rounded away.
- **A segment states its STORAGE verdict and withholds availability while that storage is
  incomplete.** This closed a real defect in the old numbers: a segment could quote
  `availability 100%` over a range it never materialized. Coverage is still printed, as its own
  separately named fraction.
- **The Response time panel draws every heartbeat at its real timestamp** — including the
  zero-latency failures that used to be filtered out and disappear — as points only, with no
  connecting stroke and no fill, because neither can span time nothing was measured in. Absence is
  drawn POSITIVELY by an observation ruler: one tick per recorded check, and the empty spans
  between them are focusable and say only what is known.
- **The `/sla` objective card says which state it is in.** Read-only with an explicit Edit, a
  guarded save, the draft cleared on success, and both writers gated on their load generation — so
  a save that started under one project can no longer land under another.
- **NFR-025 — no timestamp is rendered without naming its zone, and it is enforced rather than
  fixed.** One mechanism, a NAMED RENDERER PER SUBJECT and deliberately no generic date formatter,
  with the UTC offset resolved AT the instant (a 30-day window in late March or October crosses a
  DST change, so this is the ordinary case). Identity stays UTC; presentation is local and always
  names its offset. **Every hand-rolled date site in the SPA is gone**: outside two modules — one
  for renderings, one for keys, wire values and HTML control values — no product file calls
  `toISOString` or pulls a field off a `Date` at all. The suite runs green at `TZ=UTC` and at
  `TZ=Asia/Yekaterinburg`, which is what makes that a fact rather than a claim.

<sub>Decisions D-0235 … D-0236, D-0246 · FR-031, NFR-025a/b/c DONE · iterations iter-0174 and the
NFR-025c half in iter-0178 · no Go code and no API change in the FR-031 half: all four surfaces are
the SPA and the storage verdict reads a field the payload already carried</sub>

### Part 3 — cerbix records that a run was expected (FR-032)

Until now a check that never ran left nothing behind. A gap in the heartbeats meant "we do not
know", and the product could not tell "the monitor was fine and nothing was due" from "a run was
due and never happened" — so no rendering could honestly connect two points across it.

- **An expected-run ledger.** The scheduler now persists the window it already computed:
  `expected_runs`, range-partitioned by `due_at`, one row per `(monitor, due window)`, with a
  `monitor_schedule` segment history behind it so a monitor whose interval changed is read against
  the interval that was in force. Every window ends in a verdict — covered, covered late, missed,
  withheld, reserved — and the ledger **withholds** rather than guesses whenever it cannot defend
  an answer.
- **RESERVE → PUBLISH → CONFIRM.** The window is recorded BEFORE the job leaves the process, and
  `issued_at` is written only by the CONFIRM that follows a successful publish. A running instance
  produced a false `expected_never_issued` under the older advance-then-publish ordering; this is
  the fix, and only the windows whose reservation was proved durable are published.
- **A fourth carrier generation that announces itself.** `checks.jobs.v4.<region>` and its
  HTTP-pull twin carry job identity; a region gets ledger-eligible jobs only where an executor has
  ANNOUNCED it can consume them, so a half-upgraded fleet degrades to "not eligible" instead of
  losing runs. Rolling back the migration REFUSES while generation-4 rows are pending rather than
  discarding them.
- **The panel may draw a connecting stroke again — but only across a span the ledger says was
  covered.** That is the whole point of the requirement: the stroke returns as a claim the product
  can defend.
- **Reading it:** `GET …/expected-runs` with a versioned cursor, a `ledger_from` fence so nothing
  before the ledger existed is presented as a missed run, retention that includes the DEFAULT
  partition, and an unlabelled HOT-ratio gauge sampled by the leader.

<sub>Decisions D-0237 … D-0245 · FR-032 DONE · iterations iter-0174 … iter-0177 · schema `00100`
`monitor_execution_revisions`, `00101` the generation-4 pull boundary, `00102` the ledger itself,
`00103` the reservation columns — all additive</sub>

### Fixed — two things only a live distributed stack could show

- **A credentialed Test Connection failed in the distributed topology with `no worker queue for
  region "core"`.** The API enforced credential envelopes and the core worker's config did not, so
  the API published on a carrier that worker had never bound. The two halves of one topology now
  agree, and a regression fails if they ever disagree again — in either direction, and also if the
  executor is handed the at-rest master key it must never hold.
- **Every generation-3 Test Connection was dead-lettered by the consumer bound to its own queue.**
  One function serves both envelope test carriers and its admission check compared against a
  hardcoded generation, so the newer carrier accepted nothing. Because the caller is answered only
  on success, this surfaced as a ten-second timeout reported as `no worker responded in region …` —
  the same message an empty region gives. The queue is the generation now, as it already was on the
  jobs path, and a table-driven regression publishes on every carrier an executor serves.

- **An envelope OLDER than its carrier defines was accepted on every path.** The mapping is
  explicit — carrier generation 2 carries envelope v1, generation 3 carries v2 — and the producer
  had implemented it exactly since generation 3 shipped, while every consumer enforced only a
  floor. So a generation-1 envelope on a generation-3 carrier opened normally and its credential
  reached the prober WITHOUT the execution-body binding that generation exists to add: the property
  that stops a credential being replayed against a different target. One owner holds the mapping
  now, the gate every executor crosses refuses a mismatch, and both AMQP consumers dead-letter it.
- **A refused dispatch is answered instead of being met with silence.** It used to publish nothing,
  so the caller waited out its RPC timeout and reported `no worker responded in region …` — the
  same sentence an EMPTY region gives, which made a refused delivery indistinguishable from a dead
  region. It now returns a typed reason immediately, from the same bounded vocabulary executors
  already answer with, and the poison body is still dead-lettered for inspection. **If you alert on
  Test Connection latency, refusals stop costing a full timeout.**
- **Every operator-typed timestamp control now names the zone it is read in.** Maintenance windows,
  gate overrides, backfill instants and the instance-silence deadline are entered in controls whose
  HTML value carries no offset, and they said only "Starts", "Ends", "Until", "from". The date
  ranges on the change and gate-decision views are read as UTC calendar days and said only
  "From"/"To". Each now states its interpretation, with the offset resolved AT the instant typed, so
  a window entered across a DST change is not labelled with the wrong one.

<sub>iter-0178, `D-0246` · `make dev-test-distributed` 11 passed / 1 skipped · geo topology 14
passed, including credentialed dispatch on both remote transports — a path no geo run exercised
before · `make secret-smoke` and `make mac-smoke` green · full `-race` 33 packages exit 0 ·
`iter-0178` CLOSED by the owner</sub>

### Part 4 — the repairs a two-axis review of this range found (audit gap package 3)

Fifty-two findings from an independent review of everything since `v0.1.8`, in seven clusters. The
package adds no requirement: it repairs FR-020, FR-026, FR-029, FR-030, FR-031/NFR-025 and FR-032,
which is why it carries an iteration number and no `FR-`. Three of the findings were P0.

#### ⚠️ Upgrade Notes

- **Migration `00104` can REFUSE the upgrade, deliberately.** It narrows the `pull_tests` carrier
  ceiling to generations 1–3 and stops with a count if the table holds a row at
  `protocol_version = 4`. No build emits a generation-4 TEST carrier, so such a row was not written
  by this product, and the migration will not discard it for you: silently deleting an unexplained
  row destroys the evidence of whatever wrote it. Nothing is lost when this happens — the database
  stays at `00103` — and `runbook.md` carries the query to inspect the rows and the reason the
  asymmetry with `pull_jobs` is correct.
- **Migration `00105` drops `expected_runs_job_idx`**, which served no query in the tree: every
  statement that mentions `job_id` also constrains `due_at`, and the primary key is
  `(monitor_id, due_at)`.
- **`ON DELETE SET NULL` column-list migrations: SIX, not five.** `00093` added the sixth when the
  reliability gate landed, and only the README and the refusal message were corrected then. Eight
  other statements — the overview, the runbook's count and its list, two rows of `status.md`, two
  passages of `decisions.md`, and a test comment — still said five. The PostgreSQL 15 floor itself
  is unchanged; what was wrong was every document's count of why.

#### 🩹 Fixed

- **A credentialed-by-SCHEMA monitor with no credential value was stamped with the wrong carrier**
  (P0). One producer branch skipped the stamp, so the ledger read those windows as `unknown` and
  coverage was lost, silently, for a whole class of monitors — probes ran and results correlated.
- **An external canary kept its binding headers across a scheme-downgrading redirect** (P0). The hop
  is now refused before any header operation, and the origin comparison includes the scheme, so a
  redirect from `https` to `http` on the same host and port is cross-origin rather than "the same
  place".
- **An undispatchable canary reported nothing at all** (P0). A canary in a region with no capable
  runner now flips the monitor DOWN through the ordinary result pipeline instead of sitting on a
  queue until its TTL — the indefinite pending the invariant forbids.
- **A canary workflow with no completion block dereferenced a nil pointer**, which one crafted
  message could use to take down a region's whole prober pool. It is refused structurally now.
- **The expected-run ledger's read API listed rows it declared unanswerable.** The retained floor
  ignored the DEFAULT partition, so rows stored there were returned by the list while
  `ledger_from` said the ledger could not speak for that span — two answers, one of them provably
  wrong. The retention cutoff is also midnight-aligned everywhere, matching the purge that drops
  partitions on that boundary.
- **A batch of expectation advances failed as a whole when one item was unrepresentable.** The
  healthy items reserve and publish; the rejected one is named in the log with its reason.
- **The reliability timeline drew cells wider than the windows they belonged to**, so a covered cell
  painted over the missed windows beside it and the pointer targets overlapped identically — the
  readout named a window the pointer was not over. Cell width now comes from the window grid, and
  each pointer target is bounded by half the space to its neighbour, so two targets can touch and
  never overlap.
- **An on-call rotation anchor moved by the viewer's offset on a cosmetic edit.** The control is
  pre-filled in the viewer's zone and saved back as an instant; CI now runs the frontend suite a
  second time in a non-UTC zone, without which the test proved nothing.
- **Six more rendering repairs**: a storage verdict printed from a series that had not been read; a
  panel subtitle describing strokes it had not drawn; a legend naming all observations where the
  figure was computed from measured ones; a timeout declared in scale from an unrelated average; a
  UTC day range that hid its clock; and a retention bound the API description had hardcoded.

#### 🔧 Changed

- **The carrier decision has one owner, and the pull-region exclusion one reader.** The scheduler
  held three independent resolvers for "what can this region's executors consume"; they now share
  one record, and a source guard fails by name if any of them goes back to reading the map directly.
- **Four guards were widened to what they claim.** The `Test…` citation guard reads Go comments in
  both spellings; the declared-door guard follows embedded interfaces at any depth and reports an
  element it cannot read rather than treating it as absence; the PG15 migration guard derives its
  sites from the tree instead of a hardcoded pair; and the rendering guard matches the locale
  formatter with or without `new`.

<sub>iter-0179, `D-0247` · 52 of 52 discharge rows built, each `behaviour` row with a recorded
killing mutation · independent review COMPLETE, 52 of 52, no open findings · full `-race` 33
packages exit 0 (`internal/store` 755.8 s) · `make docs-check` OK · SPA 683 tests at UTC and
`Asia/Kolkata` · `make dev-test` 70 passed / 1 skipped on an image rebuilt from the CLOSING tree,
verified by CONTENT rather than by build timestamp — the live server serves the closing SPA build
and answers the pre-E4 asset with the index fallback · `make geo-test` 14 passed,
run by the independent reviewer on a stack raised for him · `iter-0179` CLOSED by the owner, on his
word and not on the review's result</sub>

---

## [v0.1.8] - 2026-09-03

A credential inside a synthetic monitor's scenario used to be stored in cleartext, returned to every
principal who could READ the monitor, skipped by key rotation, and echoed into the heartbeat message
with the whole request URL whenever a step's transport failed. This release closes all four, in three
stages that were built and reviewed separately, and gives the editor a way to declare a credential
without ever typing one. It BREAKS a synthetic monitor that keeps a token in an authorization-style
header — read the upgrade note. No migration.

This was a **spec-versus-code defect, not a new feature**: `func-oncall-synthetic-pull.md` FR-SYN-1
already promised "encryption like the other types" and its §217 promised inclusion in `reencrypt`, and
D-0090 promised the failure message "never echoes bodies/headers". None of the three was true of the
code, and the SYN requirements had no rows in `docs/status.md` at all, which is how a false promise
survived unnoticed. They have rows now.

### ⚠️ Upgrade Notes

- **A synthetic monitor whose credential-bearing header holds a LITERAL now fails validation on its
  next write.** The affected headers are the finite credential-bearing set — `authorization`,
  `proxy-authorization`, `cookie`, `x-api-key`, `api-key`, `x-auth-token`, `auth-token`,
  `x-access-token`, `access-token`, `private-token` — whose value must now be exactly one
  `{{secret:<binding>}}` placeholder. **Existing monitors keep probing**: nothing re-validates on
  read, so the refusal appears the next time such a monitor is EDITED, and it names the step and the
  header without echoing the value. Move the token into the project secret inventory and reference it
  as a binding ([b3c99b6](https://github.com/teamlead-com/cerbix/commit/b3c99b6))
- **No migration, and no readiness coupling.** The scenario stays in `monitors.config`; only its form
  changes, to ciphertext. An instance with no `security.encryption_key` starts, reports ready and keeps
  probing every monitor it was probing: what it refuses is a scenario WRITE it cannot protect. Legacy
  plaintext scenarios stay plaintext until a key is supplied — that cost is stated rather than traded
  for an outage ([cc90e4a](https://github.com/teamlead-com/cerbix/commit/cc90e4a))
- **Probe failure messages changed shape for every monitor type, not only synthetic.** A transport
  failure now reads as a bounded class plus the target's host (`dns: no such host (api.internal)`)
  instead of Go's error text. If you have alert rules or dashboards matching on `heartbeat.msg`
  substrings, check them. Nothing in the product parses that field to make a decision ([e6a3db8](https://github.com/teamlead-com/cerbix/commit/e6a3db8))

### 🔒 Security

**A credential in a scenario is a secret at rest, on read, and in the record (FR-028 / NFR-023)**

- **Stage 0 — no probe result carries a request URL, for any type.** `net/http` embeds the request URL
  in every error it returns, so `Msg: err.Error()` published whatever the target's query string
  carried — reproduced on an ordinary `http` monitor and on `promql`, not only on synthetic. Every
  failure is now composed from a bounded class plus a host, asserted per type through the real prober
  registry with a secret planted in the URL, the query, a header and the body ([e6a3db8](https://github.com/teamlead-com/cerbix/commit/e6a3db8))
- **Stage 1 — the scenario is ciphertext at rest, and withheld from anyone who cannot write the
  monitor.** One secret set became three classifications by MEANING (encrypted-at-rest,
  write-only-on-read, writer-only-display) and the store reader became an explicit MODE named by what
  it decrypts. A viewer receives no scenario at all — not plaintext, not ciphertext — through a store
  call chosen after the authorization decision rather than by redacting a decrypted document. Key
  rotation covers scenarios, and an idempotent, compare-and-set, NON-fatal startup backfill converts
  existing rows ([cc90e4a](https://github.com/teamlead-com/cerbix/commit/cc90e4a))
- **Stage 2 — a declared credential is a NAMED BINDING resolved from the project inventory.** The
  document carries `{{secret:<binding>}}` and the secret's NAME lives in a flat config key,
  `scenario_secret_<binding>_ref`, so rename, delete-counting and rotation run on the path
  `password_ref` already runs on. The value is delivered in the credential envelope, substituted into
  the scenario by the executor, and the substituted copy is dropped when the probe ends ([b3c99b6](https://github.com/teamlead-com/cerbix/commit/b3c99b6))
- **A moved placeholder cannot survive a valid envelope.** The scenario and its reference keys became
  EXECUTION BINDING KEYS, so `EnvelopeV2`'s body digest covers the stored document: an attacker with a
  valid envelope who rewrites a step's URL produces a different document and the AEAD fails before any
  request. A binding therefore REQUIRES a body-bound envelope, and a region on an older carrier gets a
  per-monitor `carrier_too_old` rather than a job that looks protected and is not ([b3c99b6](https://github.com/teamlead-com/cerbix/commit/b3c99b6))
- **The binding belongs to a synthetic monitor and to nothing else**, decided once at the write
  boundary and gated in the store, in both derived key sets, and at the dispatch gate — which refuses
  rather than ignores such a job on any other type, permanently, on carrier integrity ([084b49d](https://github.com/teamlead-com/cerbix/commit/084b49d))
  ([4fdedff](https://github.com/teamlead-com/cerbix/commit/4fdedff))

**What this does NOT claim, stated because a security note that overstates is worse than none.** A
literal secret is not detectable by shape, so the enforceable rule is the header-NAME one. A credential
pasted into a header nobody would call a credential header, or into a body, is still legal: it is
encrypted at rest, withheld from viewers and kept out of probe results, and it travels to the prober
inside the ordinary job rather than in an envelope. Buying the stronger property needs a restrictive
typed request model for the scenario, which is an owner's decision and separate work — and a heuristic
over VALUES is refused rather than deferred ([900aa1b](https://github.com/teamlead-com/cerbix/commit/900aa1b))

### New Features

**Declaring a binding in the UI**

- **The synthetic monitor form has a Scenario secrets panel** above the steps: pick a project secret,
  name the binding, and the row then shows which secret fills it, where it is used, and the flat key it
  is stored as. A binding name is never displayed without the secret it resolves to ([3a67106](https://github.com/teamlead-com/cerbix/commit/3a67106))
- **A credential-bearing header stops being a free-text field.** Its value control is a binding
  selector from the first keystroke of the header name — empty and disabled with "Add a binding first"
  until one exists — so the rule is met before a token is pasted rather than after a failed save
  ([95ee945](https://github.com/teamlead-com/cerbix/commit/95ee945))
- **"Save before test" is stated at the button.** A scenario carrying bindings is deliberately not
  testable before it is saved: that path builds an unsaved monitor with no envelope, so a placeholder
  would travel to the target as literal text. The Test control is disabled with the reason and the way
  forward; a credential-free scenario still tests unchanged ([3a67106](https://github.com/teamlead-com/cerbix/commit/3a67106))
- **What is NOT protected is shown too**: a pasted-looking value in an ordinary header gets a hint
  offering the inventory — never a refusal, because cerbix cannot tell a credential from data there
  ([3a67106](https://github.com/teamlead-com/cerbix/commit/3a67106))

### Bug Fixes

- **A synthetic monitor could not be created from the SPA at all.** `canSubmit` required a target for
  every active type while the form deliberately hides that field for `synthetic` — whose
  `NeedsTarget()` is false — so **Create monitor was permanently disabled and unexplained**. Found by a
  new component test, and it survived because no unit test and no browser test had ever submitted this
  type ([3a67106](https://github.com/teamlead-com/cerbix/commit/3a67106))
- **The form's default scenario opened INVALID.** The scaffold shipped
  `Authorization: Bearer {{token}}`, which the new rule refuses, so merely choosing Synthetic
  produced a form whose own example could not be saved. It now demonstrates `extract` → interpolate
  with an id used in the next step's path, which is what `extract` is for ([95ee945](https://github.com/teamlead-com/cerbix/commit/95ee945))
- **A malformed binding reference was silently ignored.** `scenario_secret_Login_ref` — capitalised, so
  outside the grammar — meant the operator declared a binding, saw no error, and shipped a scenario the
  credential was never wired into. It is refused by name now ([084b49d](https://github.com/teamlead-com/cerbix/commit/084b49d))

### Improvements

**Documentation that stopped lying**

- **FR-SYN-1, NFR-SYN-2 and AC-SYN-2 corrected in place**, and the five SYN requirements given the
  `docs/status.md` rows they never had — including two gaps stated rather than implied: no test names
  the whole-scenario deadline, and no browser test puts a synthetic monitor on a GEO worker
  ([4e6def8](https://github.com/teamlead-com/cerbix/commit/4e6def8))
- **The binding key is documented in `openapi.yaml`** on all three monitor config schemas. The feature
  was API-reachable and undocumented, which makes it unusable rather than merely un-designed
  ([084b49d](https://github.com/teamlead-com/cerbix/commit/084b49d))
- **The runbook gained an FR-028 section** covering what is protected at rest, on read and in the
  record; the detection query and one-edit repair for a stale reference; and — as plainly — what is not
  protected ([258adb5](https://github.com/teamlead-com/cerbix/commit/258adb5))

**Repository hygiene**

- **The SPA build runs as the developer, not as root.** Every docker build wrote
  `frontend/node_modules` and its caches as root, so any tool later run by a developer failed with
  EACCES on a tree it owned nothing in — it stopped an independent reviewer from running the frontend
  tests at all. `make spa-snapshot` now passes `--user` and an in-container npm cache ([c321931](https://github.com/teamlead-com/cerbix/commit/c321931))
- **The first browser coverage a synthetic monitor has ever had** (`e2e/tests/synthetic-bindings.spec.ts`):
  declare a binding through the UI, meet the refusal, see the test blocked, save, and find the flat key
  and the placeholder on the wire with no value anywhere ([3a67106](https://github.com/teamlead-com/cerbix/commit/3a67106))

### API · Metrics · Schema

- **API:** the monitor `config` description documents `scenario_secret_<binding>_ref` and its rules on
  create, update and test; a `scenario_secret_*` key on any non-synthetic type is refused with 400
  naming the key; test-before-save answers 400 naming the binding for a scenario that declares one.
- **Metrics:** unchanged. A binding that cannot be materialized surfaces as the existing per-monitor
  reason (`carrier_too_old`, `missing_reference`, `decrypt_failed`), never as a readiness flip.
- **Schema:** no migration and no new column. `monitor_secret_refs` carries a binding through the same
  `setting_key` shape as `password_ref`. The scenario's VALUE in `monitors.config` becomes
  self-describing ciphertext, converted in place by the startup backfill.

<sub>15 commits · decisions D-0216 (+ addendum) and D-0217 · FR-028 and NFR-023 DONE, FR-SYN-1/2/3 and
NFR-SYN-1/2 given their first rows · iterations iter-0166 and iter-0167, both CLOSED and both approved
by the independent reviewer after five and four rounds · full `-race` suite green (33 packages, with
`internal/store` re-run idle to separate a pre-existing load-dependent FR-024 flake), vitest 38 files /
418 tests, browser suite 60 passed / 1 skipped</sub>

---

## [v0.1.7] - 2026-09-02

PromQL grew up: it is expressible in a Monitoring-as-Code bundle and can authenticate to a Prometheus
behind basic auth. One security fix rides with it, and it BREAKS a configuration that works today —
read the upgrade note first. No migration.

### ⚠️ Upgrade Notes

- **A monitor target may no longer carry credentials in its URL userinfo.**
  `https://user:pass@prom.internal:9090` is refused on every surface now, not only in bundles. It used
  to work — Go's `net/http` turns such a URL into an `Authorization: Basic` header on its own — and the
  password was stored as plaintext in `monitors.target`, which the API's redaction does not blank, so
  every VIEWER of the project could read it in the monitor list. **Stored monitors keep probing**:
  nothing re-validates on read, so the refusal appears the next time such a monitor is EDITED. Move the
  credential to the type's own settings (`promql` now has them) or to a target without userinfo before
  editing ([f827d96](https://github.com/teamlead-com/cerbix/commit/f827d96))
- **No migration, no configuration change.** A `promql` monitor with no `auth_mode` behaves exactly as
  before, at validation and at the dispatch gate — the default is resolved, never written, so no
  canonical hash moves and no monitor is rescheduled ([754e5dc](https://github.com/teamlead-com/cerbix/commit/754e5dc))
- **Worth knowing if you deploy the release BINARIES:** the `v0.1.6` assets were compiled with Go
  1.25.12, which carries seven known standard-library vulnerabilities this release closes by moving the
  pin. The container image was never affected — it is built on `golang:1.26.6` — so an installation
  running the image was not exposed by this ([be75d51](https://github.com/teamlead-com/cerbix/commit/be75d51))

### New Features

**PromQL: bundles and basic auth**

- **A `promql` monitor can live in a Monitoring-as-Code bundle.** It carries `query` — required,
  bounded at 1024 characters, never blank. The type was excluded because a 2026-08 classification
  grouped it with the credentialed types; the prober had in fact never had a credential slot, so what
  stood between it and a bundle was one typed field ([39245fe](https://github.com/teamlead-com/cerbix/commit/39245fe))
- **Optional HTTP basic auth against Prometheus**, through `auth_mode: none | basic`. `basic` requires
  a `username` and a credential — a project-secret reference in a bundle, value or reference in the
  UI/API — and the prober sends the header only when a username is configured, so an unauthenticated
  Prometheus is never handed an empty one. Bearer tokens and mTLS are deliberately NOT supported: basic
  is what Prometheus implements natively, and for anything else the answer remains a regional agent
  plus an unauthenticated path for the prober ([754e5dc](https://github.com/teamlead-com/cerbix/commit/754e5dc))
- **In the SPA:** the PromQL section gained an Authentication selector, a username field and the same
  credential control the database types use — value or project secret, with the dangling-secret warning
  ([05db534](https://github.com/teamlead-com/cerbix/commit/05db534))

### Security

- **The Go pin carries the standard-library fixes `govulncheck` asks for.** Under the version the
  workflows install from `go.mod` (1.25.12) it reported SEVEN called vulnerabilities — `crypto/tls` ×2,
  `net/http` ×2, `encoding/xml`, `encoding/asn1`, `html/template` — reached through ordinary product
  paths: the HTTP prober's `Client.Do`, the mailer's TLS handshake, the operational server's
  `ListenAndServe`. All seven are fixed in 1.25.13, and one line moves the scan AND the release
  binaries, because all three workflows read the version from `go.mod` ([be75d51](https://github.com/teamlead-com/cerbix/commit/be75d51))
- **The full-history secret scan is clean, without weakening it.** Its seven findings were read one by
  one and are fabricated test fixtures — two sequential-hex AES keys, the same two base64-encoded, and
  an `actor_label` in a committed CLI transcript, which is the server's derived name for a bearer and
  never the token. Nothing needed rotating. The allowlist entries are anchored to those exact literals,
  so any other high-entropy string in the same files still fails the scan ([7a9350d](https://github.com/teamlead-com/cerbix/commit/7a9350d))

### Improvements

**Correctness**

- **The file provider no longer conflates "has settings" with "is credentialed".** One entry point
  routes a type to the credential registry, to its own schema, or to an error naming the key — which is
  what the specification has said in words since the beginning ([39245fe](https://github.com/teamlead-com/cerbix/commit/39245fe))
- **A blank-after-trim `query` is refused.** A presence check accepts `"   "`, and a whitespace-only
  expression makes the prober report "no query configured" on every run: a monitor that is configured,
  scheduled and permanently meaningless ([754e5dc](https://github.com/teamlead-com/cerbix/commit/754e5dc))
- **A rejection reason is decided by a typed error, not by matching message text**, so a reworded
  validator can no longer silently change which reason a bundle is refused with ([754e5dc](https://github.com/teamlead-com/cerbix/commit/754e5dc))

**Repository hygiene**

- **The documented test command is the one that finishes.** CI has passed `-timeout 40m` since
  iter-0163 because `internal/store` needs about eleven minutes under `-race`; `make race`, `CLAUDE.md`
  and `AGENTS.md` did not, so a developer running the documented command met
  `panic: test timed out` naming whichever test was running — a name that sends the reader after the
  wrong bug ([9608fbf](https://github.com/teamlead-com/cerbix/commit/9608fbf))
- **Dependabot got quieter without hiding anything**: monthly instead of weekly, all MAJOR bumps in one
  PR per ecosystem instead of one each, lower open-PR limits, and a cooldown so a version released
  yesterday is not proposed today ([18e3564](https://github.com/teamlead-com/cerbix/commit/18e3564))
- **A roadmap exists** (`docs/roadmap.md`) and the PRD points at it; the red Security workflow is its
  first item ([6e89c84](https://github.com/teamlead-com/cerbix/commit/6e89c84), [1efd5dc](https://github.com/teamlead-com/cerbix/commit/1efd5dc))
- **A flake left the suite by being understood rather than retried.** A scheduler test waited on the
  gate pass's sink events and then asserted on the leader gauge, which the leadership loop publishes on
  an unordered path — so under load it read the state before the step-down landed. The assertion now
  waits for the signal it asserts on, with the same requirement and its own deadline ([aa3cbfb](https://github.com/teamlead-com/cerbix/commit/aa3cbfb))

### API · Metrics · Schema

- **API:** the monitor `config` description now names promql's settings (`query`, and with
  `auth_mode: "basic"` a username plus `password` | `password_ref`); a monitor target with URL userinfo
  is refused with 400 on create and update.
- **Metrics:** unchanged.
- **Schema:** no migration. `promql` joins the credential registry, so `monitor_secret_refs` covers its
  `password_ref` through the same normalization every credentialed type uses.

<sub>14 commits · decisions D-0145 (addendum) and D-0215 · no requirement row: the type boundary is
FR-017's, and extending it is an addendum to its decision rather than a new requirement · full `-race`
suite green under the new pin, browser suite 59 passed / 1 skipped, `govulncheck` and `gitleaks` both
clean</sub>

---

## [v0.1.6] - 2026-09-01

Two requirements that turn reliability facts into something a pipeline can act on: a **release gate**
that answers whether the error budget allows a deploy, and **change intelligence** that records the
deploy went and lets the service's own facts say what followed. Eighty-five commits since `v0.1.5`.

### ⚠️ Upgrade Notes

- **Two additive migrations, no data repair, nothing to stop.** `00093` creates the gate's policy,
  override and decision tables plus the partition registry; `00094` creates the change tables and adds
  `api_tokens.actions`. Run `cerbix migrate` with the new binary and start as usual — every role applies
  migrations on startup anyway. On a database where no service has a gate policy, nothing evaluates and
  nothing is written ([16eecaf](https://github.com/teamlead-com/cerbix/commit/16eecaf), [c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd))
- **`00094` builds a UNIQUE index on `incidents`.** The constraint `(id, project_id)` is what lets an
  incident↔change link be tenant-safe by the schema rather than by a query. It is built
  non-concurrently and takes a brief exclusive lock on `incidents`; the guard skips the build entirely
  if an equivalent constraint already exists ([c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd))
- **The scheduler leader gains one background pass.** Gate-ledger partition maintenance runs on its own
  fenced advisory session — daily partitions created a week ahead, dropped past
  `gate.decision_retention_days` (default 90). Watch `cerbix_gate_decisions_writable_horizon_seconds`
  and `cerbix_gate_decisions_partitions_pending_drop`; a healthy pass keeps the first well above zero
  ([a6a9915](https://github.com/teamlead-com/cerbix/commit/a6a9915))
- **Existing API tokens are unchanged.** The new `actions` allow-list is optional: omitted or `null`
  means the token's role decides, exactly as before. A token is only ever NARROWED by it — the list is
  intersected with the role, never added to it ([7260e68](https://github.com/teamlead-com/cerbix/commit/7260e68))
- **`gate.*` and `change.*` ship with defaults**, so no YAML edit is required. The ones worth knowing:
  gate decisions retained 90 days and purged hourly; change groups retained by whole identity; the
  record endpoint bounded at 300 requests/minute per process and 30 per principal ([16eecaf](https://github.com/teamlead-com/cerbix/commit/16eecaf),
  [c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd))

### New Features

**Reliability Gate — a deploy asks whether the error budget allows it (FR-024)**

- **One call, one machine-readable answer.** `cerbix gate check --project <id> --service <id>` (or
  `POST /api/v1/projects/{p}/services/{s}/gate`) returns an observed `state` — `ALLOW`, `WARN`,
  `BLOCK`, `UNKNOWN`, `NOT_CONFIGURED` — an effective `action`, every matching reason and the evidence
  under it. The CLI's exit code follows the action (`0` allow/warn, `2` block, `4` not configured,
  `1` transport/auth), so a CI step is one command; credentials come from the environment only
  ([d926568](https://github.com/teamlead-com/cerbix/commit/d926568), [229b33e](https://github.com/teamlead-com/cerbix/commit/229b33e))
- **What blocks is declared, not guessed.** A per-service policy names ONE SLO window and assigns each
  clause of a closed vocabulary — budget exhausted, budget consumed over a threshold, page-burn firing,
  ticket-burn firing, an open service incident — to `block`, `warn` or `ignore`, with a mandatory
  `unknown_behavior` and a seal-lag bound past which the budget is UNAVAILABLE rather than quoted stale
  ([8cb12d6](https://github.com/teamlead-com/cerbix/commit/8cb12d6))
- **The gate derives nothing.** The service row, the policy, the active override, the report, the burn
  latches, the incident and the ledger write all happen in ONE `REPEATABLE READ` transaction whose
  first snapshot-bearing statement is the decision's `evaluated_at` — so a gate answer and the service
  page cannot disagree about the same instant ([8cb12d6](https://github.com/teamlead-com/cerbix/commit/8cb12d6), [b7518b9](https://github.com/teamlead-com/cerbix/commit/b7518b9))
- **An override changes the action and never the facts.** At most seven days, one active per service,
  bound to the policy revision, project-admin only; the decision still records the state that was
  observed and that somebody overrode it ([229b33e](https://github.com/teamlead-com/cerbix/commit/229b33e))
- **Every decision is an immutable ledger row**, in a daily-partitioned bounded table, readable by id
  after the service is renamed or deleted — the moment the evidence is actually wanted. Partition
  maintenance runs as a fenced pass with a 30-second lifecycle and ownership proved by marker rather
  than by OID ([a6a9915](https://github.com/teamlead-com/cerbix/commit/a6a9915), [24e901f](https://github.com/teamlead-com/cerbix/commit/24e901f))
- **In the SPA:** a `Release gate` card on the service page with the policy editor, the latest decision
  and the override panel; a `Gate decisions` browser with a server-side state filter and keyset paging;
  the by-id record; and the per-service override history. Opening a page never creates a decision — only
  a pipeline does ([9793758](https://github.com/teamlead-com/cerbix/commit/9793758), [84adac1](https://github.com/teamlead-com/cerbix/commit/84adac1), [3fc19e3](https://github.com/teamlead-com/cerbix/commit/3fc19e3))

**Change Intelligence — the pipeline says what it changed (FR-025)**

- **A change is a fact about time, not a catalog.** `cerbix change record` (or one `POST`) reports a
  `deploy`, `rollback` or `flag` in one of its phases — `started`, `succeeded`, `failed`, `cancelled` —
  under an external identity `(source, external_id)`, optionally naming the gate decision the release
  rested on. Phases are append-only and idempotent: an identical retry returns the original row, a
  contradictory one is refused by name, and two runners reporting different endings for one run cannot
  both pass ([c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd), [74ce187](https://github.com/teamlead-com/cerbix/commit/74ce187))
- **The service timeline** is a bounded `[from, to)` read of change groups with an opaque cursor that
  never returns a group twice, each group carrying its live gate decision and the incidents it preceded
  ([7260e68](https://github.com/teamlead-com/cerbix/commit/7260e68))
- **Incident correlation says "preceded", never "caused".** When a service auto-incident opens, the
  changes within the correlation window on that service and on its `probable_root` upstreams are linked
  and named in one `🚀 Changes:` note. It is fail-open in both directions: the incident opens and
  resolves exactly as before whatever the correlation does ([7260e68](https://github.com/teamlead-com/cerbix/commit/7260e68), [3b43b70](https://github.com/teamlead-com/cerbix/commit/3b43b70),
  [f5654d7](https://github.com/teamlead-com/cerbix/commit/f5654d7))
- **Before/after SLI around a change**, computed from SEALED buckets through the same query the
  reliability page uses — never a second implementation. Each side is a figure, or withheld with the
  page's own word, or `pending` until the seal reaches it; a delta only when both sides are figures
  ([c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd), [5cace76](https://github.com/teamlead-com/cerbix/commit/5cace76))
- **A CI token can be narrower than a role.** An optional `actions` allow-list on an API token is
  intersected with the role in `authz.Can`, so a pipeline token can be exactly "ask the gate, record a
  change" and nothing else ([7260e68](https://github.com/teamlead-com/cerbix/commit/7260e68), [a1cfe00](https://github.com/teamlead-com/cerbix/commit/a1cfe00))
- **In the SPA:** a `Changes` card beside the release gate with terminal-only marks on the facts strip,
  the timeline view, the comparison view, and `Preceded by` on the incident page ([d2e0f3c](https://github.com/teamlead-com/cerbix/commit/d2e0f3c),
  [42ae4ab](https://github.com/teamlead-com/cerbix/commit/42ae4ab))

**Notification channels are edited in place**

- **A channel's name and config are editable**, so rotating a bot token or a hook URL no longer means
  delete-and-recreate — which silently dropped every monitor link, escalation step and alert route
  pointing at the channel. A secret left blank keeps the stored value, because the API never sends one
  out; the merged config is validated, so an edit cannot leave a channel undeliverable ([3e76791](https://github.com/teamlead-com/cerbix/commit/3e76791))

### Improvements

**UI**

- **The alerting panel keeps an operator's unsaved edits** when a late prop arrives instead of
  discarding them ([dd83bfa](https://github.com/teamlead-com/cerbix/commit/dd83bfa))
- **The gate ledger's state filter is the server's**, so a page of results is a page of matches and the
  cursor continues the filtered set ([84adac1](https://github.com/teamlead-com/cerbix/commit/84adac1))

**Documentation & Gates**

- **Every surface now says what the product is.** D-0174's positioning — a service reliability platform,
  not "uptime & SLA monitoring" — reached the README at iter-0160 and stopped there; the CLI's help, the
  OpenAPI description, the overview, the onboarding doc, the systemd unit and the OIDC client all still
  carried the pre-FR-021 framing. Claims that the repository is private are gone with them; they told a
  reader to authenticate for things that need no authentication ([92edc5b](https://github.com/teamlead-com/cerbix/commit/92edc5b))
- **The PRD describes the product as it is now** — services, their incidents, their escalation ladder
  and change intelligence — and points at a roadmap that exists ([4066527](https://github.com/teamlead-com/cerbix/commit/4066527), [59d05c4](https://github.com/teamlead-com/cerbix/commit/59d05c4))
- **`make docs-check` compares the FR-025 acceptance map as a SET** and refuses the spellings the design
  retired, in the specification and in every living document ([056eff5](https://github.com/teamlead-com/cerbix/commit/056eff5), [b320290](https://github.com/teamlead-com/cerbix/commit/b320290))
- **The incident-audit gap has a requirement.** FR-026 / NFR-021 are specified and approved at revision
  four after three review rounds; no product code changes until the iteration opens ([9208171](https://github.com/teamlead-com/cerbix/commit/9208171))

### API · Metrics · Schema

- **API:** eleven gate routes (the decision, the policy CRUD with `expected_revision`, the override
  lifecycle and history, the project-scoped ledger read and listing); four change routes (record,
  timeline, compare, an incident's preceding changes); `ApiToken.actions`; `PATCH
  /api/v1/notification-channels/{id}` now accepts `name` and `config` as well as `enabled`; the
  service detail carries its `sla_targets` inventory.
- **Metrics:** the gate family — `cerbix_gate_decisions_total{state,action,overridden}`,
  `cerbix_gate_decision_duration_seconds` (this project's first histogram),
  `cerbix_gate_evaluate_rejected_total`, `cerbix_gate_evaluate_errors_total`,
  `cerbix_gate_maintenance_errors_total` and four ledger gauges; and the change family —
  `cerbix_change_correlations_total`, `cerbix_change_correlation_errors_total`,
  `cerbix_change_compare_total`, `cerbix_change_record_rejected_total` ([db16dfa](https://github.com/teamlead-com/cerbix/commit/db16dfa)).
- **Schema:** migrations `00093`–`00094` — gate policies, overrides, the daily-partitioned decision
  ledger and its ownership registry; `service_changes`, `incident_changes`, `api_tokens.actions`, and
  `UNIQUE (id, project_id)` on `incidents`.

<sub>85 commits · decisions D-0188…D-0214 · independent review: every FR-024 range approved, FR-025
approved as four effective slices plus the live-evidence correction · full account in
`docs/iterations/iter-0163.md`, `docs/iterations/iter-0164.md`, `docs/iterations/iter-0165.md`</sub>

---

## [v0.1.5] - 2026-08-28

### ⚠️ Upgrade Notes

- **Stop the outbox owners before migrating.** Roles `all`, `api` and `scheduler` run the outbox worker; `worker` and `agent` do not and can keep running. Run `cerbix migrate` once with the new binary, then start the owners. Migration `00088` hands ownership of a class of outbox rows to the database and cannot reach a delivery an old worker already has in flight. Skipping the stop loses nothing; it risks out-of-order delivery for at most one already-claimed batch (≤50 rows) per old owner ([fc6608d](https://github.com/teamlead-com/cerbix/commit/fc6608d), [88ec4ad](https://github.com/teamlead-com/cerbix/commit/88ec4ad))
- **Migration `00090` repairs incident data written by earlier versions.** Incidents resolved and then walked backwards, and service incidents stranded open after their alert ended, are resolved with a `🔧 Repaired:` timeline note. Two classes are reported in the migration output but deliberately left alone — member snapshots that may name a not-yet-governing revision, and auto-incidents with no monitor or service left — because fixing them would mean guessing at history. Queries and the manual procedure are in `docs/runbook.md`. On a database that never hit the races the migration is a no-op ([af7a7a2](https://github.com/teamlead-com/cerbix/commit/af7a7a2))
- **PostgreSQL 15 or newer is required.** 14 is not supported and will not be; `cerbix migrate` refuses before applying anything ([e53244b](https://github.com/teamlead-com/cerbix/commit/e53244b))
- **Armed services keep their coverage across the upgrade.** `00089` seeds delivery tracking as "delivered" for existing rows, so no member monitors start paging in the minute after the upgrade ([7a9e87d](https://github.com/teamlead-com/cerbix/commit/7a9e87d))

### New Features

**Service Alerting: Coverage Means Somebody Was Told**

- **A route must name a live channel.** A schedule pointing at a deleted or disabled channel no longer counts as a route; members keep paging instead of being silenced in favour of a replacement that can reach nobody ([fb9e94b](https://github.com/teamlead-com/cerbix/commit/fb9e94b), [08a8b31](https://github.com/teamlead-com/cerbix/commit/08a8b31))
- **An alert nobody can receive is withheld, not sent into the void.** Counted as `cerbix_service_alert_withheld_total{signal,reason}` with `unroutable` or `no_governing_revision`, and announced as soon as a route exists — on both the health and burn signals ([44dfdad](https://github.com/teamlead-com/cerbix/commit/44dfdad), [a3aecfc](https://github.com/teamlead-com/cerbix/commit/a3aecfc), [19d4943](https://github.com/teamlead-com/cerbix/commit/19d4943))
- **Coverage requires a delivered announcement.** A service suppresses its members only once its own alert *reached* at least one recipient — a channel row that exists is not a delivery, and a 500 from the only channel is not a delivery either ([7a9e87d](https://github.com/teamlead-com/cerbix/commit/7a9e87d), [5ec101b](https://github.com/teamlead-com/cerbix/commit/5ec101b), [35de54d](https://github.com/teamlead-com/cerbix/commit/35de54d))
- **Coverage follows the state the service is in.** A delivered DEGRADED announcement no longer covers a service that has since observed DOWN and is still confirming it ([6e6339f](https://github.com/teamlead-com/cerbix/commit/6e6339f), [46c50de](https://github.com/teamlead-com/cerbix/commit/46c50de))
- **An outage nobody heard about is announced again** once there is somebody to tell — channel deleted mid-flight, every send failed, or retries exhausted — with a fresh recipient list and a new episode. A partial delivery that reached some recipients is never re-sent to them ([23aa3cc](https://github.com/teamlead-com/cerbix/commit/23aa3cc), [425a59f](https://github.com/teamlead-com/cerbix/commit/425a59f), [2b2594c](https://github.com/teamlead-com/cerbix/commit/2b2594c))
- **One vocabulary for "why did my monitor page".** The service badge and `cerbix_alert_delegation_fail_open_total{reason}` are computed from the same clause evaluation. New values `no_owning_service`, `onset_pending`, `onset_undelivered`, `latch_inconsistent`; `stale_lease` on the burn arm now means an expired lease and nothing else ([18e3aef](https://github.com/teamlead-com/cerbix/commit/18e3aef), [3192ebb](https://github.com/teamlead-com/cerbix/commit/3192ebb), [6e0b6a4](https://github.com/teamlead-com/cerbix/commit/6e0b6a4), [4c4bae3](https://github.com/teamlead-com/cerbix/commit/4c4bae3))

**Service Escalation: Repeat Cadence**

- **Services can repeat the last escalation step.** `renotify_seconds` on the service — 0 is off and the default, otherwise 60..86400 — read live, so turning it down mid-incident takes effect immediately. Available in the UI, the API and monitoring-as-code bundles (`alerting.renotify_seconds`). Previously `repeat_last` on a service-attached policy did nothing ([90bf146](https://github.com/teamlead-com/cerbix/commit/90bf146), [425a59f](https://github.com/teamlead-com/cerbix/commit/425a59f))
- **An incident climbs the ladder it started with.** The escalation policy is snapshotted when the incident opens, so editing a policy mid-outage cannot re-time a page already in flight. Incidents open across the upgrade start their ladder at the upgrade instant instead of firing every overdue step at once ([0c6e5dd](https://github.com/teamlead-com/cerbix/commit/0c6e5dd), [d3f428f](https://github.com/teamlead-com/cerbix/commit/d3f428f), [118c55c](https://github.com/teamlead-com/cerbix/commit/118c55c))

**Outbox: Ordered and Bounded Delivery**

- **Incident webhooks are dispatched in order.** Every `incident.*` payload carries `seq`; the claim will not release an event while an earlier one for the same incident is undelivered, and a dead predecessor blocks too. Arrival order over the network is not promised — `(incident.id, seq)` is what lets a receiver dedupe and order, and the runbook describes two receiver strategies ([554e609](https://github.com/teamlead-com/cerbix/commit/554e609), [8ed191a](https://github.com/teamlead-com/cerbix/commit/8ed191a), [02bc005](https://github.com/teamlead-com/cerbix/commit/02bc005), [4762f1a](https://github.com/teamlead-com/cerbix/commit/4762f1a), [64349a0](https://github.com/teamlead-com/cerbix/commit/64349a0))
- **A delivery is bounded by the lease that authorised it.** A deposed worker can no longer keep an HTTP request or SMTP session open while the new owner sends the same event. The lease is measured in database time, the settle is never bounded, and a claim whose turn came after its lease is handed back with its attempt refunded ([b17e0e2](https://github.com/teamlead-com/cerbix/commit/b17e0e2), [425a59f](https://github.com/teamlead-com/cerbix/commit/425a59f), [2b2594c](https://github.com/teamlead-com/cerbix/commit/2b2594c))

### Improvements

**Incident Lifecycle**

- **`resolved` is terminal and status only moves forward**, enforced in the write rather than in a stale read. A plain comment keeps the current status instead of carrying whatever the client last saw ([b21b09f](https://github.com/teamlead-com/cerbix/commit/b21b09f), [183b9f8](https://github.com/teamlead-com/cerbix/commit/183b9f8), [451406b](https://github.com/teamlead-com/cerbix/commit/451406b))
- **Service incidents are a full lifecycle.** They announce open *and* resolve to webhooks and status-page subscribers, and resolve when their alert ends — including through disown and delete, which used to leave them open forever ([e405d85](https://github.com/teamlead-com/cerbix/commit/e405d85), [744980f](https://github.com/teamlead-com/cerbix/commit/744980f))
- **The postmortem names the declaration that governed the outage**, not the newest one; a foreign revision is refused ([0a8ba9b](https://github.com/teamlead-com/cerbix/commit/0a8ba9b), [335d4db](https://github.com/teamlead-com/cerbix/commit/335d4db))
- **Every incident write stamps its times after the row lock**, so a writer that waited cannot date its action before the wait ([3ad2a4b](https://github.com/teamlead-com/cerbix/commit/3ad2a4b), [5ec101b](https://github.com/teamlead-com/cerbix/commit/5ec101b))

**Status Pages**

- **A page, its feed and its subscriber mail agree on what the page reports.** A page made only of Service components used to show an incident and email nobody about it. One project axis now, with the subscriber query as its exact inverse ([c7ce059](https://github.com/teamlead-com/cerbix/commit/c7ce059))

**UI**

- **Search hits bring their workspace.** Opening a monitor or incident from another project switches to it, so edit controls are not hidden from a legitimate editor; detail views follow the URL when navigating between two of the same kind ([2dbc7ce](https://github.com/teamlead-com/cerbix/commit/2dbc7ce))
- **A partly unmeasured hour no longer renders green** on the Reliability card ([27c63f7](https://github.com/teamlead-com/cerbix/commit/27c63f7))
- **The service picker in status-page components works again**, and the escalation form says what "repeat last step" does on a service ([a26260d](https://github.com/teamlead-com/cerbix/commit/a26260d), [a331af0](https://github.com/teamlead-com/cerbix/commit/a331af0))

**Documentation & Gates**

- `make docs-check` compares the FR-021 invariant set in the spec against the discharge map exactly — missing, extra, duplicate or skipped numbers all fail; invariants 92–105 moved into the spec ([46c50de](https://github.com/teamlead-com/cerbix/commit/46c50de), [35de54d](https://github.com/teamlead-com/cerbix/commit/35de54d))
- Four documents that still described the product as it was before FR-022/FR-023 shipped are corrected, and a gate catches the class ([afc8cd1](https://github.com/teamlead-com/cerbix/commit/afc8cd1))

### API · Metrics · Schema

- **API:** `ServiceAlertPolicy.renotify_seconds`; alerting-state `reason` gains `onset_pending`, `onset_undelivered`, `latch_inconsistent`; `incident.*` webhook payloads carry `seq`.
- **Metrics:** new `cerbix_service_alert_withheld_total{signal,reason}`; `cerbix_alert_delegation_fail_open_total{reason}` also emits `error`, `record_failed`, `unspecified` for lookup-level failures.
- **Schema:** migrations `00085`–`00092` — incident escalation snapshots, `incidents.event_seq`, `CHECK ((status = 'resolved') = (resolved_at IS NOT NULL))`, `delivered_seq`/`undelivered_seq` on both service latch tables, `services.renotify_seconds`, `undelivered` as an episode close reason.

<sub>57 commits · decisions D-0175…D-0187 · independent review: product approved at `35de54d`, follow-on work at `eba2b69` · full account in `docs/iterations/iter-0161.md`</sub>

---

## [v0.1.5-beta.2] - 2026-08-19

Three defects found by running `v0.1.5-beta.1` in production rather than by a test. The first blocked the
upgrade outright.

### Fixed

- **PostgreSQL 15 is now enforced instead of assumed.** A production upgrade to `v0.1.5-beta.1` on
  PostgreSQL 14 applied `00061`…`00069` and died on `00070` with `syntax error at or near "("`: five
  migrations use the column-list `ON DELETE SET NULL (col)` form introduced in PG15. Every document, image
  and CI job already said 16, but nothing checked it and nothing said it out loud. `cerbix migrate` now
  reads `server_version_num` before the first file and refuses with the version, the requirement and the
  fact that nothing was applied. README, `runbook.md` and `overview.md` state the requirement; the runbook
  also carries the recovery note for a system left partially migrated (`00065` makes `monitors.slug` NOT
  NULL, which an older binary does not write).
- **A status-page component could not be created from a Service.** The service picker rendered a blank
  option AND carried no value, because the view read `sv.name`/`sv.id` from a list endpoint that answers
  `ServiceSummary` (the service wrapped with its rollup counts). An `as Service[]` cast on the load is what
  stopped the compiler from reporting it, and the test fixture repeated the same wrong shape, so six
  passing tests never saw it.
- **A stale claim on the service page.** The footer said availability, the error budget and the burn rate
  "arrive with the next iteration". They arrived in iter-0144 and the page already renders them; what was
  actually missing on a fresh service is sealed facts and a declared objective, which is what it says now.

---

## [v0.1.5-beta.1] - 2026-08-19

242 commits since `v0.1.0-beta.5`. This is the release where a **Service** becomes the object
reliability is defined on, measured for, and paged about — and where the product's own
positioning was corrected to match (**D-0174**).

### Added

- **Service reliability, phases 3–5 (FR-021 / NFR-016, D-0169)** — closed against an ENFORCED
  discharge map of 91 acceptance invariants and 24 required scenarios, each naming a test that
  exists; `make docs-check` fails if a number lacks a row or a row cites a test the tree lacks.
  - **dependency impact graph** — a same-project service DAG (schema-enforced tenancy, bounded,
    outside the declaration), symmetric open-time correlation into structured incident↔service
    links with 🕸 timeline notes. It annotates and links: it records **candidates**, never elects
    a culprit, and never suppresses or hides.
  - **status-page projection (§15.0)** — a component renders from ONE of three sources under a
    discriminator (`monitor`/`service`/`manual`) with the replaced binding kept dormant for
    revert. **Public-output change:** the page summary is worst-of-MEASURED plus an unmeasured
    count, and measurement ABSENT is the public status `no_data` — never `operational`.
  - **alerting ownership (§16)** — a Service can be the thing that pages, and its declared SLI
    members can stop delivering their own alerts for the same failure. Suppression is per
    SIGNAL (live health, sealed burn), per POLARITY (onset-like only; a recovery is never
    suppressed) and only while a replacement is demonstrably ARMED. Anything ambiguous **fails
    open** — the member pages.
  - **service burn alerting** — arbitrary long/short windows over one burn-math owner, with the
    hold matrix and the watermark every number was computed from.
- **Service incidents (FR-022 / NFR-017, D-0170, D-0171)** — an incident can be an incident OF a
  Service. At most one anchor, enforced by CHECK; opened in the SAME transaction as the
  announcement and resolved by its close; **never on a burn breach**; at most one open
  auto-incident per service; a member snapshot a postmortem can still name after the world moved;
  impact links through the service graph, with no link ever naming its own subject. Closed
  against 16 invariants + 16 scenarios.
- **Escalation for services (FR-023 / NFR-018, D-0172, D-0173)** — a Service with an escalation
  policy escalates its own auto-opened incident: steps from the incident's start, durable
  progress, acknowledgement or resolution ends it, every step names the SERVICE. The ladder
  **fails closed** where delegation fails open, and the service graph does **not** pause it.
  Closed against 16 invariants + 19 scenarios.
- **Project-level SLO objective** and an **instance-wide audit surface** for a global admin's own
  actions, which had been recorded for months and shown nowhere.
- **`job_id` correlation** end to end, and `observed_at` ordering that refuses a result older
  than the issue it answers.
- **A write path for a service's escalation policy** — the column existed since phase 5 and was
  reachable only at create time or from a file provider; a change to who gets woken is now
  audited with what moved, inside the mutating transaction.
- **`cerbix_service_incidents_total{action}`** and **`cerbix_escalation_steps_total{subject}`** —
  the on-call ladder had no metric at all before, only a log line.

### Changed

- **Positioning (D-0174)** — cerbix is a **service reliability platform**, not "uptime & SLA
  monitoring". The README states its NON-GOALS publicly, quoted from the specification: no
  arbitrary time-series queries, no generic telemetry, no query language, no metrics backend, no
  service catalog, no trace or log ingestion, and **no automatic root-cause analysis**.
- **`ServiceDetail.reliability`** stays `null` by design and now SAYS so: SLO, error budget and
  burn rate live on `GET …/services/{id}/reliability` with the honesty context a bare number
  would lack. The previous description claimed they were unbuilt.
- **Dependencies** — `golang.org/x/crypto` 0.55.0, `x/net` 0.58.0, `x/text` 0.41.0; build image
  golang 1.26.6; pinia 4.0.3; and four frontend majors (`@vitejs/plugin-vue` 6, `npm-run-all2` 9,
  `unplugin-auto-import` 21, `@vueuse/core` 14), each merged and verified one at a time.

### Fixed

- **A public leak found by reviewing our own invariant:** `Incident.PublicRedacted` cleared
  `project_id`, `monitor_id`, the external key and the ack actor — and not the `service_id` added
  days earlier, so every unauthenticated render of a page with a service incident shipped the
  service's internal UUID.
- **CI had never run on this line of work**: tests triggered only on `pull_request`, `docs-check`
  ran only by hand, one storage mode was covered, and readiness was awaited with `pg_isready`,
  which this project's own discipline calls a non-barrier. All four repaired.
- **Two flaky tests fixed at their cause**, not re-run: a fence test that bet a budget refusal on
  5ms of wall clock (2 failures in 12 runs under load), and scheduler telemetry waits that
  asserted more series than they waited for.

### Migrations

24 new migrations (`00061`…`00084`), forward-only and applied automatically by every role.

### Compatibility

FR-021 §17 makes backward compatibility an acceptance criterion, not a footnote: zero Services is
a valid installation state, every existing Monitor stays valid without a service, bundle format 1
stays valid, existing composites and monitor SLOs keep their semantics. **The one intentional
break is the public status-page output** described above — a consumer that read `operational` for
an unmeasured component will now read `no_data`.

---

## [v0.1.0-beta.1 … v0.1.0-beta.5] — released 2026-07-25 … 2026-08-12

> **Corrected on 2026-08-19.** This block sat under `[Unreleased]` while its contents were
> shipping across the five `v0.1.0-beta.*` tags — nobody moved it out. It is relabelled rather
> than rewritten, because it is a record of what happened, not a plan. The per-beta split is
> not reconstructed here: the tag messages carry it, and inventing a division after the fact
> would be a worse claim than admitting the block covers the whole beta train.

### Added
- **Geo-Distributed HTTP Pull Agent** (`--role agent`) with Long-Polling (`LISTEN/NOTIFY`), Edge Ring-Buffer (`bufferCap=10000`), and historical backfill (`POST /agent/backfill`).
- **Observability for Pull Transport**: Prometheus gauges `cerbix_pull_jobs_pending{region}` and `cerbix_pull_agent_lag_seconds{region}` with automatic lagging alerts.
- **Database Agent Tokens**: Table `agent_tokens` and Admin API (`POST/GET/DELETE /api/v1/agent-tokens`) for issuing, listing, and revoking agent tokens without redeploy.
- **Region Scoping**: Enforcement of `monitor.region == agent.region` on `/agent/results` and `/agent/backfill` (403 Forbidden on mismatch).
- **16 Prober Types**: Added probers for PostgreSQL, MySQL, Redis, RabbitMQ, PromQL, gRPC, WebSocket, SSH, DNS, TLS cert expiry, Composite, and Synthetic multi-step HTTP scenarios.
- **On-Call & Escalation Engine**: Escalation ladders, on-call schedules with vacation overrides, and acknowledge-to-stop incident handling.
- **Prometheus Alertmanager Receiver**: Inbound webhook receiver (`firing` -> auto-incident, `resolved` -> auto-close by fingerprint).
- **Instance Settings Framework**: Database singleton `instance_settings` for branding, auth policies, SMTP mailer, and global silence toggle.

### Changed
- **OIDC Provider Independence**: Identity provider is now any OpenID Connect issuer (Keycloak, Auth0, Okta, Google, Entra ID) discovered via `oidc.issuer`.
- **Database Schema**: Renamed `keycloak_sub` to `oidc_sub`.
- **Heartbeat Retention & Partitioning**: Switched `heartbeats` to native daily RANGE partitioning with automatic retention purging (`retention_days`).

### Security
- **SSRF Guard**: Prober target resolution validated by [`prober.Guard`](internal/prober/guard.go), blocking cloud metadata (`169.254.169.254`) and link-local ranges by default.
- **AES-256-GCM Secrets at Rest**: Keyring encryption for webhook secrets and channel credentials with zero-downtime key rotation (`cerbix reencrypt`).

---

> **Note on the two entries below.** `v0.35.0` and `v0.10.0` belong to an EARLIER numbering
> scheme and correspond to no git tag in this repository — the tag line is `v0.1.0-beta.*`.
> They are kept because they document real work; treat their version numbers as historical
> labels, not as releases anybody can check out.

## [v0.35.0] - 2026-08-01

### Added
- **Global Search**: Tenant-scoped search endpoint (`GET /api/v1/search`) across monitors, projects, and incidents.
- **p95 Latency**: SLA reporting enriched with `p95_latency_ms` via `percentile_cont(0.95)`.
- **UI 1:1 Design Sync**: Vue 3 SPA views rebuilt matching modern dark-theme design artifacts with bespoke inline-SVG charts.

---

## [v0.10.0] - 2026-07-25

### Added
- **Transactional Outbox**: Outbox delivery pipeline for notifications and incident webhooks with exponential backoff and dead-letter queue.
- **SLA & SLO**: Error budgets, maintenance window exclusion, and daily availability rollup aggregation.
- **Single Binary Multi-Role Execution**: `--role all|api|scheduler|worker` process execution model.
