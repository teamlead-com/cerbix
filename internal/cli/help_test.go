package cli

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const rootHelpGolden = `cerbix — self-hosted service reliability platform

Usage:
  cerbix <command> [flags]

Runtime:
  serve                   Run cerbix services.
  migrate                 Apply database migrations and exit.
  reencrypt               Re-encrypt stored secrets.

Reliability maintenance:
  adopt-fact-month        Adopt a retained fact partition.
  enqueue-service-repair  Queue a bounded service repair range.

CI/CD:
  gate check              Evaluate whether a release may proceed.
  change record           Record a deploy, rollback or flag change.

Other:
  version                 Print build information.

Run ` + "`cerbix <command> --help`" + ` for command-specific flags and examples.
`

func TestRootHelpIsTheGroupedCommandCatalogue(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}} {
		got := runCLIProcess(t, args, nil)
		if got.code != 0 {
			t.Errorf("%v: exit = %d, want 0", args, got.code)
		}
		if got.stdout != rootHelpGolden {
			t.Errorf("%v: stdout =\n%s\nwant:\n%s", args, got.stdout, rootHelpGolden)
		}
		if got.stderr != "" {
			t.Errorf("%v: stderr = %q, want empty", args, got.stderr)
		}
	}
}

func TestHelpAfterPositionalFailsBeforeRenderingOrRuntime(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{
		"CERBIX_URL": srv.URL, "CERBIX_TOKEN": "unused", "CERBIX_CA_FILE": "",
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"gate", "check", "extra", "--help"}, want: `gate check: unexpected argument "extra"`},
		{args: []string{"change", "record", "extra", "--help"}, want: `change record: unexpected argument "extra"`},
		{args: []string{"version", "extra", "--help"}, want: `version: unexpected argument "extra"`},
	} {
		got := runCLIProcess(t, tc.args, env)
		if got.code != 2 {
			t.Errorf("%v: exit = %d, want 2", tc.args, got.code)
		}
		if got.stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", tc.args, got.stdout)
		}
		if !strings.Contains(got.stderr, tc.want) {
			t.Errorf("%v: stderr = %q, want %q", tc.args, got.stderr, tc.want)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("help-after-positional sent %d HTTP request(s), want 0", hits.Load())
	}
}

func TestHelpTokensUsedAsStringFlagValuesAreNotHelp(t *testing.T) {
	bodies := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies <- string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, changeBodyRecorded)
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{
		"CERBIX_URL": srv.URL, "CERBIX_TOKEN": changeTestToken, "CERBIX_CA_FILE": "",
	}
	base := []string{
		"change", "record", "--project", "p", "--service", "s", "--kind", "deploy", "--phase", "started",
		"--source", "ci",
	}

	externalID := append(append([]string{}, base...), "--external-id", "--help")
	got := runCLIProcess(t, externalID, env)
	if got.code != 0 || got.stderr != "" || strings.Contains(got.stdout, "cerbix change record —") {
		t.Fatalf("external-id --help was treated as help: exit=%d stdout=%q stderr=%q", got.code, got.stdout, got.stderr)
	}
	if body := <-bodies; !strings.Contains(body, `"external_id":"--help"`) {
		t.Fatalf("external-id did not travel verbatim: %s", body)
	}

	ref := append(append([]string{}, base...), "--external-id", "1", "--ref", "-h")
	got = runCLIProcess(t, ref, env)
	if got.code != 0 || got.stderr != "" || strings.Contains(got.stdout, "cerbix change record —") {
		t.Fatalf("ref -h was treated as help: exit=%d stdout=%q stderr=%q", got.code, got.stdout, got.stderr)
	}
	if body := <-bodies; !strings.Contains(body, `"ref":"-h"`) {
		t.Fatalf("ref did not travel verbatim: %s", body)
	}

	got = runCLIProcess(t, []string{"migrate", "--config", "--help"}, nil)
	if got.code != 1 || got.stdout != "" || !strings.Contains(got.stderr, `"path":"--help"`) {
		t.Fatalf("config --help did not execute as a config value: exit=%d stdout=%q stderr=%q", got.code, got.stdout, got.stderr)
	}
}

func TestMisspelledNestedCommandsBeatHelpAndLeafFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"gate", "chek", "--help"}, want: `gate: unknown subcommand "chek"`},
		{args: []string{"gate", "chek", "--project", "p", "--service", "s"}, want: `gate: unknown subcommand "chek"`},
		{args: []string{"change", "recrd", "--help"}, want: `change: unknown subcommand "recrd"`},
		{args: []string{"change", "recrd", "--project", "p", "--service", "s"}, want: `change: unknown subcommand "recrd"`},
	} {
		got := runCLIProcess(t, tc.args, nil)
		if got.code != 2 {
			t.Errorf("%v: exit = %d, want 2", tc.args, got.code)
		}
		if got.stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", tc.args, got.stdout)
		}
		if !strings.Contains(got.stderr, tc.want) {
			t.Errorf("%v: stderr = %q, want %q", tc.args, got.stderr, tc.want)
		}
		if strings.Contains(got.stderr, "unknown flag") {
			t.Errorf("%v: nested typo was misclassified as a flag error: %q", tc.args, got.stderr)
		}
	}
}

func TestInvalidHelpPathsFailClosedWithoutRuntimeAccess(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{
		"CERBIX_URL": srv.URL, "CERBIX_TOKEN": "unused", "CERBIX_CA_FILE": "",
	}
	for _, args := range [][]string{
		{"help", "bogus"},
		{"help", "gate", "bogus"},
		{"help", "gate", "check", "bogus"},
		{"help", "change", "record", "extra"},
	} {
		got := runCLIProcess(t, args, env)
		if got.code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, got.code)
		}
		if got.stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", args, got.stdout)
		}
		if !strings.Contains(got.stderr, "help: unknown command path") {
			t.Errorf("%v: stderr = %q, want unknown help path", args, got.stderr)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid help paths sent %d HTTP request(s), want 0", hits.Load())
	}
}

type rejectingHelpWriter struct{}

