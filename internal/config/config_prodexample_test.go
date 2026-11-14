package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docker/config.prod.yaml loads with exactly the variables docker-compose.prod.yml passes into the
// cerbix container. The loader expands ${NAME} in the WHOLE file, comments included, and fails on
// an undefined one — so a variable named only in prose (the header once said "expands ${VAR}")
// stopped the production example from starting at all.
func TestTheProductionExampleLoadsWithTheComposeEnvironment(t *testing.T) {
	compose, err := os.ReadFile(filepath.Join("..", "..", "docker", "docker-compose.prod.yml"))
	if err != nil {
		t.Fatal(err)
	}
	// The cerbix service's `environment:` block: the keys between it and `depends_on:`.
	text := string(compose)
	svc := strings.Index(text, "\n  cerbix:\n")
	if svc < 0 {
		t.Fatal("no cerbix service in docker-compose.prod.yml")
	}
	text = text[svc:]
	start := strings.Index(text, "\n    environment:\n")
	end := strings.Index(text, "\n    depends_on:")
	if start < 0 || end < start {
		t.Fatal("the cerbix service's environment block was not found in docker-compose.prod.yml")
	}
	block := text[start:end]
	keys := regexp.MustCompile(`(?m)^      ([A-Z_][A-Z0-9_]*):`).FindAllStringSubmatch(block, -1)
	if len(keys) < 8 {
		t.Fatalf("parsed %d environment keys of the cerbix service, want its 8", len(keys))
	}
	for _, k := range keys {
		v := "example-value-" + strings.ToLower(k[1])
		if k[1] == "CERBIX_ENCRYPTION_KEY" {
			v = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes, base64
		}
		t.Setenv(k[1], v)
	}
	cfg, err := Load(filepath.Join("..", "..", "docker", "config.prod.yaml"))
	if err != nil {
		t.Fatalf("docker/config.prod.yaml with the compose environment (%d variables): %v", len(keys), err)
	}
	if cfg.Server.OpsListen != ":9091" || cfg.Server.Listen != ":8080" {
		t.Errorf("listen %q, ops_listen %q — the compose file publishes 8080 and 9091", cfg.Server.Listen, cfg.Server.OpsListen)
	}
}
