package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// Service reliability endpoints (spec func-service-reliability §21).
//
// This is the PHASE 1 surface: the resource, its declaration, the evaluation epoch in force,
// and the state of materialization. SLO, error budget, burn rate and the coverage payload are
// phase 2 and are deliberately ABSENT rather than returned as zero — a field that reads `0%`
// because nothing computes it yet is exactly the confident-falsehood this feature exists to
// prevent.

// serviceMaxBody caps declaration bodies. A declaration is a couple of member lists and a
// policy block; 64 KiB is generous for that and bounds memory against an oversized body.
const serviceMaxBody = 64 << 10

// serviceView is the listing/detail DTO for the resource itself.
type serviceView struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	EscalationPolicyID string `json:"escalation_policy_id,omitempty"`
	OncallScheduleID   string `json:"oncall_schedule_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Managed names the file provider that owns this service, empty when it is UI-owned.
	// The UI needs it to know which controls to disable rather than to fail on 409.
	Managed string `json:"managed_by,omitempty"`
}

// declarationView is what a human declared. The two member lists are separate fields because
// they are separately declared: collapsing them in the API would invite a client to keep
// them in sync, which is the exact drift the split prevents.
type declarationView struct {
	Revision    int64                  `json:"revision"`
	EffectiveAt time.Time              `json:"effective_at"`
	CreatedAt   time.Time              `json:"created_at"`
	CreatedBy   string                 `json:"created_by,omitempty"`
	Monitors    []string               `json:"monitors"`
	SLI         []string               `json:"sli"`
	Policies    domain.ServicePolicies `json:"policies"`
}

// epochView is the evaluation epoch in force: what the system was MEASURING, as distinct
// from what a human declared.
type epochView struct {
	ID           string    `json:"id"`
	Seq          int64     `json:"seq"`
	EffectiveAt  time.Time `json:"effective_at"`
	SnapshotHash string    `json:"snapshot_hash"`
	Members      int       `json:"members"`
}

// materializationView is the honesty surface. `sealed_through` is contiguity-defined, so a
// service that has fallen behind shows a lagging timestamp rather than a plausible number,
// and a range still being computed reads as WORK IN PROGRESS rather than as missing data.
type materializationView struct {
	MaterializationStart *time.Time `json:"materialization_start,omitempty"`
	SealedThrough        *time.Time `json:"sealed_through,omitempty"`
	RetractedAt          *time.Time `json:"retracted_at,omitempty"`
	RetractedTo          *time.Time `json:"retracted_to,omitempty"`
	// Repairing lists the ranges whose numbers are not currently trustworthy. A client must
	// render these as repairing, never as data.
	Repairing []repairRangeView `json:"repairing"`
}

type repairRangeView struct {
	From     time.Time  `json:"from"`
	To       time.Time  `json:"to"`
	Reason   string     `json:"reason"`
	State    string     `json:"state"`
	Cursor   *time.Time `json:"cursor,omitempty"`
	Attempts int        `json:"attempts"`
	LastErr  string     `json:"last_error,omitempty"`
}

type serviceDetailView struct {
	Service         serviceView         `json:"service"`
	Declaration     *declarationView    `json:"declaration"`
	Epoch           *epochView          `json:"epoch"`
	Materialization materializationView `json:"materialization"`
	// Reliability is absent in phase 1 and the field says so explicitly, rather than
	// shipping zeros a client might render as a number.
	Reliability *struct{} `json:"reliability"`
}

func (h *Handler) writeServiceError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "slug already in use")
	case errors.Is(err, store.ErrServiceManagedByFile):
		writeError(w, http.StatusConflict, "managed_by_file")
	case errors.Is(err, store.ErrOwnerNotInProject):
		writeError(w, http.StatusBadRequest, "owner_not_in_project")
	case errors.Is(err, store.ErrTooManyServices):
		writeError(w, http.StatusConflict, "too_many_services")
	case errors.Is(err, store.ErrTooManyMembers):
		writeError(w, http.StatusBadRequest, "too_many_members")
	case errors.Is(err, store.ErrMonitorInTooManyServices):
		writeError(w, http.StatusConflict, "monitor_in_too_many_services")
	case errors.Is(err, store.ErrRevisionConflict):
		writeError(w, http.StatusConflict, "revision_conflict")
	case errors.Is(err, store.ErrSLINotInContext):
		writeError(w, http.StatusBadRequest, "sli_not_in_monitors")
	case errors.Is(err, store.ErrRetroactiveNotFirstRevision):
		writeError(w, http.StatusBadRequest, "retroactive_not_first_revision")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
	return true
}

// serviceSummaryView is one row of the services list.
//
// The list is where an operator decides which service to open, so it carries the watermark:
// omitting it would let a service that stopped materializing an hour ago look exactly like a
// healthy one. `revision: 0` means nothing has been declared — a valid state.
type serviceSummaryView struct {
	Service serviceView `json:"service"`

	Revision       int64      `json:"revision"`
	EffectiveAt    *time.Time `json:"effective_at,omitempty"`
	ContextMembers int        `json:"context_members"`
	SLIMembers     int        `json:"sli_members"`
	EpochSeq       int64      `json:"epoch_seq"`

	SealedThrough  *time.Time `json:"sealed_through,omitempty"`
	RepairingCount int        `json:"repairing_count"`
}

// listServices returns a project's services with the state a reader needs to choose one.
func (h *Handler) listServices(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	services, err := h.store.ListServiceSummaries(r.Context(), proj.ID)
	if h.writeServiceError(w, err) {
		return
	}
	out := make([]serviceSummaryView, 0, len(services))
	for _, v := range services {
		out = append(out, serviceSummaryView{
			Service: serviceView{
				ID: v.Service.ID, Slug: v.Service.Slug, Name: v.Service.Name,
				Description:        v.Service.Description,
				EscalationPolicyID: v.Service.EscalationPolicyID,
				OncallScheduleID:   v.Service.OncallScheduleID,
				CreatedAt:          v.Service.CreatedAt, UpdatedAt: v.Service.UpdatedAt,
				Managed: v.ManagedBy,
			},
			Revision: v.Revision, EffectiveAt: v.EffectiveAt,
			ContextMembers: v.ContextMembers, SLIMembers: v.SLIMembers,
			EpochSeq:      v.EpochSeq,
			SealedThrough: v.SealedThrough, RepairingCount: v.RepairingCount,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type createServiceRequest struct {
	Slug               string `json:"slug"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	EscalationPolicyID string `json:"escalation_policy_id"`
	OncallScheduleID   string `json:"oncall_schedule_id"`
}