func (rejectingHelpWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestHelpWriterErrorsUseOneExitContract(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"--help=true"},
		{"help"},
		{"gate", "check", "--help"},
		{"gate", "check", "--help=true"},
		{"change", "record", "-h=true"},
		{"migrate", "--config", "--", "--help"},
		{"migrate", "--config", "--", "--help=true"},
		{
			"change", "record", "--project", "p", "--service", "s", "--kind", "deploy",
			"--phase", "started", "--source", "ci", "--external-id", "--", "--help",
		},
		{
			"change", "record", "--project", "p", "--service", "s", "--kind", "deploy",
			"--phase", "started", "--source", "ci", "--external-id", "--", "-h=true",
		},
		{"help", "gate", "check"},
	} {
		var stderr bytes.Buffer
		code := mainWithWriters(args, rejectingHelpWriter{}, &stderr)
		if code != 1 {
			t.Errorf("%v: exit = %d, want 1", args, code)
		}
		if !strings.Contains(stderr.String(), "help: write failed") {
			t.Errorf("%v: stderr = %q, want help write error", args, stderr.String())
		}
	}
}

func TestCommandExamplesAreShellSafe(t *testing.T) {
	root := newRootCommand(io.Discard, io.Discard)
	for _, path := range [][]string{
		{"serve"}, {"migrate"}, {"reencrypt"}, {"adopt-fact-month"}, {"enqueue-service-repair"},
		{"gate", "check"}, {"change", "record"}, {"version"},
	} {
		cmd, remaining, err := root.Find(path)
		if err != nil || len(remaining) != 0 {
			t.Fatalf("find %v: remaining=%v err=%v", path, remaining, err)
		}
		var out bytes.Buffer
		if err := writeCommandHelp(cmd, &out); err != nil {
			t.Fatal(err)
		}
		examples := helpSection(out.String(), "Examples:", "\x00")
		if strings.ContainsAny(examples, "<>") {
			t.Errorf("%s examples contain shell redirection metacharacters: %q", cmd.CommandPath(), examples)
		}
	}
}

func TestRootNoArgsKeepsUsageExitAndUsesTheCatalogue(t *testing.T) {
	got := runCLIProcess(t, nil, nil)
	if got.code != 2 {
		t.Errorf("exit = %d, want 2", got.code)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want empty", got.stdout)
	}
	if got.stderr != rootHelpGolden {
		t.Errorf("stderr =\n%s\nwant:\n%s", got.stderr, rootHelpGolden)
	}
}

func TestRootEndOfOptionsCannotBypassUsageOrGateExecution(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{
		"CERBIX_URL": srv.URL, "CERBIX_TOKEN": "unused", "CERBIX_CA_FILE": "",
	}

	noCommand := runCLIProcess(t, []string{"--"}, env)
	if noCommand.code != 2 || noCommand.stdout != "" || noCommand.stderr != rootHelpGolden {
		t.Errorf("cerbix -- = exit %d stdout %q stderr %q", noCommand.code, noCommand.stdout, noCommand.stderr)
	}

	for _, args := range [][]string{
		{"--", "gate", "check", "--project", "p", "--service", "s"},
		{"--", "change", "record", "--project", "p"},
		{"--", "bogus"},
	} {
		got := runCLIProcess(t, args, env)
		if got.code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, got.code)
		}
		if got.stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", args, got.stdout)
		}
		if !strings.Contains(got.stderr, "unexpected argument") {
			t.Errorf("%v: stderr = %q, want unexpected argument", args, got.stderr)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("post--- paths sent %d HTTP request(s), want 0", hits.Load())
	}
}

func TestUnknownRootCommandUsesTheCatalogueWithoutSuggestions(t *testing.T) {
	got := runCLIProcess(t, []string{"bogus"}, nil)
	if got.code != 2 {
		t.Errorf("exit = %d, want 2", got.code)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want empty", got.stdout)
	}
	want := "unknown command \"bogus\"\n" + rootHelpGolden
	if got.stderr != want {
		t.Errorf("stderr =\n%s\nwant:\n%s", got.stderr, want)
	}
	if strings.Contains(got.stderr, "Did you mean") || strings.Contains(got.stderr, "Suggestions") {
		t.Errorf("stderr contains a command suggestion: %q", got.stderr)
	}
}

func TestCommandHelpSynopsisAndSectionsFollowRegisteredFlagMetadata(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		root := newRootCommand(io.Discard, io.Discard)
		cmd, _, err := root.Find([]string{"gate", "check"})
		if err != nil {
			t.Fatal(err)
		}
		timeout := cmd.Flags().Lookup("timeout")
		timeout.DefValue = "17s"
		var out bytes.Buffer
		if err := writeCommandHelp(cmd, &out); err != nil {
			t.Fatal(err)
		}
		help := out.String()
		usage := helpSection(help, "Usage:", "Required flags:")
		if !strings.Contains(usage, "[--timeout <duration>]") || strings.Contains(usage, "10s") {
			t.Fatalf("usage did not derive from timeout metadata: %q", usage)
		}
		options := helpSection(help, "Options:", "Environment:")
		if !strings.Contains(options, "(default 17s)") {
			t.Fatalf("options did not derive the changed default: %q", options)
		}
	})

	t.Run("required", func(t *testing.T) {
		root := newRootCommand(io.Discard, io.Discard)
		cmd, _, err := root.Find([]string{"gate", "check"})
		if err != nil {
			t.Fatal(err)
		}
		project := cmd.Flags().Lookup("project")
		delete(project.Annotations, helpAnnotationRequired)
		var out bytes.Buffer
		if err := writeCommandHelp(cmd, &out); err != nil {
			t.Fatal(err)
		}
		help := out.String()
		usage := helpSection(help, "Usage:", "Required flags:")
		if !strings.Contains(usage, "[--project <id>]") {
			t.Fatalf("usage did not move project to optional: %q", usage)
		}
		required := helpSection(help, "Required flags:", "Options:")
		if strings.Contains(required, "--project") {
			t.Fatalf("required section retained optionalized project: %q", required)
		}
		options := helpSection(help, "Options:", "Environment:")
		if !strings.Contains(options, "--project string") {
			t.Fatalf("options section lacks optionalized project: %q", options)
		}
	})
}

