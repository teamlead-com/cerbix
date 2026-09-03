package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-026 / NFR-021 — an incident write says who wrote it.
//
// FR-022 promised, as its invariant 14, that "every write is audited with actor and tenant, in the
// mutating transaction". Building the discharge map found the promise false of the PRODUCT rather
// than merely unimplemented: an incident write left NO `audit_logs` row, for either anchor. This file
// is that promise made true.
//
// The shape is D3's, and it is the reason this is a door split rather than an argument: revision 1 of
// the design passed an actor into one shared writer and read `nil` as "machine", which is a silent
// bypass — an API handler that forgets the argument produces exactly what FR-026 forbids, and the
// empty-label check cannot tell an omission from a machine. With two entry points, a forgotten actor
// is a COMPILE error.

// AuditActor is the principal behind an audited incident write. Same shape as GateActor and
// SecretActor, deliberately not collapsed into them by this requirement (D3).
type AuditActor struct {
	// ActorUserID is a soft user FK; empty is stored as NULL, which is what a token principal has.
	ActorUserID string
	// ViaToken marks an API-token principal.
	ViaToken bool
	// Label is the principal's audit label. A zero-valued actor is refused before any statement, so
	// a half-wired construction fails loudly rather than writing an anonymous row.
	Label string
}

func (a AuditActor) userID() *string {
	if a.ActorUserID == "" {
		return nil
	}
	id := a.ActorUserID
	return &id
}

// valid refuses the two shapes that are not attributions. An empty label is the half-wired
// construction; a label with NEITHER a user id NOR the token flag is worse — it reads as a session
// user who has no user row, which is nobody. The three legal shapes are the three principals that
// exist: a session user (id, not via token), a Cerbix token (no id, via token) and an OIDC
// client-credentials principal (a real id, via token).
func (a AuditActor) valid() error {
	if strings.TrimSpace(a.Label) == "" {
		return fmt.Errorf("store: an audited incident write needs an actor label")
	}
	if a.ActorUserID == "" && !a.ViaToken {
		return fmt.Errorf("store: an audited incident write needs a principal, not a bare label")
	}
	return nil
}

// IncidentAuditAction is the closed vocabulary of D4 — five words, as a TYPE rather than as a string,
// so no incident path can spell an action the list does not have.
//
// The closure is a property of this helper and NOT of the column: `audit_logs.action` is free text
// with no CHECK and already carries `member.add`, `token.create`, `gate.policy.*` and more. Revision 1
// of the design claimed the table itself refused other spellings, which was false.
type IncidentAuditAction string

const (
	IncidentAuditCreate      IncidentAuditAction = "incident.create"
	IncidentAuditStatus      IncidentAuditAction = "incident.status"
	IncidentAuditNote        IncidentAuditAction = "incident.note"
	IncidentAuditAcknowledge IncidentAuditAction = "incident.acknowledge"
	IncidentAuditPostmortem  IncidentAuditAction = "incident.postmortem"
)

// insertIncidentAudit writes the row inside the caller's transaction (D2), resolving the tenant from
// the project rather than taking it as a parameter (D6): the organization owns this table, and an
// incident keeps its project even after a service deletion clears its anchor, so a project-level
// incident and an orphaned one both resolve.
//
// An error here ABORTS the mutation (D7). That is the intended trade: an incident write that cannot
// be attributed does not happen — which is also why the statement is one insert with no lookup and no
// second round trip.
func insertIncidentAudit(ctx context.Context, tx pgx.Tx, projectID string, actor AuditActor,
	action IncidentAuditAction, target string) error {

	if err := actor.valid(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
		SELECT p.org_id, $2, $3, $4, $5 FROM projects p WHERE p.id = $1`,
		projectID, actor.userID(), actor.ViaToken, string(action), target); err != nil {
		return fmt.Errorf("store: audit %s: %w", action, err)
	}
	return nil
}

// incidentAuditTarget builds the metadata shapes of D5 — fixed here so a reader can parse them by eye
// and a test can assert them. A token principal appends its label, because `actor_user_id` is NULL for
// a synthetic token identity and the typed half would otherwise read "some token".
func incidentAuditTarget(actor AuditActor, body string) string {
	if actor.ViaToken {
		return body + " · actor=" + actor.Label
	}
	return body
}

func incidentAuditAnchor(inc domain.Incident) string {
	switch {
	case inc.MonitorID != "":
		return "monitor " + inc.MonitorID
	case inc.ServiceID != "":
		return "service " + inc.ServiceID
	default:
		return "project-level"
	}
}

// IncidentCreateTarget, IncidentStatusTarget and the rest are exported so the tests that pin D5's
// shapes read the same builder the product uses rather than a copy that can drift.
func IncidentCreateTarget(actor AuditActor, inc domain.Incident) string {
	return incidentAuditTarget(actor, fmt.Sprintf("incident %s · %s · impact=%s · source=%s",
		inc.ID, incidentAuditAnchor(inc), inc.Impact, inc.Source))
}

func IncidentStatusTarget(actor AuditActor, id string, from, to domain.IncidentStatus) string {
	return incidentAuditTarget(actor, fmt.Sprintf("incident %s · %s → %s", id, from, to))
}

func IncidentNoteTarget(actor AuditActor, id string) string {
	return incidentAuditTarget(actor, "incident "+id+" · note")
}

func IncidentAcknowledgeTarget(actor AuditActor, id string) string {
	return incidentAuditTarget(actor, "incident "+id+" · acknowledged")
}

func IncidentPostmortemTarget(actor AuditActor, id string, created bool) string {
	verb := "updated"
	if created {
		verb = "created"
	}
	return incidentAuditTarget(actor, "incident "+id+" · postmortem "+verb)
}
