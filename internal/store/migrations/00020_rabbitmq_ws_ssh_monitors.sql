-- +goose Up
-- Allow the rabbitmq/websocket/ssh monitor types (broker, WebSocket-upgrade, SSH-banner probers).

ALTER TABLE monitors DROP CONSTRAINT monitors_type_check;
ALTER TABLE monitors ADD CONSTRAINT monitors_type_check CHECK (type IN ('http', 'tcp', 'icmp', 'dns', 'tls', 'grpc', 'composite', 'postgres', 'mysql', 'redis', 'promql', 'rabbitmq', 'websocket', 'ssh', 'push'));

-- +goose Down
ALTER TABLE monitors DROP CONSTRAINT monitors_type_check;
ALTER TABLE monitors ADD CONSTRAINT monitors_type_check CHECK (type IN ('http', 'tcp', 'icmp', 'dns', 'tls', 'grpc', 'composite', 'postgres', 'mysql', 'redis', 'promql', 'push'));
