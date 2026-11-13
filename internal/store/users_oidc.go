package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// OIDC just-in-time provisioning, split by who is signing in (sec-hardening-2 H-1). Both methods
// key on the OIDC identity (issuer, subject) — not the subject alone, so two providers minting the
// same `sub` map to distinct accounts (migration 00056) — and never match by email.

// ErrSubjectIsHuman is returned by UpsertMachineUserByOIDCIdentity when the OIDC identity belongs
// to a human user: the bearer path never authenticates a human.
var ErrSubjectIsHuman = errors.New("store: oidc subject belongs to a human user")

// HumanFlip reports what UpsertHumanUserByOIDCIdentity did to a row that was a machine.
type HumanFlip struct {
	Flipped        bool                // the row was a machine and is now human
	WasGlobalAdmin bool                // the machine row held the global-admin flag (now cleared)
	Revoked        []domain.Membership // every membership the machine held, now deleted
}

// claimLegacyOIDCUser is the one-time claim of a row provisioned before the issuer column existed
// (issuer NULL, this subject): it adopts the current issuer, unless (issuer, sub) already exists —
// never steal a bound identity. Both upserts run it inside their transaction, so a refusal rolls
// the claim back with everything else.
func claimLegacyOIDCUser(ctx context.Context, tx pgx.Tx, issuer, sub string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE users SET oidc_issuer = $1
		   WHERE oidc_issuer IS NULL AND oidc_sub = $2
		     AND NOT EXISTS (SELECT 1 FROM users u2 WHERE u2.oidc_issuer = $1 AND u2.oidc_sub = $2)`,
		issuer, sub); err != nil {
		return fmt.Errorf("store: claim legacy oidc user: %w", err)
	}
	return nil
}

// UpsertMachineUserByOIDCIdentity provisions the identity of an OIDC client-credentials token on
// the bearer path: it creates a `machine` user, or refreshes the email and display name of the
// machine already there. When the identity is a HUMAN it returns ErrSubjectIsHuman and the
// transaction rolls back, so a refusal changes no row — not even the legacy issuer claim. It never
// grants global admin.
func (s *Store) UpsertMachineUserByOIDCIdentity(ctx context.Context, issuer, sub, email, displayName string) (domain.User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.User{}, fmt.Errorf("store: begin upsert machine user: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := claimLegacyOIDCUser(ctx, tx, issuer, sub); err != nil {
		return domain.User{}, err
	}
	// ON CONFLICT … WHERE: a human row is not updated and RETURNING yields nothing. A row locked by
	// a concurrent sign-in is waited for, and the WHERE is re-checked against its new version.
	u, err := scanUser(tx.QueryRow(ctx,
		`INSERT INTO users (oidc_issuer, oidc_sub, email, display_name, kind)
		 VALUES ($1, $2, $3, $4, 'machine')
		 ON CONFLICT (oidc_issuer, oidc_sub) WHERE oidc_sub IS NOT NULL
		 DO UPDATE SET email = EXCLUDED.email, display_name = EXCLUDED.display_name, updated_at = now()
		 WHERE users.kind = 'machine'
		 RETURNING `+userColumns,
		issuer, sub, email, displayName))
	if noRows(err) {
		return domain.User{}, ErrSubjectIsHuman // the deferred rollback undoes the legacy claim too
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("store: upsert machine user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.User{}, fmt.Errorf("store: commit upsert machine user: %w", err)
	}
	return u, nil
}

// UpsertHumanUserByOIDCIdentity provisions the identity of an interactive OIDC sign-in. The row
// becomes `human`, permanently. When it was a `machine`, everything granted to the machine goes in
// the same transaction — every membership and the global-admin flag — with audit rows: a person
// must never inherit what was granted to something badged as a machine.
//
// The existing row is read FOR UPDATE because the old kind must be known. When that read finds no
// row it locks nothing, so the insert is ON CONFLICT DO NOTHING: an empty result means a concurrent
// bearer request created the row first, and the FOR UPDATE branch runs again — it then sees the
// machine and strips it. (ON CONFLICT DO UPDATE SET kind = 'human' would flip the row while
// skipping the strip and the audit.)
func (s *Store) UpsertHumanUserByOIDCIdentity(ctx context.Context, issuer, sub, email, displayName string) (domain.User, HumanFlip, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.User{}, HumanFlip{}, fmt.Errorf("store: begin upsert human user: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := claimLegacyOIDCUser(ctx, tx, issuer, sub); err != nil {
		return domain.User{}, HumanFlip{}, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		var (
			id, kind string
			admin    bool
		)
		err := tx.QueryRow(ctx,
			`SELECT id, kind, is_global_admin FROM users
			  WHERE oidc_issuer = $1 AND oidc_sub = $2 FOR UPDATE`, issuer, sub).Scan(&id, &kind, &admin)
		if noRows(err) {
			u, ierr := scanUser(tx.QueryRow(ctx,
				`INSERT INTO users (oidc_issuer, oidc_sub, email, display_name, kind)
				 VALUES ($1, $2, $3, $4, 'human')
				 ON CONFLICT (oidc_issuer, oidc_sub) WHERE oidc_sub IS NOT NULL DO NOTHING
				 RETURNING `+userColumns,
				issuer, sub, email, displayName))
			if noRows(ierr) {
				continue // a concurrent transaction created the row: take the FOR UPDATE branch
			}
			if ierr != nil {
				return domain.User{}, HumanFlip{}, fmt.Errorf("store: insert human user: %w", ierr)
			}
			if err := tx.Commit(ctx); err != nil {
				return domain.User{}, HumanFlip{}, fmt.Errorf("store: commit upsert human user: %w", err)
			}
			return u, HumanFlip{}, nil
		}
		if err != nil {
			return domain.User{}, HumanFlip{}, fmt.Errorf("store: lock oidc user: %w", err)
		}

		var flip HumanFlip
		if kind == domain.UserKindMachine {
			if flip, err = stripMachineGrants(ctx, tx, id, email, admin); err != nil {
				return domain.User{}, HumanFlip{}, err
			}
		}
		u, err := scanUser(tx.QueryRow(ctx,
			`UPDATE users SET kind = 'human', email = $2, display_name = $3, updated_at = now(),
			        is_global_admin = CASE WHEN $4 THEN false ELSE is_global_admin END
			  WHERE id = $1
			 RETURNING `+userColumns,
			id, email, displayName, flip.Flipped))
		if err != nil {
			return domain.User{}, HumanFlip{}, fmt.Errorf("store: update human user: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.User{}, HumanFlip{}, fmt.Errorf("store: commit upsert human user: %w", err)
		}
		return u, flip, nil
	}
	return domain.User{}, HumanFlip{}, errors.New("store: upsert human user: row neither found nor inserted")
}

// stripMachineGrants deletes every membership of a machine row that is becoming human and writes
// the audit trail: one instance-level row, and one row per organization that lost grants, so each
// organization's own audit log shows what its machine integration lost. The actor is the user.
func stripMachineGrants(ctx context.Context, tx pgx.Tx, userID, email string, wasAdmin bool) (HumanFlip, error) {
	flip := HumanFlip{Flipped: true, WasGlobalAdmin: wasAdmin}
	rows, err := tx.Query(ctx, `DELETE FROM memberships WHERE user_id = $1 RETURNING `+membershipColumns, userID)
	if err != nil {
		return HumanFlip{}, fmt.Errorf("store: revoke machine grants: %w", err)
	}
	for rows.Next() {
		m, err := scanMembership(rows)
		if err != nil {
			rows.Close()
			return HumanFlip{}, fmt.Errorf("store: scan revoked grant: %w", err)
		}
		flip.Revoked = append(flip.Revoked, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return HumanFlip{}, fmt.Errorf("store: revoke machine grants: %w", err)
	}
	sort.Slice(flip.Revoked, func(i, j int) bool {
		a, b := flip.Revoked[i], flip.Revoked[j]
		if a.OrgID != b.OrgID {
			return a.OrgID < b.OrgID
		}
		return a.ProjectID < b.ProjectID
	})

	audit := func(orgID *string, action, target string) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target) VALUES ($1, $2, false, $3, $4)`,
			orgID, userID, action, target); err != nil {
			return fmt.Errorf("store: audit machine flip: %w", err)
		}
		return nil
	}
	if err := audit(nil, "user.kind_human", fmt.Sprintf(
		"%s · user %s · machine became human on sign-in; revoked %d membership(s); global admin cleared: %v",
		email, userID, len(flip.Revoked), wasAdmin)); err != nil {
		return HumanFlip{}, err
	}
	byOrg := map[string][]string{}
	var orgs []string
	for _, m := range flip.Revoked {
		if _, ok := byOrg[m.OrgID]; !ok {
			orgs = append(orgs, m.OrgID)
		}
		byOrg[m.OrgID] = append(byOrg[m.OrgID], grantLabel(m))
	}
	for _, org := range orgs {
		if err := audit(&org, "member.revoke_machine_grants", fmt.Sprintf("%s · user %s · revoked: %s",
			email, userID, strings.Join(byOrg[org], "; "))); err != nil {
			return HumanFlip{}, err
		}
	}
	return flip, nil
}

func grantLabel(m domain.Membership) string {
	if m.ProjectID == "" {
		return "org " + m.OrgID + " role " + string(m.Role)
	}
	return "project " + m.ProjectID + " role " + string(m.Role)
}
