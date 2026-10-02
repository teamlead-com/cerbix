package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

type cliProcessResult struct {
	code   int
	stdout string
	stderr string
}

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("CERBIX_CLI_HELPER_PROCESS") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("CERBIX_CLI_HELPER_ARGS")), &args); err != nil {
		_, _ = os.Stderr.WriteString("decode helper args: " + err.Error() + "\n")
		os.Exit(125)
	}
	os.Exit(Main(args))
}

func runCLIProcess(t *testing.T, args []string, env map[string]string) cliProcessResult {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	cmd.Env = append(os.Environ(),
		"CERBIX_CLI_HELPER_PROCESS=1",
		"CERBIX_CLI_HELPER_ARGS="+string(encoded),
	)
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			t.Fatalf("run helper: %v", runErr)
		}
		code = exitErr.ExitCode()
	}
	return cliProcessResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestLegacyUsageErrorsStayBeforeRemoteRequests(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{
		"CERBIX_URL":     srv.URL,
		"CERBIX_TOKEN":   "characterization-token",
		"CERBIX_CA_FILE": "",
	}

	for _, tc := range []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{
			name:       "gate extra",
			args:       []string{"gate", "check", "--project", "p", "--service", "s", "extra"},
			wantStderr: "gate check: unexpected argument \"extra\"\n",
		},
		{
			name: "change extra",
			args: []string{
				"change", "record", "--project", "p", "--service", "s", "--kind", "deploy",
				"--phase", "started", "--source", "ci", "--external-id", "1", "extra",
			},
			wantStderr: "change record: unexpected argument \"extra\"\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := hits.Load()
			got := runCLIProcess(t, tc.args, env)
			if got.code != 2 {
				t.Errorf("exit = %d, want 2", got.code)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
			if got.stderr != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", got.stderr, tc.wantStderr)
			}
			if after := hits.Load(); after != before {
				t.Errorf("server hits = %d after %d; argument validation reached the network", after, before)
			}
		})
	}
}

func TestLegacyMissingAndInvalidValuesStayUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		contains string
	}{
		{name: "serve config", args: []string{"serve"}, contains: "serve: --config is required"},
		{name: "serve role", args: []string{"serve", "--config", "unused.yaml", "--role", "bogus"}, contains: "serve: invalid --role \"bogus\""},
		{name: "migrate config", args: []string{"migrate"}, contains: "migrate: --config is required"},
		{name: "reencrypt config", args: []string{"reencrypt"}, contains: "reencrypt: --config is required"},
		{name: "adopt required", args: []string{"adopt-fact-month"}, contains: "--config and --month are required"},
		{name: "adopt month", args: []string{"adopt-fact-month", "--config", "unused.yaml", "--month", "2026-13"}, contains: "--month must be YYYY-MM"},
		{name: "adopt duration", args: []string{"adopt-fact-month", "--config", "unused.yaml", "--month", "2026-01", "--timeout", "soon"}, contains: "invalid duration \"soon\""},
		{name: "repair required", args: []string{"enqueue-service-repair"}, contains: "--config, --project, --service, --from and --to are required"},
		{
			name:     "repair RFC3339",
			args:     []string{"enqueue-service-repair", "--config", "unused.yaml", "--project", "p", "--service", "s", "--from", "yesterday", "--to", "2026-01-01T01:00:00Z"},
			contains: "--from must be RFC3339",
		},
		{name: "gate required", args: []string{"gate", "check", "--project", "p"}, contains: "--project and --service are required"},
		{name: "gate duration", args: []string{"gate", "check", "--project", "p", "--service", "s", "--timeout", "soon"}, contains: "invalid duration \"soon\""},
		{name: "change required", args: []string{"change", "record", "--project", "p"}, contains: "--service, --kind, --phase, --source, --external-id are required"},
		{
			name:     "change duration",
			args:     []string{"change", "record", "--project", "p", "--service", "s", "--kind", "deploy", "--phase", "started", "--source", "ci", "--external-id", "1", "--timeout", "soon"},
			contains: "invalid duration \"soon\"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runCLIProcess(t, tc.args, map[string]string{"CERBIX_URL": "", "CERBIX_TOKEN": "", "CERBIX_CA_FILE": ""})
			if got.code != 2 {
				t.Errorf("exit = %d, want 2", got.code)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
			if !strings.Contains(got.stderr, tc.contains) {
				t.Errorf("stderr = %q, want it to contain %q", got.stderr, tc.contains)
			}
			if strings.Contains(got.stderr, "config_load_failed") || strings.Contains(got.stderr, "request failed") {
				t.Errorf("stderr = %q; usage validation reached a runtime dependency", got.stderr)
			}
		})
	}
}