func helpSection(help, start, end string) string {
	from := strings.Index(help, start)
	if from < 0 {
		return ""
	}
	body := help[from+len(start):]
	if to := strings.Index(body, end); to >= 0 {
		body = body[:to]
	}
	return body
}

func TestRootHelpListsEveryRunnableLeafExactlyOnceInStableGroupOrder(t *testing.T) {
	got := runCLIProcess(t, []string{"--help"}, nil)
	if got.code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", got.code, got.stderr)
	}
	for _, line := range []string{
		"  serve                   Run cerbix services.\n",
		"  migrate                 Apply database migrations and exit.\n",
		"  reencrypt               Re-encrypt stored secrets.\n",
		"  adopt-fact-month        Adopt a retained fact partition.\n",
		"  enqueue-service-repair  Queue a bounded service repair range.\n",
		"  gate check              Evaluate whether a release may proceed.\n",
		"  change record           Record a deploy, rollback or flag change.\n",
		"  version                 Print build information.\n",
	} {
		if count := strings.Count(got.stdout, line); count != 1 {
			t.Errorf("line %q occurs %d times, want exactly once", strings.TrimSpace(line), count)
		}
	}
	positions := []int{
		strings.Index(got.stdout, "Runtime:\n"),
		strings.Index(got.stdout, "Reliability maintenance:\n"),
		strings.Index(got.stdout, "CI/CD:\n"),
		strings.Index(got.stdout, "Other:\n"),
	}
	for i, position := range positions {
		if position < 0 {
			t.Fatalf("group %d missing from root help", i)
		}
		if i > 0 && position <= positions[i-1] {
			t.Fatalf("group order = %v, want Runtime, Reliability maintenance, CI/CD, Other", positions)
		}
	}
	if strings.Contains(got.stdout, "\x1b[") {
		t.Fatalf("root help contains ANSI escapes: %q", got.stdout)
	}
}

func TestRootExposesNeitherCompletionNorImplicitVersionFlag(t *testing.T) {
	for _, args := range [][]string{{"completion"}, {"--version"}} {
		got := runCLIProcess(t, args, nil)
		if got.code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, got.code)
		}
		if got.stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", args, got.stdout)
		}
		if strings.Contains(got.stderr, "Did you mean") || strings.Contains(got.stderr, "Suggestions") {
			t.Errorf("%v: stderr contains a command suggestion: %q", args, got.stderr)
		}
	}
}

const versionHelpGolden = `cerbix version — Print build information.

Print version, commit, and Go toolchain information as indented JSON, then exit.

Usage:
  cerbix version

Options:
  -h, --help  help for version

Examples:
  cerbix version
`

func TestVersionHelpIsCommandSpecificAndDoesNotExecuteVersion(t *testing.T) {
	for _, args := range [][]string{{"version", "-h"}, {"version", "--help"}, {"help", "version"}} {
		got := runCLIProcess(t, args, nil)
		if got.code != 0 {
			t.Errorf("%v: exit = %d, want 0", args, got.code)
		}
		if got.stdout != versionHelpGolden {
			t.Errorf("%v: stdout =\n%s\nwant:\n%s", args, got.stdout, versionHelpGolden)
		}
		if got.stderr != "" {
			t.Errorf("%v: stderr = %q, want empty", args, got.stderr)
		}
		if strings.Contains(got.stdout, `"go_version"`) {
			t.Errorf("%v: help executed the version encoder: %q", args, got.stdout)
		}
	}
}

func TestVersionRejectsUnexpectedArgumentsBeforeWritingJSON(t *testing.T) {
	got := runCLIProcess(t, []string{"version", "extra"}, nil)
	if got.code != 2 {
		t.Errorf("exit = %d, want 2", got.code)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want empty", got.stdout)
	}
	if got.stderr != "version: unexpected argument \"extra\"\n" {
		t.Errorf("stderr = %q", got.stderr)
	}
}

func TestVersionUnknownFlagKeepsLegacyJSONContract(t *testing.T) {
	got := runCLIProcess(t, []string{"version", "--bogus"}, nil)
	if got.code != 0 {
		t.Errorf("exit = %d, want 0", got.code)
	}
	if got.stderr != "" {
		t.Errorf("stderr = %q, want empty", got.stderr)
	}
	for _, field := range []string{`"version"`, `"commit"`, `"go_version"`} {
		if !strings.Contains(got.stdout, field) {
			t.Errorf("stdout lacks %s: %q", field, got.stdout)
		}
	}
}

func TestVersionUnknownFlagCannotHideUnexpectedPositionals(t *testing.T) {
	for _, args := range [][]string{
		{"version", "--bogus", "extra"},
		{"version", "-x", "extra"},
		{"version", "--bogus=1", "extra"},
		{"version", "--bogus", "extra", "more"},
	} {
		got := runCLIProcess(t, args, nil)
		if got.code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, got.code)
		}
		if got.stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", args, got.stdout)
		}
		if !strings.Contains(got.stderr, `version: unexpected argument "extra"`) {
			t.Errorf("%v: stderr = %q", args, got.stderr)
		}

		calls := 0
		root := newRootCommandWithVersionExecutor(io.Discard, io.Discard, func(io.Writer) error {
			calls++
			return nil
		})
		if code := executeCommand(root, args, io.Discard); code != 2 {
			t.Errorf("%v injected: exit = %d, want 2", args, code)
		}
		if calls != 0 {
			t.Errorf("%v: version executor calls = %d, want 0", args, calls)
		}
	}
}

func TestVersionHelpAndPositionalErrorsNeverCallExecutor(t *testing.T) {
	for _, args := range [][]string{
		{"version", "-h"},
		{"version", "--help"},
		{"help", "version"},
		{"version", "extra"},
	} {
		calls := 0
		root := newRootCommandWithVersionExecutor(io.Discard, io.Discard, func(io.Writer) error {
			calls++
			return nil
		})
		wantCode := 0
		if args[len(args)-1] == "extra" {
			wantCode = 2
		}
		if code := executeCommand(root, args, io.Discard); code != wantCode {
			t.Errorf("%v: exit = %d, want %d", args, code, wantCode)
		}
		if calls != 0 {
			t.Errorf("%v: version executor calls = %d, want 0", args, calls)
		}
	}
}

