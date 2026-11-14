-- +goose Up
-- sec-hardening-2 H-2 (S2), external review of iter-0210 (P2-2): a control byte in a monitor's RAW
-- target is refused on every write surface, and the websocket, ssh, redis and rabbitmq probers
-- re-check the raw target before they dial. Before this release those probers — and postgres and
-- mysql, which still do — trimmed the target first (strings.TrimSpace), so a row whose target
-- starts or ends with CR, LF, a tab, VT or FF probed fine. This trims that ASCII whitespace from
-- the edges of those six types' targets, so none flips DOWN on upgrade or answers 400 at its next
-- PATCH; the probe dials exactly what it dialled before.
--   * Interior bytes are left alone: no prober ever accepted them.
--   * Other types are left alone: their probers never trimmed, so such a row never probed — save
--     an http target with control bytes after its "#", which the last statement repairs.
-- The execution revision is NOT bumped. Every statement is idempotent: a repaired target never
-- matches its statement again.
--
-- In the regular expressions below \v is the regex engine's vertical tab; in a plain string
-- literal (00113's btrim) it would be the letter v, which is why 00113 writes \x0b.
UPDATE monitors
   SET target = regexp_replace(target, E'^[ \\t\\n\\v\\f\\r]+|[ \\t\\n\\v\\f\\r]+$', '', 'g')
 WHERE type IN ('websocket', 'ssh', 'redis', 'rabbitmq', 'postgres', 'mysql')
   AND target ~ E'^[ \\t\\n\\v\\f\\r]|[ \\t\\n\\v\\f\\r]$';

-- Repair for a database that ran the FIRST 00113 (second external review of iter-0210, P1-1). It
-- trimmed only spaces, so a target such as "\nwss://host/ws" was taken for a scheme-less one: it
-- became "ws://\nwss://host/ws" without tls_skip_verify, and the first 00115 then made it
-- "ws://wss://host/ws" — unparseable, DOWN. The doubled prefix collapses to the scheme the operator
-- wrote, and a wss/https row gets the tls_skip_verify "true" the first 00113 owed it, unless it
-- carries the key. Only the rows that migration damaged are touched: a wss monitor SAVED after it
-- without the key chose verification. The fixed 00113 never produces this shape.
UPDATE monitors
   SET target = regexp_replace(target, E'^ws://[ \\t\\n\\v\\f\\r]*((ws|wss|http|https)://)', E'\\1', 'i'),
       config = CASE
                  WHEN target ~* E'^ws://[ \\t\\n\\v\\f\\r]*(wss|https)://'
                   AND NOT (coalesce(config, '{}'::jsonb) ? 'tls_skip_verify')
                  THEN coalesce(config, '{}'::jsonb) || '{"tls_skip_verify":"true"}'::jsonb
                  ELSE config
                END
 WHERE type = 'websocket'
   AND target ~* E'^ws://[ \\t\\n\\v\\f\\r]*(ws|wss|http|https)://';

-- The same first 00113 prefixed a scheme-less target with a leading LF/tab as "ws://\nhost/ws";
-- the whitespace after the prefix goes.
UPDATE monitors
   SET target = regexp_replace(target, E'^ws://[ \\t\\n\\v\\f\\r]+', 'ws://')
 WHERE type = 'websocket'
   AND target ~ E'^ws://[ \\t\\n\\v\\f\\r]';

-- http (second external review of iter-0210, P2-1): the http prober now parses its target, which
-- refuses a control byte anywhere. On main an http target probed with control bytes in exactly one
-- place: after the first "#". url.Parse checks for control bytes only before the fragment, and the
-- fragment is never sent, so "https://app/health#/dashboard\r\n" was UP. Exactly those rows lose
-- the control bytes of their fragment:
--   * the part before the first "#" is an http(s) URL naming a host, with no control byte;
--   * the fragment has a control byte;
--   * the fragment's percent-escapes are valid (url.Parse refuses "%zz" there too, so a row with
--     one never probed).
-- The request each row sends is unchanged. Every other http row never probed and is left alone.
UPDATE monitors
   SET target = split_part(target, '#', 1) || '#'
                || regexp_replace(substr(target, strpos(target, '#') + 1), E'[\\x01-\\x1f\\x7f]', '', 'g')
 WHERE type = 'http'
   AND strpos(target, '#') > 0
   AND split_part(target, '#', 1) ~* '^https?://[^/?#:]'
   AND split_part(target, '#', 1) !~ E'[\\x01-\\x1f\\x7f]'
   AND substr(target, strpos(target, '#') + 1) ~ E'[\\x01-\\x1f\\x7f]'
   AND substr(target, strpos(target, '#') + 1) !~ '%(?![0-9A-Fa-f]{2})';

-- +goose Down
-- Intentionally empty: the trimmed target is the one the older probers dialled anyway.
SELECT 1;
