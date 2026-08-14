package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/config"
	"gopkg.in/yaml.v3"
)

func TestComposeFileProviderMountMatchesOwningRoles(t *testing.T) {
	devPath := filepath.Join("..", "..", "docker", "config.dev.yaml")
	dev, err := config.Load(devPath)
	if err != nil {
		t.Fatalf("load config.dev.yaml: %v", err)
	}
	if len(dev.Providers.File) != 1 {
		t.Fatalf("config.dev.yaml file providers = %d, want the one shipped provider", len(dev.Providers.File))
	}
	var providerRoot string
	for _, provider := range dev.Providers.File {
		providerRoot = provider.Directory
	}
	expectedMount := "./monitoring.d:" + providerRoot + ":ro"

	body, err := os.ReadFile(filepath.Join("..", "..", "docker", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	var compose struct {
		Services map[string]struct {
			Volumes []string `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(body, &compose); err != nil {
		t.Fatalf("parse docker-compose.yml: %v", err)
	}

	for _, service := range []string{"cerbix", "api"} {
		mounted := false
		for _, volume := range compose.Services[service].Volumes {
			if volume == expectedMount {
				mounted = true
				break
			}
		}
		if !mounted {
			t.Errorf("service %q does not have exact configured provider mount %q", service, expectedMount)
		}
	}
	for _, service := range []string{"scheduler", "worker"} {
		for _, volume := range compose.Services[service].Volumes {
			if strings.Contains(volume, ":"+providerRoot+":") {
				t.Errorf("non-owner service %q unexpectedly mounts provider root via %q", service, volume)
			}
		}
	}
}

func TestGeoComposeKeepsRemoteRolesOnIntendedNetworks(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "docker", "docker-compose.geo.yml"))
	if err != nil {
		t.Fatalf("read docker-compose.geo.yml: %v", err)
	}
	var compose struct {
		Services map[string]struct {
			Networks map[string]any `yaml:"networks"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(body, &compose); err != nil {
		t.Fatalf("parse docker-compose.geo.yml: %v", err)
	}

	want := map[string][]string{
		"worker-geo1": {"geo1"},
		"worker-geo2": {"geo2"},
		"worker-core": {"central"},
		"api":         {"central", "geo2"},
		"rabbitmq":    {"central", "geo1"},
	}
	for service, networks := range want {
		got := compose.Services[service].Networks
		if len(got) != len(networks) {
			t.Errorf("%s networks = %v, want exactly %v", service, got, networks)
			continue
		}
		for _, network := range networks {
			if _, ok := got[network]; !ok {
				t.Errorf("%s is missing network %q (got %v)", service, network, got)
			}
		}
	}
}

func TestDevRoleConfigsKeepAtRestMasterOutOfWorker(t *testing.T) {
	devPath := filepath.Join("..", "..", "docker", "config.dev.yaml")
	dev, err := config.Load(devPath)
	if err != nil {
		t.Fatalf("load config.dev.yaml: %v", err)
	}
	if dev.Security.EncryptionKey == "" {
		t.Fatal("persistent dev stack needs its public development-only key to read encrypted E2E rows")
	}
	for _, role := range []string{"all", "api", "scheduler"} {
		if err := dev.ValidateSecretsForRole(role, "core"); err != nil {
			t.Errorf("config.dev.yaml invalid for %s: %v", role, err)
		}
	}

	workerPath := filepath.Join("..", "..", "docker", "config.worker-core.yaml")
	worker, err := config.Load(workerPath)
	if err != nil {
		t.Fatalf("load config.worker-core.yaml: %v", err)
	}
	if worker.Security.EncryptionKey != "" || len(worker.Security.PreviousKeys) != 0 {
		t.Fatal("worker config must not carry the at-rest master keyring")
	}
	if err := worker.ValidateSecretsForRole("worker", "core"); err != nil {
		t.Fatalf("config.worker-core.yaml invalid for worker: %v", err)
	}
}

func TestComposeRequiresPinnedRabbitMQImage(t *testing.T) {
	for _, name := range []string{"docker-compose.yml", "docker-compose.geo.yml", "docker-compose.prod.yml"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "docker", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var compose struct {
			Services map[string]struct {
				Image string `yaml:"image"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(body, &compose); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		image := compose.Services["rabbitmq"].Image
		if !strings.HasPrefix(image, "${CERBIX_RABBITMQ_IMAGE:?") {
			t.Errorf("%s RabbitMQ image %q is not an explicit required selector", name, image)
		}
		if strings.Contains(image, ":-") {
			t.Errorf("%s RabbitMQ image %q silently falls back across persisted-volume versions", name, image)
		}
	}

	for _, name := range []string{".env.dev.example", ".env.geo.example", ".env.prod.example"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "docker", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(body), "CERBIX_RABBITMQ_IMAGE=rabbitmq:4.3-management") {
			t.Errorf("%s does not pin the fresh-install RabbitMQ 4.3 image", name)
		}
	}
}

func TestMakefileNonProductionStackFacade(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makefile := string(body)
	for _, fixed := range []string{
		"override COMPOSE := docker compose",
		"override DEV_ENV_FILE := docker/.env.dev",
		"override GEO_ENV_FILE := docker/.env.geo",
		"override DEV_COMPOSE_FILE := docker/docker-compose.yml",
		"override GEO_COMPOSE_FILE := docker/docker-compose.geo.yml",
		"override DEV_DC := env -u CERBIX_RABBITMQ_IMAGE $(COMPOSE) --project-name cerbix",
		"override GEO_DC := env -u CERBIX_RABBITMQ_IMAGE $(COMPOSE) --project-name cerbix-geo",
	} {
		if !strings.Contains(makefile, fixed) {
			t.Errorf("Makefile is missing fixed dev-only path %q", fixed)
		}
	}
	for target, calls := range map[string][]string{
		"dev-up-single":         {"$(call assert_no_geo_stack)", "$(call assert_no_distributed_roles)"},
		"dev-up-distributed":    {"$(call assert_no_geo_stack)", "$(call assert_no_single_role)"},
		"geo-up":                {"$(call assert_no_base_stack)"},
		"dev-ready-single":      {"$(call assert_no_geo_stack)", "$(call assert_no_distributed_roles)"},
		"dev-ready-distributed": {"$(call assert_no_geo_stack)", "$(call assert_no_single_role)"},
		"geo-ready":             {"$(call assert_no_base_stack)"},
	} {
		rule := makeRule(t, makefile, target)
		for _, call := range calls {
			if !strings.Contains(rule, call) {
				t.Errorf("%s does not enforce topology guard %q", target, call)
			}
		}
	}
	if strings.Contains(makefile, "docker-compose.prod") {
		t.Fatal("dev Make facade must not reference the production Compose manifest")
	}
	for _, project := range []string{
		"label=com.docker.compose.project=cerbix",
		"label=com.docker.compose.project=cerbix-geo",
	} {
		if !strings.Contains(makefile, project) {
			t.Errorf("Makefile conflict guards do not query running project label %q", project)
		}
	}

	for _, target := range []string{
		"dev-init", "geo-init", "dev-compose-check", "geo-compose-check",
		"dev-build", "dev-build-single", "dev-build-distributed", "geo-build",
		"dev-up", "dev-up-single", "dev-up-distributed", "geo-up", "geo-up-all",
		"dev-ready-single", "dev-ready-distributed", "geo-ready", "geo-ready-all",
		"dev-test-single", "dev-test-distributed", "geo-test",
		"dev-down", "geo-down",
	} {
		if !strings.Contains(makefile, "\n"+target+":") {
			t.Errorf("Makefile is missing target %q", target)
		}
	}

	distributed := makeRule(t, makefile, "dev-up-distributed")
	assertOrdered(t, distributed,
		"dev-build",
		"up -d --wait --wait-timeout 180 postgres rabbitmq",
		"run --rm --no-deps api migrate",
		"up -d --no-deps scheduler api worker",
		"dev-ready-distributed",
	)
	geo := makeRule(t, makefile, "geo-up")
	assertOrdered(t, geo,
		"geo-build",
		"up -d --wait --wait-timeout 180 postgres rabbitmq",
		"run --rm --no-deps api migrate",
		"up -d --no-deps scheduler api worker-core",
		"geo-ready",
	)

	for _, target := range []string{"dev-down", "geo-down"} {
		rule := makeRule(t, makefile, target)
		for _, destructive := range []string{" -v", "volume rm", "volume prune", "down --volumes", "--remove-orphans"} {
			if strings.Contains(rule, destructive) {
				t.Errorf("%s contains destructive volume operation %q", target, destructive)
			}
		}
	}

	for target, wantURL := range map[string]string{
		"dev-test-single":      "CERBIX_URL=http://localhost:8080",
		"dev-test-distributed": "CERBIX_URL=http://localhost:8082",
		"geo-test":             "CERBIX_URL=http://localhost:8082",
	} {
		if rule := makeRule(t, makefile, target); !strings.Contains(rule, wantURL) {
			t.Errorf("%s does not pin its local E2E URL to %q", target, wantURL)
		}
		if rule := makeRule(t, makefile, target); !strings.Contains(rule, "CERBIX_TOPOLOGY=") {
			t.Errorf("%s does not select explicit E2E topology", target)
		}
	}
	if initRule := makeRule(t, makefile, "dev-init"); !strings.Contains(initRule, "cerbix_rabbit_volume") {
		t.Error("dev-init must refuse to guess an image when the retained base broker volume exists")
	}
	if initRule := makeRule(t, makefile, "geo-init"); !strings.Contains(initRule, "cerbix-geo_rabbit_volume") {
		t.Error("geo-init must refuse to guess an image when the retained geo broker volume exists")
	}
	for target, envFile := range map[string]string{
		"dev-compose-check": "$(DEV_ENV_FILE)",
		"geo-compose-check": "$(GEO_ENV_FILE)",
	} {
		rule := makeRule(t, makefile, target)
		if !strings.Contains(rule, "test -r "+envFile) || !strings.Contains(rule, "missing "+envFile) {
			t.Errorf("%s does not fail closed when %s is missing", target, envFile)
		}
	}
}

func TestMakefileDryRunTopologyExpansion(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	cases := []struct {
		target    string
		want      []string
		forbidden []string
	}{
		{
			target: "dev-up-single",
			want: []string{
				"--env-file docker/.env.dev -f docker/docker-compose.yml",
				"--profile sso --profile mail up -d --wait",
				"--profile single run --rm --no-deps cerbix migrate",
				"--profile single up -d --no-deps cerbix",
				"http://cerbix:8080/readyz",
			},
			forbidden: []string{"docker-compose.geo.yml", "up -d --no-deps scheduler api worker"},
		},
		{
			target: "dev-up-distributed",
			want: []string{
				"--env-file docker/.env.dev -f docker/docker-compose.yml",
				"--profile distributed run --rm --no-deps api migrate",
				"--profile distributed up -d --no-deps scheduler api worker",
				"http://api:8080/readyz",
				"http://scheduler:8080/readyz",
				"http://worker:8080/readyz",
			},
			forbidden: []string{"docker-compose.geo.yml", "--profile single up -d --no-deps cerbix"},
		},
		{
			target: "geo-up-all",
			want: []string{
				"--env-file docker/.env.geo -f docker/docker-compose.geo.yml",
				"run --rm --no-deps api migrate",
				"up -d --no-deps scheduler api worker-core",
				"up -d --no-deps worker-geo1 worker-geo2",
				"http://worker-geo1:8080/readyz",
				"http://worker-geo2:8080/readyz",
			},
			forbidden: []string{"docker/docker-compose.yml --profile", "--env-file docker/.env.dev"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			cmd := exec.Command(
				"make", "--no-print-directory", "-n", tc.target,
				"DEV_DC=docker compose -f docker/docker-compose.prod.yml",
				"GEO_DC=docker compose -f docker/docker-compose.prod.yml",
			)
			cmd.Dir = repoRoot
			cmd.Env = append(os.Environ(),
				"CERBIX_RABBITMQ_IMAGE=rabbitmq:3.12-management",
				"COMPOSE_PROJECT_NAME=cerbix-prod",
			)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("dry-run %s: %v\n%s", tc.target, err, output)
			}
			text := string(output)
			assertOrdered(t, text, tc.want...)
			for _, value := range tc.forbidden {
				if strings.Contains(text, value) {
					t.Errorf("dry-run %s unexpectedly contains %q", tc.target, value)
				}
			}
			if strings.Contains(text, "docker-compose.prod") {
				t.Errorf("dry-run %s references production", tc.target)
			}
			if !strings.Contains(text, "env -u CERBIX_RABBITMQ_IMAGE") {
				t.Errorf("dry-run %s does not discard a hostile shell broker-image override", tc.target)
			}
			project := "cerbix"
			if strings.HasPrefix(tc.target, "geo-") {
				project = "cerbix-geo"
			}
			if !strings.Contains(text, "--project-name "+project) {
				t.Errorf("dry-run %s does not pin Compose project %q", tc.target, project)
			}
		})
	}
}

func makeRule(t *testing.T, makefile, target string) string {
	t.Helper()
	marker := "\n" + target + ":"
	start := strings.Index(makefile, marker)
	if start < 0 {
		t.Fatalf("target %q not found", target)
	}
	rule := makefile[start+1:]
	if end := strings.Index(rule, "\n\n"); end >= 0 {
		rule = rule[:end]
	}
	return rule
}

func assertOrdered(t *testing.T, body string, parts ...string) {
	t.Helper()
	position := -1
	for _, part := range parts {
		next := strings.Index(body[position+1:], part)
		if next < 0 {
			t.Fatalf("rule does not contain %q in order:\n%s", part, body)
		}
		position += next + 1
	}
}
