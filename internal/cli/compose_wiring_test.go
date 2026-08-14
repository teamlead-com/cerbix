package cli

import (
	"os"
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

	for _, name := range []string{".env.dev.example", ".env.prod.example"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "docker", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(body), "CERBIX_RABBITMQ_IMAGE=rabbitmq:4.3-management") {
			t.Errorf("%s does not pin the fresh-install RabbitMQ 4.3 image", name)
		}
	}
}
