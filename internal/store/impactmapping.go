package store

import (
	"encoding/json"
	"fmt"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// impactMappingFromColumn resolves services.impact_by_state: NULL (nil/empty raw) is the D6
// defaults; otherwise the decoded object, validated.
func impactMappingFromColumn(raw []byte) (domain.ImpactMapping, error) {
	if len(raw) == 0 {
		return domain.DefaultImpactMapping(), nil
	}
	var m domain.ImpactMapping
	if err := json.Unmarshal(raw, &m); err != nil {
		return domain.ImpactMapping{}, fmt.Errorf("store: decode impact_by_state: %w", err)
	}
	if err := m.Validate(); err != nil {
		return domain.ImpactMapping{}, fmt.Errorf("store: stored impact_by_state: %w", err)
	}
	return m, nil
}

// impactMappingColumn is the value written (R3): nil (SQL NULL) when m.Resolved() equals
// domain.DefaultImpactMapping(), else the JSON object. Bind it as `$n::jsonb`.
func impactMappingColumn(m domain.ImpactMapping) (*string, error) {
	m = m.Resolved()
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if m == domain.DefaultImpactMapping() {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("store: encode impact_by_state: %w", err)
	}
	s := string(b)
	return &s, nil
}