const migrateHelpGolden = `cerbix migrate — Apply database migrations and exit.

Apply the embedded database migrations and exit without starting runtime services.

Usage:
  cerbix migrate --config <path>

Required flags:
  --config string  path to config YAML

Options:
  -h, --help  help for migrate

Exit codes:
  0  Migrations applied.
  1  Configuration, database, or migration failure.
  2  CLI usage error.

Examples:
  cerbix migrate --config /etc/cerbix/config.yaml
`

func TestMigrateHelpIsDetailedAndSideEffectFree(t *testing.T) {
	missingConfig := "/nonexistent/help-must-not-load.yaml"
	for _, args := range [][]string{
		{"migrate", "-h"},
		{"migrate", "--help"},
		{"migrate", "--config", missingConfig, "--help"},
		{"help", "migrate"},
	} {
		got := runCLIProcess(t, args, nil)
		if got.code != 0 {
			t.Errorf("%v: exit = %d, want 0", args, got.code)
		}
		if got.stdout != migrateHelpGolden {
			t.Errorf("%v: stdout =\n%s\nwant:\n%s", args, got.stdout, migrateHelpGolden)
		}
		if got.stderr != "" {
			t.Errorf("%v: stderr = %q, want empty", args, got.stderr)
		}
		if strings.Contains(got.stdout+got.stderr, "config_load_failed") {
			t.Errorf("%v: help loaded configuration", args)
		}
	}
}

func TestMigrateRejectsUnexpectedArgumentsBeforeConfigLoad(t *testing.T) {
	for _, args := range [][]string{
		{"migrate", "extra"},
		{"migrate", "--config", "/nonexistent/must-not-load.yaml", "extra"},
	} {
		got := runCLIProcess(t, args, nil)
		if got.code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, got.code)
		}
		if got.stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", args, got.stdout)
		}
		if got.stderr != "migrate: unexpected argument \"extra\"\n" {
			t.Errorf("%v: stderr = %q", args, got.stderr)
		}
		if strings.Contains(got.stderr, "config_load_failed") {
			t.Errorf("%v: argument validation loaded configuration", args)
		}
	}
}

func TestMigrateMissingConfigKeepsUsageExit(t *testing.T) {
	got := runCLIProcess(t, []string{"migrate"}, nil)
	if got.code != 2 {
		t.Errorf("exit = %d, want 2", got.code)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want empty", got.stdout)
	}
	if got.stderr != "migrate: --config is required\n" {
		t.Errorf("stderr = %q", got.stderr)
	}
}

func TestMigrateUnexpectedArgumentNeverCallsExecutor(t *testing.T) {
	calls := 0
	executor := func(migrateOptions, io.Writer, io.Writer) error {
		calls++
		return nil
	}
	cmd := newMigrateCommand(executor)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"extra"})
	err := cmd.Execute()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error = %v, want usage ExitError", err)
	}
	if calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("command wrote before centralized error handling: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

const reencryptHelpGolden = `cerbix reencrypt — Re-encrypt stored secrets.

Re-encrypt stored secrets with the current primary at-rest key. Keep the previous key configured until this command succeeds.

Usage:
  cerbix reencrypt --config <path>

Required flags:
  --config string  path to config YAML

Options:
  -h, --help  help for reencrypt

Exit codes:
  0  Stored secrets re-encrypted.
  1  Configuration, database, key, or rewrite failure.
  2  CLI usage error.

Examples:
  cerbix reencrypt --config /etc/cerbix/config.yaml
`

func TestReencryptHelpIsDetailedAndSideEffectFree(t *testing.T) {
	missingConfig := "/nonexistent/help-must-not-load.yaml"
	for _, args := range [][]string{
		{"reencrypt", "-h"},
		{"reencrypt", "--help"},
		{"reencrypt", "--config", missingConfig, "--help"},
		{"help", "reencrypt"},
	} {
		got := runCLIProcess(t, args, nil)
		if got.code != 0 {
			t.Errorf("%v: exit = %d, want 0", args, got.code)
		}
		if got.stdout != reencryptHelpGolden {
			t.Errorf("%v: stdout =\n%s\nwant:\n%s", args, got.stdout, reencryptHelpGolden)
		}
		if got.stderr != "" {
			t.Errorf("%v: stderr = %q, want empty", args, got.stderr)
		}
		if strings.Contains(got.stdout+got.stderr, "config_load_failed") {
			t.Errorf("%v: help loaded configuration", args)
		}
	}
}

func TestReencryptRejectsUnexpectedArgumentsBeforeConfigLoad(t *testing.T) {
	for _, args := range [][]string{
		{"reencrypt", "extra"},
		{"reencrypt", "--config", "/nonexistent/must-not-load.yaml", "extra"},
	} {
		got := runCLIProcess(t, args, nil)
		if got.code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, got.code)
		}
		if got.stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", args, got.stdout)
		}
		if got.stderr != "reencrypt: unexpected argument \"extra\"\n" {
			t.Errorf("%v: stderr = %q", args, got.stderr)
		}
		if strings.Contains(got.stderr, "config_load_failed") {
			t.Errorf("%v: argument validation loaded configuration", args)
		}
	}
}

func TestReencryptMissingConfigKeepsUsageExit(t *testing.T) {
	got := runCLIProcess(t, []string{"reencrypt"}, nil)
	if got.code != 2 {
		t.Errorf("exit = %d, want 2", got.code)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want empty", got.stdout)
	}
	if got.stderr != "reencrypt: --config is required\n" {
		t.Errorf("stderr = %q", got.stderr)
	}
}

