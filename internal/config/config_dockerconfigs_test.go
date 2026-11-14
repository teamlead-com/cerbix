package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every config shipped in docker/ still loads — Load runs Validate, so a stricter listener rule
// (review of iter-0212, second pass, P3-1) that refused a shipped value would stop that stack from
// starting. Each ${NAME} the file references gets a placeholder; "$${" is the escape for a literal
// '$' and references nothing.
func TestEveryDockerConfigLoads(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "docker", "config*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 7 {
		t.Fatalf("found %d docker/config*.yaml files, want at least the 7 shipped ones", len(files))
	}
	ref := regexp.MustCompile(`(^|[^$])\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range ref.FindAllStringSubmatch(string(raw), -1) {
				v := "example-value-" + strings.ToLower(m[2])
				if strings.HasSuffix(m[2], "ENCRYPTION_KEY") {
					v = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes, base64
				}
				t.Setenv(m[2], v)
			}
			if _, err := Load(f); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
		})
	}
}