// createService adds a Service with no declaration.
//
// A service with no reliability inputs is a valid state — operational context and no SLO —
// and it reports availability as unavailable rather than as 100%. Creating one is therefore
// a complete operation, not a half-finished wizard step.
func (h *Handler) createService(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	var req createServiceRequest
	if !decodeJSONBody(w, r, serviceMaxBody, &req) {
		return
	}
	if !domain.ValidServiceSlug(req.Slug) {
		writeError(w, http.StatusBadRequest, "slug must match "+domain.MonitorSlugPattern())
		return
	}
	if req.Name == "" {
		req.Name = req.Slug
	}
	svc, err := h.store.CreateService(r.Context(), domain.Service{
		ProjectID: proj.ID, Slug: req.Slug, Name: req.Name, Description: req.Description,
		EscalationPolicyID: req.EscalationPolicyID, OncallScheduleID: req.OncallScheduleID,
	})
	if h.writeServiceError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, serviceView{
		ID: svc.ID, Slug: svc.Slug, Name: svc.Name, Description: svc.Description,
		EscalationPolicyID: svc.EscalationPolicyID, OncallScheduleID: svc.OncallScheduleID,
		CreatedAt: svc.CreatedAt, UpdatedAt: svc.UpdatedAt,
	})
}

// getService returns one service with its declaration, the epoch in force, and the state of
// materialization.
func (h *Handler) getService(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	detail, err := h.store.ServiceDetail(r.Context(), proj.ID, r.PathValue("serviceID"))
	if h.writeServiceError(w, err) {
		return
	}
	out := serviceDetailView{
		Service: serviceView{
			ID: detail.Service.ID, Slug: detail.Service.Slug, Name: detail.Service.Name,
			Description:        detail.Service.Description,
			EscalationPolicyID: detail.Service.EscalationPolicyID,
			OncallScheduleID:   detail.Service.OncallScheduleID,
			CreatedAt:          detail.Service.CreatedAt, UpdatedAt: detail.Service.UpdatedAt,
			Managed: detail.ManagedBy,
		},
		Materialization: materializationView{
			MaterializationStart: detail.MaterializationStart,
			SealedThrough:        detail.SealedThrough,
			RetractedAt:          detail.RetractedAt,
			RetractedTo:          detail.RetractedTo,
			Repairing:            make([]repairRangeView, 0, len(detail.Repairing)),
		},
	}
	if detail.Declaration != nil {
		d := detail.Declaration
		out.Declaration = &declarationView{
			Revision: d.Revision, EffectiveAt: d.EffectiveAt, CreatedAt: d.CreatedAt,
			CreatedBy: d.CreatedBy, Monitors: d.Monitors, SLI: d.SLI, Policies: d.Policies,
		}
		if out.Declaration.Monitors == nil {
			out.Declaration.Monitors = []string{}
		}
		if out.Declaration.SLI == nil {
			out.Declaration.SLI = []string{}
		}
	}
	if detail.Epoch != nil {
		out.Epoch = &epochView{
			ID: detail.Epoch.ID, Seq: detail.Epoch.Seq, EffectiveAt: detail.Epoch.EffectiveAt,
			SnapshotHash: detail.Epoch.SnapshotHash, Members: len(detail.Epoch.Members),
		}
	}
	for _, rr := range detail.Repairing {
		v := repairRangeView{
			From: rr.From, To: rr.To, Reason: string(rr.Reason),
			State: rr.State, Attempts: rr.Attempts, LastErr: rr.LastError,
		}
		if !rr.Cursor.IsZero() {
			c := rr.Cursor
			v.Cursor = &c
		}
		out.Materialization.Repairing = append(out.Materialization.Repairing, v)
	}
	writeJSON(w, http.StatusOK, out)
}

