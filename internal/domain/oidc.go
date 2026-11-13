package domain

import (
	"errors"
	"strings"
)

// OIDCSettings is the instance-wide OIDC configuration held in the database and
// editable from the Settings UI. When a row exists it is authoritative — the YAML
// oidc: block is used only as a bootstrap seed before any UI override is saved.
type OIDCSettings struct {
	Enabled         bool     `json:"enabled"`
	Issuer          string   `json:"issuer"`
	ClientID        string   `json:"client_id"`
	ClientSecret    string   `json:"client_secret,omitempty"` // never emitted to clients; see ClientSecretSet
	RedirectURL     string   `json:"redirect_url"`
	Scopes          []string `json:"scopes"`
	PostLogoutURL   string   `json:"post_logout_url"`
	ButtonLabel     string   `json:"button_label"`
	BootstrapAdmins []string `json:"bootstrap_admins"`
	// MachineClients are the client ids whose client-credentials tokens the API's JWT bearer
	// path accepts (sec-hardening-2 H-1). Empty turns that path off. Never the interactive
	// ClientID — see NormalizeMachineClients.
	MachineClients []string `json:"machine_clients"`
}

// ErrOwnClientInMachineClients refuses a machine-client list that names the interactive client:
// every interactive cerbix token carries that client id, so listing it would accept them all.
var ErrOwnClientInMachineClients = errors.New("machine_clients must not contain this application's client_id")

// NormalizeMachineClients is the one normalization every path applies (YAML, settings API,
// runtime): entries are trimmed, empty ones dropped, duplicates removed keeping the first-seen
// order; an entry equal to the (trimmed) interactive client id is refused. The result is never nil.
func NormalizeMachineClients(in []string, ownClientID string) ([]string, error) {
	own := strings.TrimSpace(ownClientID)
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		if own != "" && id == own {
			return nil, ErrOwnClientInMachineClients
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}
