package store

import (
	"context"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Test-only bridges. This file compiles only under `go test`, so the unchecked maintenance
// writers stay unexported to production while external-package tests can still seed fixtures
// that deliberately predate the checked contract.

// SeedMaintenanceWindowForTest inserts a window with no preview/generation/repair/audit.
func (s *Store) SeedMaintenanceWindowForTest(ctx context.Context, mw domain.MaintenanceWindow) (domain.MaintenanceWindow, error) {
	return s.createMaintenanceWindowUnchecked(ctx, mw)
}
