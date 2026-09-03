package api

import (
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/store"
)

// auditActor builds the principal an audited incident write records, exactly as the gate builds its
// own (FR-026 D3). It exists so no handler assembles the three fields by hand — a half-wired
// construction would be refused by the store's empty-label check, but a shared builder means the
// refusal never has to fire.
//
// `AuditUserID` is empty for a token principal, whose identity is synthetic and has no user row; the
// label carries who it was, which is why D5 appends it to the target for a token write.
func auditActor(p authz.Principal) store.AuditActor {
	return store.AuditActor{
		ActorUserID: p.AuditUserID(),
		ViaToken:    p.ViaToken,
		Label:       p.AuditActorLabel(),
	}
}
