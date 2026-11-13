package auth

import (
	"context"
	"errors"
	"log/slog"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// The OIDC client-credentials bearer path (D-0034, narrowed by sec-hardening-2 H-1, D-0276).
//
// Invariant S1: a realm JWT is accepted only if its client claim names a listed machine client,
// it carries no delegated-session marker, and its subject is not a human user; every other JWT is
// 401 and changes no row.

// BearerRefusalRecorder counts JWTs refused on the client-credentials bearer path by bounded
// reason (implemented by *metrics.Registry as cerbix_oidc_bearer_refused_total{reason}).
type BearerRefusalRecorder interface {
	RecordOIDCBearerRefused(reason string)
}

// WithMetrics wires the refusal counter of the OIDC bearer path.
func (a *Authenticator) WithMetrics(m BearerRefusalRecorder) *Authenticator {
	a.bearerRefusals = m
	return a
}

// Refusal reasons: the bounded label set of the log line and the metric.
const (
	refusedInvalidToken         = "invalid_token"
	refusedInvalidClaims        = "invalid_claims"
	refusedMachineClientsEmpty  = "machine_clients_empty"
	refusedClientClaimMissing   = "client_claim_missing"
	refusedClientClaimsDisagree = "client_claims_disagree"
	refusedClientNotListed      = "client_not_listed"
	refusedDelegatedNonce       = "delegated_nonce"
	refusedDelegatedSid         = "delegated_sid"
	refusedDelegatedCidSub      = "delegated_cid_sub"
	refusedSubjectIsHuman       = "subject_is_human"
)

// jwtClaims is what the bearer path reads from a verified token. Every field is a string: a claim
// of another JSON type fails the decode, and the token is refused (invalid_claims).
type jwtClaims struct {
	Azp               string `json:"azp"`
	ClientID          string `json:"client_id"`
	Cid               string `json:"cid"`   // Okta
	Appid             string `json:"appid"` // Entra ID v1
	Nonce             string `json:"nonce"`
	Sid               string `json:"sid"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
}

// clientClaim names the client a token was issued to: the first present of azp, client_id, cid,
// appid. Every non-empty member of that set must agree — "first present wins" alone would let one
// claim shadow another. Returns the claim's name and value, or a refusal reason.
func clientClaim(c jwtClaims) (name, value, reason string) {
	for _, cl := range []struct{ name, value string }{
		{"azp", c.Azp}, {"client_id", c.ClientID}, {"cid", c.Cid}, {"appid", c.Appid},
	} {
		switch {
		case cl.value == "":
		case value == "":
			name, value = cl.name, cl.value
		case cl.value != value:
			return name, value, refusedClientClaimsDisagree
		}
	}
	if value == "" {
		return "", "", refusedClientClaimMissing
	}
	return name, value, ""
}

// principalFromJWT validates an OIDC client-credentials token (issuer + signature; the audience is
// relaxed because a machine token's `aud` is its own client) and, when it passes every S1 rule,
// provisions the `machine` user keyed by the token subject and builds a principal from that
// identity's memberships. Every refusal is ok=false with a nil error — the caller answers 401 —
// and is logged and counted. An error is returned only for a store failure (500).
func (a *Authenticator) principalFromJWT(ctx context.Context, raw string) (authz.Principal, bool, error) {
	rt := a.rt()
	if rt == nil {
		return authz.Principal{}, false, nil // OIDC inactive: nothing to refuse, just not ours
	}
	idt, err := rt.ccVerifier.Verify(ctx, raw)
	if err != nil {
		// Any bearer string that is not a cerbix API token lands here, so this one is Debug.
		a.refuseBearer(slog.LevelDebug, refusedInvalidToken, "", "", "", "")
		return authz.Principal{}, false, nil
	}
	var c jwtClaims
	if err := idt.Claims(&c); err != nil {
		a.refuseBearer(slog.LevelWarn, refusedInvalidClaims, "", "", idt.Issuer, idt.Subject)
		return authz.Principal{}, false, nil
	}
	claimName, client, reason := clientClaim(c)
	refuse := func(reason string) (authz.Principal, bool, error) {
		a.refuseBearer(slog.LevelWarn, reason, claimName, client, idt.Issuer, idt.Subject)
		return authz.Principal{}, false, nil
	}
	switch {
	case reason != "":
		return refuse(reason)
	case len(rt.machineClients) == 0:
		return refuse(refusedMachineClientsEmpty)
	case !rt.machineClients[client]:
		return refuse(refusedClientNotListed)
	// A listed client that also allows a user flow mints tokens with the same client claim and a
	// human's sub; these markers betray a delegated token. They narrow the hole — what closes it
	// is the human/machine split below, and the IdP rule that a listed client is service-only.
	case c.Nonce != "":
		return refuse(refusedDelegatedNonce)
	case c.Sid != "":
		return refuse(refusedDelegatedSid)
	case c.Cid != "" && c.Cid != idt.Subject:
		return refuse(refusedDelegatedCidSub)
	}

	display := c.PreferredUsername
	if display == "" {
		display = client
	}
	email := c.Email
	if email == "" {
		email = idt.Subject + "@clients" // service accounts often carry no email
	}
	// The email allowlist is an interactive-login rule: a machine token carries no verified email.
	user, err := a.store.UpsertMachineUserByOIDCIdentity(ctx, idt.Issuer, idt.Subject, email, display)
	if errors.Is(err, store.ErrSubjectIsHuman) {
		return refuse(refusedSubjectIsHuman)
	}
	if err != nil {
		return authz.Principal{}, false, err
	}
	principal, err := a.loadPrincipal(ctx, user.ID)
	if err != nil {
		return authz.Principal{}, false, err
	}
	// The kind that decides is read AFTER the memberships: a concurrent sign-in may have flipped
	// the row to human (and granted it) between the upsert and loadPrincipal's two reads. Machine
	// → human is irreversible, so `machine` read now proves the memberships were a machine's.
	again, err := a.store.GetUser(ctx, user.ID)
	if err != nil {
		return authz.Principal{}, false, err
	}
	if again.Kind != domain.UserKindMachine {
		return refuse(refusedSubjectIsHuman)
	}
	principal.ViaToken = true
	// S1: no machine is a global admin. The PATCH refusal and the human flip keep the flag off a
	// machine row; this keeps a flag set outside the API from ever reaching a bearer principal.
	principal.IsGlobalAdmin = false
	a.logger.Debug("oidc_bearer_accepted", "client_claim", claimName, "client", client, "user_id", user.ID)
	return principal, true, nil
}

// refuseBearer logs `oidc_bearer_refused` and counts it by reason.
func (a *Authenticator) refuseBearer(level slog.Level, reason, claimName, client, issuer, sub string) {
	a.logger.Log(context.Background(), level, "oidc_bearer_refused",
		"reason", reason, "client_claim", claimName, "client", client, "issuer", issuer, "sub", sub)
	if a.bearerRefusals != nil {
		a.bearerRefusals.RecordOIDCBearerRefused(reason)
	}
}
