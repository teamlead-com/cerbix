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

// The bare monitor writers, for FIXTURES. The product exports none of them (FR-026 §10, D11): a
// handler reaches only the …ByPrincipal doors. Tests seed monitors by the dozen and none of that is a
// principal's write, so they get the unexported writer with no audit hook — through a file that is
// compiled into the test binary only.
func (s *Store) CreateMonitor(ctx context.Context, m domain.Monitor) (domain.Monitor, error) {
	return s.createMonitor(ctx, m, nil)
}

func (s *Store) UpdateMonitor(ctx context.Context, m domain.Monitor) (domain.Monitor, error) {
	return s.updateMonitor(ctx, m, nil)
}

func (s *Store) DeleteMonitor(ctx context.Context, id string) error {
	return s.deleteMonitor(ctx, id, nil)
}