func TestReencryptUnexpectedArgumentNeverCallsExecutor(t *testing.T) {
	calls := 0
	executor := func(reencryptOptions, io.Writer, io.Writer) error {
		calls++
		return nil
	}
	cmd := newReencryptCommand(executor)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"extra"})
	err := cmd.Execute()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error = %v, want usage ExitError", err)
	}
	if calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("command wrote before centralized error handling: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

const adoptFactMonthHelpGolden = `cerbix adopt-fact-month — Adopt a retained fact partition.

Adopt one YYYY-MM month of service-reliability facts from the DEFAULT partition through the copy-authoritative recovery path. There is no dry-run or confirmation; use a maintenance window because the fenced cutover holds the parent lock through commit.

Usage:
  cerbix adopt-fact-month --config <path> --month <YYYY-MM> [--timeout <duration>]

Required flags:
  --config string  path to config YAML
  --month string   month to adopt

Options:
  --timeout duration  total budget for the fenced cutover (parent lock through commit) (default 10m0s)
  -h, --help          help for adopt-fact-month

Exit codes:
  0  Month adopted or already attached.
  1  Configuration, database, or adoption failure.
  2  CLI usage error.

Examples:
  cerbix adopt-fact-month --config /etc/cerbix/config.yaml --month 2026-01
`

func TestAdoptFactMonthHelpIsDetailedAndSideEffectFree(t *testing.T) {
	missingConfig := "/nonexistent/help-must-not-load.yaml"
	for _, args := range [][]string{
		{"adopt-fact-month", "-h"},
		{"adopt-fact-month", "--help"},
		{"adopt-fact-month", "--config", missingConfig, "--month", "2026-01", "--help"},
		{"help", "adopt-fact-month"},
	} {
		got := runCLIProcess(t, args, nil)
		if got.code != 0 {
			t.Errorf("%v: exit = %d, want 0", args, got.code)
		}
		if got.stdout != adoptFactMonthHelpGolden {
			t.Errorf("%v: stdout =\n%s\nwant:\n%s", args, got.stdout, adoptFactMonthHelpGolden)
		}
		if got.stderr != "" {
			t.Errorf("%v: stderr = %q, want empty", args, got.stderr)
		}
		if strings.Contains(got.stdout+got.stderr, "config_load_failed") {
			t.Errorf("%v: help loaded configuration", args)
		}
	}
}

func TestAdoptFactMonthRejectsUnexpectedArgumentsBeforeConfigLoad(t *testing.T) {
	for _, args := range [][]string{
		{"adopt-fact-month", "extra"},
		{"adopt-fact-month", "--config", "/nonexistent/must-not-load.yaml", "--month", "2026-01", "extra"},
	} {
		got := runCLIProcess(t, args, nil)
		if got.code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, got.code)
		}
		if got.stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", args, got.stdout)
		}
		if got.stderr != "adopt-fact-month: unexpected argument \"extra\"\n" {
			t.Errorf("%v: stderr = %q", args, got.stderr)
		}
		if strings.Contains(got.stderr, "config_load_failed") {
			t.Errorf("%v: argument validation loaded configuration", args)
		}
	}
}

func TestAdoptFactMonthValidatesBeforeConfigLoad(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		contains string
	}{
		{name: "required", args: []string{"adopt-fact-month"}, contains: "--config and --month are required"},
		{name: "month", args: []string{"adopt-fact-month", "--config", "/nonexistent/must-not-load.yaml", "--month", "2026-13"}, contains: "--month must be YYYY-MM"},
		{name: "positive timeout", args: []string{"adopt-fact-month", "--config", "/nonexistent/must-not-load.yaml", "--month", "2026-01", "--timeout", "0"}, contains: "--timeout must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runCLIProcess(t, tc.args, nil)
			if got.code != 2 {
				t.Errorf("exit = %d, want 2", got.code)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
			if !strings.Contains(got.stderr, tc.contains) {
				t.Errorf("stderr = %q, want %q", got.stderr, tc.contains)
			}
			if strings.Contains(got.stderr, "config_load_failed") {
				t.Errorf("validation loaded configuration: %q", got.stderr)
			}
		})
	}
}

func TestAdoptFactMonthRejectsTimeoutOverflowBeforeConfigOrExecutor(t *testing.T) {
	maxDuration := time.Duration(1<<63 - 1).String()
	args := []string{
		"adopt-fact-month", "--config", "/nonexistent/must-not-load.yaml", "--month", "2026-01",
		"--timeout", maxDuration,
	}
	got := runCLIProcess(t, args, nil)
	if got.code != 2 {
		t.Errorf("exit = %d, want 2", got.code)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want empty", got.stdout)
	}
	if !strings.Contains(got.stderr, "--timeout is too large") || strings.Contains(got.stderr, "config_load_failed") {
		t.Errorf("stderr = %q, want overflow refusal before config", got.stderr)
	}

	calls := 0
	cmd := newAdoptFactMonthCommand(func(adoptFactMonthOptions, io.Writer, io.Writer) error {
		calls++
		return nil
	})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(args[1:])
	err := cmd.Execute()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error = %v, want usage ExitError", err)
	}
	if calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
}

