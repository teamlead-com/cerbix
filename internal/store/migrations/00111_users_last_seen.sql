-- +goose Up
-- iter-0205 (D-0272, func-admin-users.md "Last active"): "last active" on the user and member
-- lists was max(sessions.created_at), forgotten as soon as the sessions were — logout, expiry and
-- the expired-session purge left signed-in users reading "never". It is a column of its own now,
-- stamped by sign-in and refreshed by use. A nullable column without a default: no table rewrite.
-- The backfill takes each user's newest REMAINING session; purged history is gone, so those users
-- read "never" until they next sign in. It never overwrites a stamp already present, so re-running
-- this section is harmless.
ALTER TABLE users ADD COLUMN IF NOT EXISTS last_seen_at timestamptz;

UPDATE users u
   SET last_seen_at = s.newest
  FROM (SELECT user_id, max(created_at) AS newest FROM sessions GROUP BY user_id) s
 WHERE s.user_id = u.id
   AND u.last_seen_at IS NULL;

-- +goose Down
ALTER TABLE users DROP COLUMN IF EXISTS last_seen_at;
