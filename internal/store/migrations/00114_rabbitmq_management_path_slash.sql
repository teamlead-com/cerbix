-- +goose Up
-- sec-hardening-2 H-2 fix 2, external review of iter-0210 (P2-1): a RabbitMQ management `path`
-- starts with a single "/". The prober used to prepend that "/" to a relative path silently; from
-- this release it refuses one, as the write surfaces already did. Existing rows get the "/" the
-- prober used to add, so the request each monitor sends is unchanged and none flips DOWN on
-- upgrade. Only rabbitmq rows with a non-empty path that does not start with "/" are touched; an
-- empty or absent path still means /api/overview. The execution revision is NOT bumped: the probe's
-- request is the same. Idempotent: a path that starts with "/" is never matched again.
UPDATE monitors
   SET config = config || jsonb_build_object('path', '/' || (config->>'path'))
 WHERE type = 'rabbitmq'
   AND coalesce(config->>'path', '') <> ''
   AND left(config->>'path', 1) <> '/';

-- +goose Down
-- Intentionally empty: a path with its leading "/" is what the older prober built anyway.
SELECT 1;
