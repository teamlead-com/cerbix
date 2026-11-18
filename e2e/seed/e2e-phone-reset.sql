-- The code budget of e2e/tests/phone-alerts.spec.ts (FR-041a). A run asks for two verification
-- codes as the bootstrap admin (its own number, and a fixed-number channel), and the hourly limit
-- counts the REQUESTER across every number it verifies (3 an hour, phone_verifications.requested_by,
-- review P2-3) — so without this a second `make dev-test` within the hour would meet send_limited.
-- The per-number daily limit is not reset: every run uses fresh random numbers.
--
-- `make dev-ready-single` applies it to the dev database before every dev-test; dev stacks only.
-- Idempotent, and a no-op on a database without migration 00120 (each table is guarded).
DO $$
BEGIN
    IF to_regclass('public.phone_verifications') IS NOT NULL THEN
        DELETE FROM phone_verifications v
         WHERE v.requested_by IN (SELECT u.id::text FROM users u WHERE u.email = 'admin@cerbix.local')
            OR (v.subject_kind = 'user'
                AND v.subject_id IN (SELECT u.id FROM users u WHERE u.email = 'admin@cerbix.local'))
            OR (v.subject_kind = 'channel'
                AND NOT EXISTS (SELECT 1 FROM notification_channels c WHERE c.id = v.subject_id));
    END IF;
    IF to_regclass('public.user_phones') IS NOT NULL THEN
        DELETE FROM user_phones p
         WHERE p.user_id IN (SELECT u.id FROM users u WHERE u.email = 'admin@cerbix.local');
    END IF;
END
$$;
