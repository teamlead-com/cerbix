-- +goose Up
-- sec-hardening-2 H-7 (S7, iter-0212, D-0279): a TOTP code authenticates at most once. The local
-- sign-in records the time step of the code it accepted, with one compare-and-set
--   UPDATE users SET totp_last_step = $2 WHERE id = $1 AND (totp_last_step IS NULL OR totp_last_step < $2)
-- so a replayed code — sequential or concurrent — is refused. NULL means "no step accepted since
-- this secret was enrolled": SetTOTPSecret and DisableTOTP write NULL. TOTP state lives on `users`
-- (there is no separate row). Enabling 2FA does not write the step (see the spec). Idempotent.
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_last_step bigint NULL;

-- +goose Down
ALTER TABLE users DROP COLUMN IF EXISTS totp_last_step;