type putDeclarationRequest struct {
	// ExpectedRevision is the revision the caller observed; 0 means "no declaration yet".
	// A mismatch is a 409 rather than a merge: two operators editing an SLI have made two
	// different statements about what availability means, and picking one silently is the
	// worst of the three options.
	ExpectedRevision int64 `json:"expected_revision"`

	Monitors []string               `json:"monitors"`
	SLI      []string               `json:"sli"`
	Policies domain.ServicePolicies `json:"policies"`

	// BackfillFrom makes the FIRST declaration retroactive, adopting existing history. What
	// it produces is a DECLARED RECONSTRUCTION evaluated with today's members — not evidence
	// about how they were configured then — and the UI must label it as such.
	BackfillFrom *time.Time `json:"backfill_from,omitempty"`
}

// putServiceDeclaration writes a new definition revision and its matching evaluation epoch.
func (h *Handler) putServiceDeclaration(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	var req putDeclarationRequest
	if !decodeJSONBody(w, r, serviceMaxBody, &req) {
		return
	}
	opts := store.DeclarationOptions{CreatedBy: h.actorLabel(r)}
	if req.BackfillFrom != nil {
		opts.BackfillFrom = *req.BackfillFrom
	}
	rev, epoch, err := h.store.PutServiceDeclaration(r.Context(), proj.ID, r.PathValue("serviceID"),
		domain.ServiceDeclaration{Monitors: req.Monitors, SLI: req.SLI, Policies: req.Policies},
		req.ExpectedRevision, opts)
	if h.writeServiceError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Declaration declarationView `json:"declaration"`
		Epoch       epochView       `json:"epoch"`
	}{
		Declaration: declarationView{
			Revision: rev.Revision, EffectiveAt: rev.EffectiveAt, CreatedAt: rev.CreatedAt,
			CreatedBy: rev.CreatedBy, Monitors: rev.Monitors, SLI: rev.SLI, Policies: rev.Policies,
		},
		Epoch: epochView{
			ID: epoch.ID, Seq: epoch.Seq, EffectiveAt: epoch.EffectiveAt,
			SnapshotHash: epoch.SnapshotHash, Members: len(epoch.Members),
		},
	})
}

// deleteService removes a service and everything derived from it, in one transaction.
func (h *Handler) deleteService(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	if h.writeServiceError(w, h.store.DeleteService(r.Context(), proj.ID, r.PathValue("serviceID"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeJSONBody reads a bounded JSON body, answering 400 on anything malformed.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, max int64, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, max))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	// The body is ONE JSON value: DisallowUnknownFields only guards fields inside the first
	// value, so `{...}{"burn_alert":true}` would otherwise pass with its trailer silently
	// ignored (iter-0141 P1-3). Fail closed on anything after it.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must be a single JSON value")
		return false
	}
	return true
}

// actorLabel is the principal recorded on a declaration. A revision states what availability
// MEANS, so who said so belongs on the row — including when it was a machine, which is why
// the synthetic token identity is kept rather than blanked.
func (h *Handler) actorLabel(r *http.Request) string {
	p, ok := h.principal(r)
	if !ok {
		return ""
	}
	return p.UserID
}