func TestAdoptFactMonthUnexpectedArgumentNeverCallsExecutor(t *testing.T) {
	calls := 0
	executor := func(adoptFactMonthOptions, io.Writer, io.Writer) error {
		calls++
		return nil
	}
	cmd := newAdoptFactMonthCommand(executor)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"extra"})
	err := cmd.Execute()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error = %v, want usage ExitError", err)
	}
	if calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("command wrote before centralized error handling: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

const enqueueServiceRepairHelpGolden = `cerbix enqueue-service-repair — Queue a bounded service repair range.

Queue a durable admin repair for one service and RFC3339 range. The store normalizes and coalesces pending work; the scheduler leader recomputes it later through the normal audited repair machinery. There is no dry-run, confirmation, or immediate recompute.

Usage:
  cerbix enqueue-service-repair --config <path> --project <id> --service <id> --from <RFC3339> --to <RFC3339>

Required flags:
  --config string   path to config YAML
  --project string  project id
  --service string  service id
  --from string     range start (floored to the bucket)
  --to string       range end (ceiled to the bucket)

Options:
  -h, --help  help for enqueue-service-repair

Exit codes:
  0  Repair range durably queued.
  1  Configuration, database, or enqueue failure.
  2  CLI usage error.

Examples:
  cerbix enqueue-service-repair --config /etc/cerbix/config.yaml --project 00000000-0000-4000-8000-000000000001 --service 00000000-0000-4000-8000-000000000002 --from 2026-01-01T00:00:00Z --to 2026-01-01T01:00:00Z
`

func TestEnqueueServiceRepairHelpIsDetailedAndSideEffectFree(t *testing.T) {
	missingConfig := "/nonexistent/help-must-not-load.yaml"
	for _, args := range [][]string{
		{"enqueue-service-repair", "-h"},
		{"enqueue-service-repair", "--help"},
		{
			"enqueue-service-repair", "--config", missingConfig, "--project", "p", "--service", "s",
			"--from", "2026-01-01T00:00:00Z", "--to", "2026-01-01T01:00:00Z", "--help",
		},
		{"help", "enqueue-service-repair"},
	} {
		got := runCLIProcess(t, args, nil)
		if got.code != 0 {
			t.Errorf("%v: exit = %d, want 0", args, got.code)
		}
		if got.stdout != enqueueServiceRepairHelpGolden {
			t.Errorf("%v: stdout =\n%s\nwant:\n%s", args, got.stdout, enqueueServiceRepairHelpGolden)
		}
		if got.stderr != "" {
			t.Errorf("%v: stderr = %q, want empty", args, got.stderr)
		}
		if strings.Contains(got.stdout+got.stderr, "config_load_failed") {
			t.Errorf("%v: help loaded configuration", args)
		}
	}
}

func TestEnqueueServiceRepairRejectsUnexpectedArgumentsBeforeConfigLoad(t *testing.T) {
	valid := []string{
		"enqueue-service-repair", "--config", "/nonexistent/must-not-load.yaml", "--project", "p", "--service", "s",
		"--from", "2026-01-01T00:00:00Z", "--to", "2026-01-01T01:00:00Z", "extra",
	}
	for _, args := range [][]string{{"enqueue-service-repair", "extra"}, valid} {
		got := runCLIProcess(t, args, nil)
		if got.code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, got.code)
		}
		if got.stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", args, got.stdout)
		}
		if got.stderr != "enqueue-service-repair: unexpected argument \"extra\"\n" {
			t.Errorf("%v: stderr = %q", args, got.stderr)
		}
		if strings.Contains(got.stderr, "config_load_failed") {
			t.Errorf("%v: argument validation loaded configuration", args)
		}
	}
}

func TestEnqueueServiceRepairValidatesBeforeConfigLoad(t *testing.T) {
	base := []string{"enqueue-service-repair", "--config", "/nonexistent/must-not-load.yaml", "--project", "p", "--service", "s"}
	for _, tc := range []struct {
		name     string
		args     []string
		contains string
	}{
		{name: "required", args: []string{"enqueue-service-repair"}, contains: "--config, --project, --service, --from and --to are required"},
		{name: "from", args: append(append([]string{}, base...), "--from", "yesterday", "--to", "2026-01-01T01:00:00Z"), contains: "--from must be RFC3339"},
		{name: "to", args: append(append([]string{}, base...), "--from", "2026-01-01T00:00:00Z", "--to", "tomorrow"), contains: "--to must be RFC3339"},
		{name: "range", args: append(append([]string{}, base...), "--from", "2026-01-01T01:00:00Z", "--to", "2026-01-01T00:00:00Z"), contains: "--to must be after --from"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runCLIProcess(t, tc.args, nil)
			if got.code != 2 {
				t.Errorf("exit = %d, want 2", got.code)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
			if !strings.Contains(got.stderr, tc.contains) {
				t.Errorf("stderr = %q, want %q", got.stderr, tc.contains)
			}
			if strings.Contains(got.stderr, "config_load_failed") {
				t.Errorf("validation loaded configuration: %q", got.stderr)
			}
		})
	}
}

func TestEnqueueServiceRepairUnexpectedArgumentNeverCallsExecutor(t *testing.T) {
	calls := 0
	executor := func(enqueueServiceRepairOptions, io.Writer, io.Writer) error {
		calls++
		return nil
	}
	cmd := newEnqueueServiceRepairCommand(executor)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"extra"})
	err := cmd.Execute()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error = %v, want usage ExitError", err)
	}
	if calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("command wrote before centralized error handling: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

const serveHelpGolden = `cerbix serve — Run cerbix services.

Run cerbix in one process role. Role all runs the local in-process stack; api, scheduler, and worker are distributed roles; agent is the broker-less HTTP-pull prober. Region selects the worker or agent pool and an empty value means core.

Usage:
  cerbix serve --config <path> [--role <all|api|scheduler|worker|agent>] [--region <name>]

Required flags:
  --config string  path to config YAML

Options:
  --role string    process role: all|api|scheduler|worker|agent (default "all")
  --region string  worker or agent region; empty = core
  -h, --help       help for serve

Exit codes:
  0  Services stopped cleanly.
  1  Configuration, startup, dependency, runtime, or shutdown failure.
  2  CLI usage error.

Examples:
  cerbix serve --config /etc/cerbix/config.yaml --role all
  cerbix serve --config /etc/cerbix/worker.yaml --role worker --region eu-west
`

func TestServeHelpIsDetailedAndSideEffectFree(t *testing.T) {
	missingConfig := "/nonexistent/help-must-not-load.yaml"
	for _, args := range [][]string{
		{"serve", "-h"},
		{"serve", "--help"},
		{"serve", "--config", missingConfig, "--help"},
		{"help", "serve"},
	} {
		got := runCLIProcess(t, args, nil)
		if got.code != 0 {
			t.Errorf("%v: exit = %d, want 0", args, got.code)
		}
		if got.stdout != serveHelpGolden {
			t.Errorf("%v: stdout =\n%s\nwant:\n%s", args, got.stdout, serveHelpGolden)
		}
		if got.stderr != "" {
			t.Errorf("%v: stderr = %q, want empty", args, got.stderr)
		}
		if strings.Contains(got.stdout+got.stderr, "config_load_failed") {
			t.Errorf("%v: help loaded configuration", args)
		}
	}
}

func TestServeRejectsUnexpectedArgumentsBeforeConfigLoad(t *testing.T) {
	for _, args := range [][]string{
		{"serve", "extra"},
		{"serve", "--config", "/nonexistent/must-not-load.yaml", "extra"},
	} {
		got := runCLIProcess(t, args, nil)
		if got.code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, got.code)
		}
		if got.stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", args, got.stdout)
		}
		if got.stderr != "serve: unexpected argument \"extra\"\n" {
			t.Errorf("%v: stderr = %q", args, got.stderr)
		}
		if strings.Contains(got.stderr, "config_load_failed") {
			t.Errorf("%v: argument validation loaded configuration", args)
		}
	}
}

