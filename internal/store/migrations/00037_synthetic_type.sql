-- +goose Up
-- Allow the synthetic monitor type (scripted multi-step HTTP scenario) through the
-- monitors.type whitelist CHECK.
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_type_check;
ALTER TABLE monitors ADD CONSTRAINT monitors_type_check
    CHECK (type IN ('http', 'tcp', 'icmp', 'dns', 'tls', 'grpc', 'composite', 'postgres', 'mysql', 'redis', 'promql', 'rabbitmq', 'websocket', 'ssh', 'synthetic', 'push'));

-- +goose Down
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_type_check;
ALTER TABLE monitors ADD CONSTRAINT monitors_type_check
    CHECK (type IN ('http', 'tcp', 'icmp', 'dns', 'tls', 'grpc', 'composite', 'postgres', 'mysql', 'redis', 'promql', 'rabbitmq', 'websocket', 'ssh', 'push'));
