-- +goose Up
-- +goose StatementBegin

-- iter-0135 (round 2/2): the adoption paths — keyset copy, fenced month DELETE — filter by
-- bucket_start range, but every existing fact index leads with service_id, so their work was
-- bounded by wall clock only, never by rows examined. The partitioned index propagates to
-- every partition, DEFAULT included, and its (bucket_start, service_id) order is exactly the
-- copy's keyset cursor.
CREATE INDEX IF NOT EXISTS service_reliability_buckets_month_idx
    ON service_reliability_buckets (bucket_start, service_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS service_reliability_buckets_month_idx;
-- +goose StatementEnd
