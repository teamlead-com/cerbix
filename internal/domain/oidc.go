package domain

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
}
