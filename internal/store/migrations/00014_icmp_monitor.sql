-- +goose Up
-- Allow the icmp/dns/tls monitor types (ping, DNS-resolution, TLS-cert probers).

ALTER TABLE monitors DROP CONSTRAINT monitors_type_check;
ALTER TABLE monitors ADD CONSTRAINT monitors_type_check CHECK (type IN ('http', 'tcp', 'icmp', 'dns', 'tls', 'grpc', 'composite', 'postgres', 'mysql', 'redis', 'promql', 'push'));

-- +goose Down
ALTER TABLE monitors DROP CONSTRAINT monitors_type_check;
ALTER TABLE monitors ADD CONSTRAINT monitors_type_check CHECK (type IN ('http', 'tcp', 'push'));
