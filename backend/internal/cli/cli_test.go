package cli

import (
	"os"
	"path/filepath"
	"testing"
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
