package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestMainNoArgsReturnsUsageError(t *testing.T) {
	if code := Main(nil); code != 2 {
		t.Fatalf("Main(nil) = %d, want 2", code)
	}
}

func TestMainVersion(t *testing.T) {
	if code := Main([]string{"version"}); code != 0 {
		t.Fatalf("Main(version) = %d, want 0", code)
	}
}

func TestServeRequiresConfig(t *testing.T) {
	if code := Main([]string{"serve"}); code != 2 {
		t.Fatalf("serve without --config = %d, want 2", code)
	}
}

func TestServeRejectsInvalidRole(t *testing.T) {
	if code := Main([]string{"serve", "--config", "x.yaml", "--role", "bogus"}); code != 2 {
		t.Fatalf("serve invalid role = %d, want 2", code)
	}
}

func TestServeFailsOnMissingConfigFile(t *testing.T) {
	if code := Main([]string{"serve", "--config", "/nonexistent/cerbix.yaml"}); code != 1 {
		t.Fatalf("serve missing config = %d, want 1", code)
	}
}

func TestServeRejectsInvalidConfig(t *testing.T) {
	cfg := writeConfig(t, "log:\n  level: nonsense\n")
	if code := Main([]string{"serve", "--config", cfg}); code != 1 {
		t.Fatalf("serve invalid config = %d, want 1", code)
	}
}

func TestMigrateRequiresConfig(t *testing.T) {
	if code := Main([]string{"migrate"}); code != 2 {
		t.Fatalf("migrate without --config = %d, want 2", code)
	}
}

func TestMigrateFailsOnMissingConfigFile(t *testing.T) {
	if code := Main([]string{"migrate", "--config", "/nonexistent/cerbix.yaml"}); code != 1 {
		t.Fatalf("migrate missing config = %d, want 1", code)
	}
}

func TestMigrateRequiresDatabaseDSN(t *testing.T) {
	cfg := writeConfig(t, "log:\n  level: info\n") // no database.dsn
	if code := Main([]string{"migrate", "--config", cfg}); code != 1 {
		t.Fatalf("migrate without dsn = %d, want 1", code)
	}
}

func TestMigrateFailsFastOnUnreachableDB(t *testing.T) {
	cfg := writeConfig(t, "database:\n  dsn: \"postgres://x:y@127.0.0.1:1/none?sslmode=disable\"\n")
	if code := Main([]string{"migrate", "--config", cfg}); code != 1 {
		t.Fatalf("migrate unreachable db = %d, want 1", code)
	}
}

func TestHelpAndUnknownCommand(t *testing.T) {
	if code := Main([]string{"help"}); code != 0 {
		t.Fatalf("help = %d, want 0", code)
	}
	if code := Main([]string{"bogus"}); code != 2 {
		t.Fatalf("unknown command = %d, want 2", code)
	}
}

// TestServicesForRoleControlPlaneIsolation pins the runtime wiring boundary from
// func-secret-inventory §4.1/D-0155. A worker has a DB connection for the existing
// composite-prober surface, but it must remain an executor: no settings/mailer,
// generic outbox claimant, user API, or future authoritative materializer. Thus a
// pending generic outbox row remains claimable by an api/scheduler/all replica in
// the master-key trust domain instead of being consumed by a master-less worker.
func TestServicesForRoleControlPlaneIsolation(t *testing.T) {
	cases := []struct {
		role string
		want roleServices
	}{
		{"all", roleServices{api: true, coreDelivery: true, materializing: true}},
		{"api", roleServices{api: true, coreDelivery: true, materializing: true}},
		{"scheduler", roleServices{coreDelivery: true, materializing: true}},
		{"worker", roleServices{}},
		{"agent", roleServices{}},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			if got := servicesForRole(tc.role); got != tc.want {
				t.Fatalf("servicesForRole(%q) = %+v, want %+v", tc.role, got, tc.want)
			}
		})
	}
}

func TestCleanupServeResourcesCancelsAndDrainsBeforeClosingInfrastructure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var bg sync.WaitGroup
	drained := make(chan struct{})
	bg.Add(1)
	go func() {
		defer bg.Done()
		<-ctx.Done()
		close(drained)
	}()

	dispatcherClosed := false
	storeClosed := false
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cleanupServeResources(cancel, &bg, func() error {
		select {
		case <-drained:
		default:
			return fmt.Errorf("dispatcher closed before background drain")
		}
		dispatcherClosed = true
		return nil
	}, func() {
		if !dispatcherClosed {
			t.Error("store closed before dispatcher")
		}
		storeClosed = true
	}, time.Second, logger)

	if !dispatcherClosed || !storeClosed {
		t.Fatalf("cleanup incomplete: dispatcher=%t store=%t", dispatcherClosed, storeClosed)
	}
}

func TestCleanupServeResourcesBoundsNonCooperativeDrain(t *testing.T) {
	var bg sync.WaitGroup
	release := make(chan struct{})
	bg.Add(1)
	go func() {
		defer bg.Done()
		<-release
	}()
	var logOutput strings.Builder
	logger := slog.New(slog.NewTextHandler(&logOutput, nil))
	var closeOrder []string
	drainTimeout := 10 * time.Millisecond
	started := time.Now()
	cleanupServeResources(func() {}, &bg, func() error {
		closeOrder = append(closeOrder, "dispatcher")
		return nil
	}, func() {
		closeOrder = append(closeOrder, "store")
	}, drainTimeout, logger)
	if got := strings.Join(closeOrder, ","); got != "dispatcher,store" {
		t.Fatalf("timeout close order = %q, want dispatcher,store", got)
	}
	elapsed := time.Since(started)
	if elapsed < drainTimeout {
		t.Fatalf("cleanup skipped the configured drain wait: elapsed=%s timeout=%s", elapsed, drainTimeout)
	}
	if elapsed > time.Second {
		t.Fatalf("bounded drain took %s", elapsed)
	}
	if !strings.Contains(logOutput.String(), "background_drain_timeout") {
		t.Fatalf("timeout warning missing from log: %q", logOutput.String())
	}
	close(release)
	bg.Wait()
}