func TestServeValidatesRequiredConfigAndRoleBeforeConfigLoad(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		stderr string
	}{
		{name: "config", args: []string{"serve"}, stderr: "serve: --config is required\n"},
		{name: "role", args: []string{"serve", "--config", "/nonexistent/must-not-load.yaml", "--role", "bogus"}, stderr: "serve: invalid --role \"bogus\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runCLIProcess(t, tc.args, nil)
			if got.code != 2 {
				t.Errorf("exit = %d, want 2", got.code)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
			if got.stderr != tc.stderr {
				t.Errorf("stderr = %q, want %q", got.stderr, tc.stderr)
			}
			if strings.Contains(got.stderr, "config_load_failed") {
				t.Errorf("validation loaded configuration: %q", got.stderr)
			}
		})
	}
}

func TestServeArgumentsAndDefaultsReachExecutor(t *testing.T) {
	var got serveOptions
	executor := func(opts serveOptions, _ io.Writer, _ io.Writer) error {
		got = opts
		return nil
	}
	cmd := newServeCommand(executor)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"--config", "config.yaml"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := (serveOptions{ConfigPath: "config.yaml", Role: "all"})
	if got != want {
		t.Fatalf("options = %+v, want %+v", got, want)
	}
}

func TestServeUnexpectedArgumentNeverCallsExecutor(t *testing.T) {
	calls := 0
	executor := func(serveOptions, io.Writer, io.Writer) error {
		calls++
		return nil
	}
	cmd := newServeCommand(executor)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"extra"})
	err := cmd.Execute()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error = %v, want usage ExitError", err)
	}
	if calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("command wrote before centralized error handling: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

const gateGroupHelpGolden = `cerbix gate — Evaluate release reliability gates.

Evaluate and inspect release reliability gate commands.

Usage:
  cerbix gate <command> [flags]

Commands:
  check  Evaluate whether a release may proceed.

Options:
  -h, --help  help for gate

Examples:
  cerbix gate check --help
`

func TestGateGroupHelpAndNoSubcommandBehavior(t *testing.T) {
	for _, args := range [][]string{{"gate", "-h"}, {"gate", "--help"}, {"help", "gate"}} {
		got := runCLIProcess(t, args, nil)
		if got.code != 0 {
			t.Errorf("%v: exit = %d, want 0", args, got.code)
		}
		if got.stdout != gateGroupHelpGolden {
			t.Errorf("%v: stdout =\n%s\nwant:\n%s", args, got.stdout, gateGroupHelpGolden)
		}
		if got.stderr != "" {
			t.Errorf("%v: stderr = %q, want empty", args, got.stderr)
		}
	}

	got := runCLIProcess(t, []string{"gate"}, nil)
	if got.code != 2 {
		t.Errorf("gate: exit = %d, want 2", got.code)
	}
	if got.stdout != "" {
		t.Errorf("gate: stdout = %q, want empty", got.stdout)
	}
	if got.stderr != gateGroupHelpGolden {
		t.Errorf("gate: stderr =\n%s\nwant:\n%s", got.stderr, gateGroupHelpGolden)
	}
}

func TestGateCheckHelpIsDetailedSecureAndSideEffectFree(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{
		"CERBIX_URL":     srv.URL,
		"CERBIX_TOKEN":   "help-must-not-send",
		"CERBIX_CA_FILE": "/nonexistent/help-must-not-read.pem",
	}
	var canonical string
	for _, args := range [][]string{
		{"gate", "check", "-h"},
		{"gate", "check", "--help"},
		{"gate", "check", "--project", "p", "--service", "s", "--help"},
		{"help", "gate", "check"},
	} {
		got := runCLIProcess(t, args, env)
		if got.code != 0 {
			t.Errorf("%v: exit = %d, want 0", args, got.code)
		}
		if got.stderr != "" {
			t.Errorf("%v: stderr = %q, want empty", args, got.stderr)
		}
		if canonical == "" {
			canonical = got.stdout
		} else if got.stdout != canonical {
			t.Errorf("%v: help differs from canonical output", args)
		}
	}
	for _, want := range []string{
		"cerbix gate check — Evaluate whether a release may proceed.",
		"Usage:\n  cerbix gate check --project <id> --service <id> [--json] [--timeout <duration>]",
		"Required flags:\n  --project string",
		"--service string",
		"Options:",
		"--json",
		"--timeout duration",
		"(default 10s)",
		"Environment:",
		"CERBIX_URL",
		"CERBIX_TOKEN",
		"CERBIX_CA_FILE",
		"Exit codes:",
		"0  ALLOW or WARN.",
		"1  Transport, timeout, TLS, authentication, server, or malformed-response error.",
		"2  BLOCK or CLI usage error.",
		"4  NOT_CONFIGURED.",
		"Examples:",
	} {
		if !strings.Contains(canonical, want) {
			t.Errorf("help lacks %q:\n%s", want, canonical)
		}
	}
	for _, forbidden := range []string{"--token", "--url", "--insecure", "skip-verify"} {
		if strings.Contains(canonical, forbidden) {
			t.Errorf("help exposes forbidden option %q:\n%s", forbidden, canonical)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("help sent %d HTTP request(s), want 0", hits.Load())
	}
}

func TestGateUnknownSubcommandDoesNotSuggestOrRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	got := runCLIProcess(t, []string{"gate", "bogus"}, map[string]string{
		"CERBIX_URL": srv.URL, "CERBIX_TOKEN": "unused", "CERBIX_CA_FILE": "",
	})
	if got.code != 2 {
		t.Errorf("exit = %d, want 2", got.code)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want empty", got.stdout)
	}
	if !strings.Contains(got.stderr, "unknown") || !strings.Contains(got.stderr, "bogus") {
		t.Errorf("stderr = %q, want an unknown-subcommand diagnostic", got.stderr)
	}
	if !strings.Contains(got.stderr, "Usage:\n  cerbix gate <command>") || !strings.Contains(got.stderr, "Commands:\n  check") {
		t.Errorf("stderr lacks structured gate help: %q", got.stderr)
	}
	if strings.Contains(got.stderr, "Did you mean") || strings.Contains(got.stderr, "Suggestions") {
		t.Errorf("stderr contains a command suggestion: %q", got.stderr)
	}
	if hits.Load() != 0 {
		t.Fatalf("unknown subcommand sent %d HTTP request(s), want 0", hits.Load())
	}
}

func TestGateCheckUnexpectedArgumentNeverCallsExecutor(t *testing.T) {
	calls := 0
	executor := func(gateCheckOptions, io.Writer, io.Writer) error {
		calls++
		return nil
	}
	cmd := newGateCheckCommand(executor)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"extra"})
	err := cmd.Execute()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error = %v, want usage ExitError", err)
	}
	if calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
}

