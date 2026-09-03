-- +goose Up
-- FR-029: `async_canary` joins the monitor type CHECK.
--
-- The domain, the file provider, the API and the executor all knew the type before this migration
-- did, and the database answered a create with a 23514 the API could only report as a 500 — the
-- constraint is the LAST place a new type has to be told, and the easiest to forget because every
-- other layer is Go and this one is SQL. Found by the live E2E on a rebuilt image, which is exactly
-- the thing a live gate exists to catch: every unit test in the tree passed with the type missing
-- here, because none of them writes to a real `monitors` table with the real constraint.
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_type_check;
ALTER TABLE monitors ADD CONSTRAINT monitors_type_check CHECK (type = ANY (ARRAY[
    'http', 'tcp', 'icmp', 'dns', 'tls', 'grpc', 'composite', 'postgres', 'mysql', 'redis',
    'promql', 'rabbitmq', 'websocket', 'ssh', 'synthetic', 'async_canary', 'push'
]));

-- +goose Down
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_type_check;
ALTER TABLE monitors ADD CONSTRAINT monitors_type_check CHECK (type = ANY (ARRAY[
    'http', 'tcp', 'icmp', 'dns', 'tls', 'grpc', 'composite', 'postgres', 'mysql', 'redis',
    'promql', 'rabbitmq', 'websocket', 'ssh', 'synthetic', 'push'
]));
