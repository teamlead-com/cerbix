-- +goose Up
-- sec-hardening-2 H-2 (iter-0210, D-0277): the WebSocket prober used to skip TLS verification
-- unconditionally and to dial a scheme-less target as ws://. From this release it verifies unless
-- the monitor's config says `tls_skip_verify: "true"`, and every write requires a scheme. Existing
-- monitors keep the behaviour they had, so none flips DOWN on upgrade:
--   * a wss:// or https:// target gets tls_skip_verify "true" (it was always skipped) — unless the
--     row already carries the key;
--   * a target with no scheme gets the ws:// it was dialled with (owner ruling, 2026-11-14).
-- Plaintext targets and every other type are untouched. The execution revision is NOT bumped:
-- nothing about how these probes behave changes. Every statement is idempotent.
--
-- The old prober trimmed its target with strings.TrimSpace, so every test below trims the same
-- ASCII whitespace: space, tab, LF, VT, FF, CR (second external review of iter-0210: a one-argument
-- btrim trims only spaces, and took "\nwss://…" for a scheme-less target). VT is written \x0b: in
-- a Postgres string literal \v is the letter v. The edges themselves are trimmed by 00115.
UPDATE monitors
   SET target = 'ws://' || btrim(target, E' \t\n\x0b\f\r')
 WHERE type = 'websocket'
   AND btrim(target, E' \t\n\x0b\f\r') <> ''
   AND btrim(target, E' \t\n\x0b\f\r') !~* '^(ws|wss|http|https)://';

UPDATE monitors
   SET config = coalesce(config, '{}'::jsonb) || '{"tls_skip_verify":"true"}'::jsonb
 WHERE type = 'websocket'
   AND btrim(target, E' \t\n\x0b\f\r') ~* '^(wss|https)://'
   AND NOT (coalesce(config, '{}'::jsonb) ? 'tls_skip_verify');

-- +goose Down
-- Intentionally empty: the Up section restores the behaviour these monitors already had, and a
-- rollback of the binary that reads them would restore the old prober behaviour by itself.
SELECT 1;