const changeGroupHelpGolden = `cerbix change — Record service changes.

Record and inspect CI/CD change commands.

Usage:
  cerbix change <command> [flags]

Commands:
  record  Record a deploy, rollback or flag change.

Options:
  -h, --help  help for change

Examples:
  cerbix change record --help
`

func TestChangeGroupHelpAndNoSubcommandBehavior(t *testing.T) {
	for _, args := range [][]string{{"change", "-h"}, {"change", "--help"}, {"help", "change"}} {
		got := runCLIProcess(t, args, nil)
		if got.code != 0 {
			t.Errorf("%v: exit = %d, want 0", args, got.code)
		}
		if got.stdout != changeGroupHelpGolden {
			t.Errorf("%v: stdout =\n%s\nwant:\n%s", args, got.stdout, changeGroupHelpGolden)
		}
		if got.stderr != "" {
			t.Errorf("%v: stderr = %q, want empty", args, got.stderr)
		}
	}

	got := runCLIProcess(t, []string{"change"}, nil)
	if got.code != 2 {
		t.Errorf("change: exit = %d, want 2", got.code)
	}
	if got.stdout != "" {
		t.Errorf("change: stdout = %q, want empty", got.stdout)
	}
	if got.stderr != changeGroupHelpGolden {
		t.Errorf("change: stderr =\n%s\nwant:\n%s", got.stderr, changeGroupHelpGolden)
	}
}

func TestChangeRecordHelpIsDetailedSecureAndSideEffectFree(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{
		"CERBIX_URL":     srv.URL,
		"CERBIX_TOKEN":   "help-must-not-send",
		"CERBIX_CA_FILE": "/nonexistent/help-must-not-read.pem",
	}
	var canonical string
	for _, args := range [][]string{
		{"change", "record", "-h"},
		{"change", "record", "--help"},
		{"help", "change", "record"},
	} {
		got := runCLIProcess(t, args, env)
		if got.code != 0 {
			t.Errorf("%v: exit = %d, want 0", args, got.code)
		}
		if got.stderr != "" {
			t.Errorf("%v: stderr = %q, want empty", args, got.stderr)
		}
		if canonical == "" {
			canonical = got.stdout
		} else if got.stdout != canonical {
			t.Errorf("%v: help differs from canonical output", args)
		}
	}
	for _, want := range []string{
		"cerbix change record — Record a deploy, rollback or flag change.",
		"Usage:\n  cerbix change record --project <id> --service <id>",
		"Required flags:",
		"--project string",
		"--service string",
		"--kind string",
		"--phase string",
		"--source string",
		"--external-id string",
		"Options:",
		"--ref string",
		"--url string",
		"--decision string",
		"--at string",
		"--json",
		"--timeout duration",
		"(default 10s)",
		"Environment:",
		"CERBIX_URL",
		"CERBIX_TOKEN",
		"CERBIX_CA_FILE",
		"Exit codes:",
		"0  Recorded or replayed.",
		"1  Transport, timeout, TLS, authentication, 429, server, or malformed-response error.",
		"2  Contract refusal (400, 404, or 409) or CLI usage error.",
		"Examples:",
	} {
		if !strings.Contains(canonical, want) {
			t.Errorf("help lacks %q:\n%s", want, canonical)
		}
	}
	for _, forbidden := range []string{"--token", "--insecure", "skip-verify"} {
		if strings.Contains(canonical, forbidden) {
			t.Errorf("help exposes forbidden option %q:\n%s", forbidden, canonical)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("help sent %d HTTP request(s), want 0", hits.Load())
	}
}

func TestChangeUnknownSubcommandDoesNotSuggestOrRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	got := runCLIProcess(t, []string{"change", "bogus"}, map[string]string{
		"CERBIX_URL": srv.URL, "CERBIX_TOKEN": "unused", "CERBIX_CA_FILE": "",
	})
	if got.code != 2 {
		t.Errorf("exit = %d, want 2", got.code)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want empty", got.stdout)
	}
	if !strings.Contains(got.stderr, "unknown") || !strings.Contains(got.stderr, "bogus") {
		t.Errorf("stderr = %q, want an unknown-subcommand diagnostic", got.stderr)
	}
	if !strings.Contains(got.stderr, "Usage:\n  cerbix change <command>") || !strings.Contains(got.stderr, "Commands:\n  record") {
		t.Errorf("stderr lacks structured change help: %q", got.stderr)
	}
	if strings.Contains(got.stderr, "Did you mean") || strings.Contains(got.stderr, "Suggestions") {
		t.Errorf("stderr contains a command suggestion: %q", got.stderr)
	}
	if hits.Load() != 0 {
		t.Fatalf("unknown subcommand sent %d HTTP request(s), want 0", hits.Load())
	}
}

func TestChangeRecordUnexpectedArgumentNeverCallsExecutor(t *testing.T) {
	calls := 0
	executor := func(changeRecordOptions, io.Writer, io.Writer) error {
		calls++
		return nil
	}
	cmd := newChangeRecordCommand(executor)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"extra"})
	err := cmd.Execute()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error = %v, want usage ExitError", err)
	}
	if calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
}
