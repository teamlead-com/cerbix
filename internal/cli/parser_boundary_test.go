package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

func runInstrumentedRoot(t *testing.T, args []string) (cliProcessResult, int32) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root := newRootCommand(&stdout, &stderr)
	var calls atomic.Int32
	instrumentRunnableCommands(root, &calls)
	code := executeCommand(root, args, &stderr)
	return cliProcessResult{code: code, stdout: stdout.String(), stderr: stderr.String()}, calls.Load()
}

func instrumentRunnableCommands(parent *cobra.Command, calls *atomic.Int32) {
	for _, child := range parent.Commands() {
		if child.Name() == "help" {
			continue
		}
		if child.RunE != nil {
			child.RunE = func(*cobra.Command, []string) error {
				calls.Add(1)
				return nil
			}
		} else if child.Run != nil {
			child.Run = func(*cobra.Command, []string) {
				calls.Add(1)
			}
		}
		instrumentRunnableCommands(child, calls)
	}
}

func TestCommandPositionIsCanonicalBeforeCobraTraversal(t *testing.T) {
	validChange := []string{
		"change", "record", "--project", "p", "--service", "s", "--kind", "deploy",
		"--phase", "started", "--source", "ci", "--external-id", "1",
	}
	for _, tc := range []struct {
		name       string
		args       []string
		wantCode   int
		wantCalls  int32
		wantStderr string
	}{
		{
			name:       "root flag before migrate",
			args:       []string{"--config", "/nope", "migrate"},
			wantCode:   2,
			wantStderr: `unknown command "--config"`,
		},
		{
			name:       "root flag before serve",
			args:       []string{"--role", "api", "serve"},
			wantCode:   2,
			wantStderr: `unknown command "--role"`,
		},
		{
			name:       "gate leaf flag before command",
			args:       []string{"gate", "--project", "p", "check", "--service", "s"},
			wantCode:   2,
			wantStderr: `gate: unknown subcommand "--project"`,
		},
		{
			name: "change leaf flag before command",
			args: []string{
				"change", "--project", "p", "record", "--service", "s", "--kind", "deploy",
				"--phase", "started", "--source", "ci", "--external-id", "1",
			},
			wantCode:   2,
			wantStderr: `change: unknown subcommand "--project"`,
		},
		{
			name:       "root false help before command",
			args:       []string{"--help=false", "migrate"},
			wantCode:   2,
			wantStderr: `unknown command "--help=false"`,
		},
		{
			name:       "group false help before command",
			args:       []string{"gate", "--help=false", "check", "--project", "p", "--service", "s"},
			wantCode:   2,
			wantStderr: `gate: unknown subcommand "--help=false"`,
		},
		{
			name:      "canonical top-level command",
			args:      []string{"migrate", "--config", "/nope"},
			wantCode:  0,
			wantCalls: 1,
		},
		{
			name:      "canonical nested command",
			args:      []string{"gate", "check", "--project", "p", "--service", "s"},
			wantCode:  0,
			wantCalls: 1,
		},
		{
			name:       "direct unknown command",
			args:       []string{"bogus"},
			wantCode:   2,
			wantStderr: `unknown command "bogus"`,
		},
		{
			name:       "post dash command-looking tokens",
			args:       append([]string{"--"}, validChange...),
			wantCode:   2,
			wantStderr: `cerbix: unexpected argument "change"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, calls := runInstrumentedRoot(t, tc.args)
			if got.code != tc.wantCode {
				t.Errorf("exit = %d, want %d; stdout=%q stderr=%q", got.code, tc.wantCode, got.stdout, got.stderr)
			}
			if calls != tc.wantCalls {
				t.Errorf("runnable command calls = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantStderr != "" && !strings.Contains(got.stderr, tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", got.stderr, tc.wantStderr)
			}
			if strings.Contains(got.stderr, "Did you mean") || strings.Contains(got.stderr, "Suggestions") {
				t.Errorf("stderr contains a suggestion: %q", got.stderr)
			}
		})
	}
}

func TestHelpBooleanFormsAreParsedWithoutExecutingCommands(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{name: "long true implicit", args: []string{"migrate", "--help"}, wantCode: 0},
		{name: "long true assigned", args: []string{"migrate", "--help=true"}, wantCode: 0},
		{name: "short true implicit", args: []string{"migrate", "-h"}, wantCode: 0},
		{name: "short true assigned", args: []string{"migrate", "-h=true"}, wantCode: 0},
		{name: "long false", args: []string{"migrate", "--config", "/nope", "--help=false"}, wantCode: 2, wantStderr: "migrate: explicitly false help is not allowed"},
		{name: "short false", args: []string{"migrate", "--config", "/nope", "-h=false"}, wantCode: 2, wantStderr: "migrate: explicitly false help is not allowed"},
		{name: "invalid boolean", args: []string{"migrate", "--config", "/nope", "--help=invalid"}, wantCode: 2, wantStderr: `migrate: invalid help assignment "--help=invalid"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, calls := runInstrumentedRoot(t, tc.args)
			if got.code != tc.wantCode {
				t.Errorf("exit = %d, want %d; stdout=%q stderr=%q", got.code, tc.wantCode, got.stdout, got.stderr)
			}
			if calls != 0 {
				t.Errorf("runnable command calls = %d, want 0", calls)
			}
			if tc.wantCode == 0 {
				if got.stdout == "" || got.stderr != "" {
					t.Errorf("help output = stdout %q stderr %q", got.stdout, got.stderr)
				}
			} else {
				if got.stdout != "" {
					t.Errorf("stdout = %q, want empty", got.stdout)
				}
				if !strings.Contains(got.stderr, tc.wantStderr) {
					t.Errorf("stderr = %q, want it to contain %q", got.stderr, tc.wantStderr)
				}
			}
		})
	}
}

func TestExplicitFalseHelpFailsClosedOnEverySideEffectPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		path string
	}{
		{name: "root", args: []string{"--help=false"}, path: "cerbix"},
		{name: "serve", args: []string{"serve", "--config", "/nope", "--help=false"}, path: "serve"},
		{name: "migrate", args: []string{"migrate", "--config", "/nope", "-h=false"}, path: "migrate"},
		{name: "reencrypt", args: []string{"reencrypt", "--config", "/nope", "--help=false"}, path: "reencrypt"},
		{name: "adopt", args: []string{"adopt-fact-month", "--config", "/nope", "--month", "2026-01", "--help=false"}, path: "adopt-fact-month"},
		{name: "repair", args: []string{"enqueue-service-repair", "--config", "/nope", "--project", "p", "--service", "s", "--from", "2026-01-01T00:00:00Z", "--to", "2026-01-01T01:00:00Z", "-h=false"}, path: "enqueue-service-repair"},
		{name: "gate group", args: []string{"gate", "--help=false"}, path: "gate"},
		{name: "gate leaf", args: []string{"gate", "check", "--project", "p", "--service", "s", "-h=false"}, path: "gate check"},
		{name: "change group", args: []string{"change", "-h=false"}, path: "change"},
		{name: "change leaf", args: []string{"change", "record", "--project", "p", "--service", "s", "--kind", "deploy", "--phase", "started", "--source", "ci", "--external-id", "1", "--help=false"}, path: "change record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, calls := runInstrumentedRoot(t, tc.args)
			if got.code != 2 {
				t.Errorf("exit = %d, want 2; stdout=%q stderr=%q", got.code, got.stdout, got.stderr)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
			if !strings.Contains(got.stderr, tc.path+": explicitly false help is not allowed") {
				t.Errorf("stderr = %q, want deterministic false-help diagnostic for %s", got.stderr, tc.path)
			}
			if calls != 0 {
				t.Errorf("runnable command calls = %d, want 0", calls)
			}
		})
	}
}

func TestHelpFlagTokensUsedAsStringValuesRemainValues(t *testing.T) {
	for _, value := range []string{"--help", "--help=false", "-h", "--"} {
		t.Run("migrate config "+value, func(t *testing.T) {
			var gotConfig string
			calls := 0
			cmd := newMigrateCommand(func(opts migrateOptions, _, _ io.Writer) error {
				calls++
				gotConfig = opts.ConfigPath
				return nil
			})
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			if code := executeCommand(cmd, []string{"--config", value}, &stderr); code != 0 {
				t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if calls != 1 || gotConfig != value {
				t.Fatalf("executor calls=%d config=%q, want one call with %q", calls, gotConfig, value)
			}
		})
	}

	base := []string{
		"record", "--project", "p", "--service", "s", "--kind", "deploy", "--phase", "started",
		"--source", "ci", "--external-id",
	}
	for _, value := range []string{"--help", "--help=false", "-h", "--", "__complete", "__completeNoDesc"} {
		t.Run("change external-id "+value, func(t *testing.T) {
			var gotExternalID string
			calls := 0
			cmd := newChangeCommand(func(opts changeRecordOptions, _, _ io.Writer) error {
				calls++
				gotExternalID = opts.ExternalID
				return nil
			})
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			args := append(append([]string{}, base...), value)
			if code := executeCommand(cmd, args, &stderr); code != 0 {
				t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if calls != 1 || gotExternalID != value {
				t.Fatalf("executor calls=%d external-id=%q, want one call with %q", calls, gotExternalID, value)
			}
		})
	}

	for _, help := range []string{"--help", "--help=true", "-h", "-h=true"} {
		t.Run("string dash followed by "+help, func(t *testing.T) {
			calls := 0
			cmd := newChangeCommand(func(changeRecordOptions, io.Writer, io.Writer) error {
				calls++
				return nil
			})
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			args := append(append(append([]string{}, base...), "--"), help)
			if code := executeCommand(cmd, args, &stderr); code != 0 {
				t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if calls != 0 {
				t.Fatalf("executor calls = %d, want 0", calls)
			}
			if !strings.Contains(stdout.String(), "change record —") || stderr.Len() != 0 {
				t.Fatalf("help output = stdout %q stderr %q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestVersionFalseHelpCompatibilityAndPositionalTightening(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantCode  int
		wantCalls int
	}{
		{name: "unknown", args: []string{"version", "--bogus"}, wantCode: 0, wantCalls: 1},
		{name: "long false", args: []string{"version", "--help=false"}, wantCode: 0, wantCalls: 1},
		{name: "short false", args: []string{"version", "-h=false"}, wantCode: 0, wantCalls: 1},
		{name: "unknown and false", args: []string{"version", "--bogus", "--help=false"}, wantCode: 0, wantCalls: 1},
		{name: "unknown positional", args: []string{"version", "--bogus", "extra"}, wantCode: 2},
		{name: "long false positional", args: []string{"version", "--help=false", "extra"}, wantCode: 2},
		{name: "short false positional", args: []string{"version", "-h=false", "extra"}, wantCode: 2},
		{name: "unknown false positional", args: []string{"version", "--bogus", "--help=false", "extra"}, wantCode: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			root := newRootCommandWithVersionExecutor(&stdout, &stderr, func(w io.Writer) error {
				calls++
				_, err := io.WriteString(w, "version-json\n")
				return err
			})
			code := executeCommand(root, tc.args, &stderr)
			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d; stdout=%q stderr=%q", code, tc.wantCode, stdout.String(), stderr.String())
			}
			if calls != tc.wantCalls {
				t.Errorf("version executor calls = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantCode == 0 {
				if stdout.String() != "version-json\n" || stderr.Len() != 0 {
					t.Errorf("success output = stdout %q stderr %q", stdout.String(), stderr.String())
				}
			} else if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestVersionTrueHelpAndUnknownFlagPrecedenceIsCharacterized(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{name: "true help", args: []string{"version", "--help"}, wantCode: 0},
		{name: "assigned true help", args: []string{"version", "--help=true"}, wantCode: 0},
		{name: "unknown before true help", args: []string{"version", "--bogus", "--help"}, wantCode: 2, wantStderr: "unknown flag: --bogus"},
		{name: "unknown before assigned true help", args: []string{"version", "--bogus", "--help=true"}, wantCode: 2, wantStderr: "unknown flag: --bogus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			root := newRootCommandWithVersionExecutor(&stdout, &stderr, func(io.Writer) error {
				calls++
				return nil
			})
			code := executeCommand(root, tc.args, &stderr)
			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d; stdout=%q stderr=%q", code, tc.wantCode, stdout.String(), stderr.String())
			}
			if calls != 0 {
				t.Errorf("version executor calls = %d, want 0", calls)
			}
			if tc.wantCode == 0 {
				if !strings.Contains(stdout.String(), "cerbix version —") || stderr.Len() != 0 {
					t.Errorf("help output = stdout %q stderr %q", stdout.String(), stderr.String())
				}
			} else {
				if stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.wantStderr) {
					t.Errorf("error output = stdout %q stderr %q", stdout.String(), stderr.String())
				}
			}
		})
	}
}

func TestCompoundHelpIsIdempotent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantPrefix string
		wantExact  string
	}{
		{name: "root long", args: []string{"help", "--help"}, wantExact: rootHelpGolden},
		{name: "root assigned", args: []string{"help", "--help=true"}, wantExact: rootHelpGolden},
		{name: "root short", args: []string{"help", "-h"}, wantExact: rootHelpGolden},
		{name: "root short assigned", args: []string{"help", "-h=true"}, wantExact: rootHelpGolden},
		{name: "serve long", args: []string{"help", "serve", "--help"}, wantPrefix: "cerbix serve —"},
		{name: "serve assigned", args: []string{"help", "serve", "--help=true"}, wantPrefix: "cerbix serve —"},
		{name: "serve short", args: []string{"help", "serve", "-h"}, wantPrefix: "cerbix serve —"},
		{name: "serve short assigned", args: []string{"help", "serve", "-h=true"}, wantPrefix: "cerbix serve —"},
		{name: "nested long", args: []string{"help", "gate", "check", "--help"}, wantPrefix: "cerbix gate check —"},
		{name: "nested assigned", args: []string{"help", "gate", "check", "--help=true"}, wantPrefix: "cerbix gate check —"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runCLIProcess(t, tc.args, nil)
			if got.code != 0 {
				t.Errorf("exit = %d, want 0; stdout=%q stderr=%q", got.code, got.stdout, got.stderr)
			}
			if got.stderr != "" {
				t.Errorf("stderr = %q, want empty", got.stderr)
			}
			if tc.wantExact != "" && got.stdout != tc.wantExact {
				t.Errorf("stdout = %q, want exact root catalogue", got.stdout)
			}
			if tc.wantPrefix != "" && !strings.HasPrefix(got.stdout, tc.wantPrefix) {
				t.Errorf("stdout = %q, want prefix %q", got.stdout, tc.wantPrefix)
			}
		})
	}
}

func TestCompoundFalseHelpFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		args []string
		path string
	}{
		{args: []string{"help", "--help=false"}, path: "help"},
		{args: []string{"help", "serve", "-h=false"}, path: "help"},
		{args: []string{"help", "gate", "check", "--help=false"}, path: "help"},
	} {
		got := runCLIProcess(t, tc.args, nil)
		if got.code != 2 || got.stdout != "" || !strings.Contains(got.stderr, tc.path+": explicitly false help is not allowed") {
			t.Errorf("%v: exit=%d stdout=%q stderr=%q", tc.args, got.code, got.stdout, got.stderr)
		}
	}
}

func TestCompoundHelpWriterFailuresUseTypedRuntimeExit(t *testing.T) {
	for _, args := range [][]string{
		{"help", "--help"},
		{"help", "serve", "--help=true"},
		{"help", "gate", "check", "-h=true"},
	} {
		var stderr bytes.Buffer
		code := mainWithWriters(args, rejectingHelpWriter{}, &stderr)
		if code != 1 {
			t.Errorf("%v: exit = %d, want 1", args, code)
		}
		if !strings.Contains(stderr.String(), "help: write failed") {
			t.Errorf("%v: stderr = %q, want typed writer error", args, stderr.String())
		}
	}
}

func TestCompletionProtocolCommandsAreUnreachable(t *testing.T) {
	for _, args := range [][]string{
		{"__complete", ""},
		{"__completeNoDesc", ""},
		{"--help=false", "__complete", ""},
		{"--help=false", "__completeNoDesc", ""},
		{"gate", "--help=false", "__complete", ""},
	} {
		got, calls := runInstrumentedRoot(t, args)
		if got.code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, got.code)
		}
		if got.stdout != "" {
			t.Errorf("%v: stdout = %q, want no completion protocol bytes", args, got.stdout)
		}
		if strings.Contains(got.stderr, "Completion ended with directive:") {
			t.Errorf("%v: stderr exposed completion directive: %q", args, got.stderr)
		}
		if calls != 0 {
			t.Errorf("%v: runnable command calls = %d, want 0", args, calls)
		}
	}
}

func TestMalformedParserBoundaryInputReachesNeitherConfigNorHTTP(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", "parser-boundary-test")
	t.Setenv("CERBIX_CA_FILE", "")

	for _, args := range [][]string{
		{"--config", "/nope", "migrate"},
		{"migrate", "--help=false", "--config", "/nope"},
		{"adopt-fact-month", "--config", "/nope", "--month", "2026-01", "--help=false"},
		{"gate", "--project", "p", "check", "--service", "s"},
		{"gate", "check", "--project", "p", "--service", "s", "--help=false"},
		{"change", "--project", "p", "record", "--service", "s"},
		{"change", "record", "--project", "p", "--service", "s", "--kind", "deploy", "--phase", "started", "--source", "ci", "--external-id", "1", "-h=false"},
	} {
		var stdout, stderr bytes.Buffer
		before := hits.Load()
		code := mainWithWriters(args, &stdout, &stderr)
		if code != 2 {
			t.Errorf("%v: exit = %d, want 2; stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("%v: stdout = %q, want empty", args, stdout.String())
		}
		if strings.Contains(stderr.String(), "config_load_failed") {
			t.Errorf("%v: malformed argv reached config loader: %q", args, stderr.String())
		}
		if after := hits.Load(); after != before {
			t.Errorf("%v: HTTP hits = %d after %d, want no request", args, after, before)
		}
	}
}

func TestHelpCommandIsSearchableBeforeExecute(t *testing.T) {
	root := newRootCommand(io.Discard, io.Discard)
	cmd, remaining, err := root.Find([]string{"help", "serve"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name() != "help" || len(remaining) != 1 || remaining[0] != "serve" {
		t.Fatalf("Find(help serve) = command %q remaining %v, want help and [serve]", cmd.Name(), remaining)
	}
}

func TestCobraWindowsMousetrapIsDisabled(t *testing.T) {
	_ = newRootCommand(io.Discard, io.Discard)
	if cobra.MousetrapHelpText != "" {
		t.Fatalf("cobra.MousetrapHelpText = %q, want empty", cobra.MousetrapHelpText)
	}
}

func TestRejectingWriterStillWinsAfterStringDashValue(t *testing.T) {
	for _, args := range [][]string{
		{"migrate", "--config", "--", "--help"},
		{"migrate", "--config", "--", "--help=true"},
		{"change", "record", "--project", "p", "--service", "s", "--kind", "deploy", "--phase", "started", "--source", "ci", "--external-id", "--", "--help"},
		{"change", "record", "--project", "p", "--service", "s", "--kind", "deploy", "--phase", "started", "--source", "ci", "--external-id", "--", "--help=true"},
	} {
		var stderr bytes.Buffer
		code := mainWithWriters(args, rejectingHelpWriter{}, &stderr)
		if code != 1 {
			t.Errorf("%v: exit = %d, want 1", args, code)
		}
		if !strings.Contains(stderr.String(), "help: write failed") {
			t.Errorf("%v: stderr = %q, want writer error", args, stderr.String())
		}
	}
}

func TestAnyFalseHelpAssignmentWinsForPublicCommands(t *testing.T) {
	changeBase := []string{
		"change", "record", "--project", "p", "--service", "s", "--kind", "deploy",
		"--phase", "started", "--source", "ci", "--external-id", "1",
	}
	for _, tc := range []struct {
		name string
		args []string
		path string
	}{
		{name: "root false then true", args: []string{"--help=false", "--help"}, path: "cerbix"},
		{name: "serve false then true", args: []string{"serve", "--config", "/nope", "--help=false", "--help"}, path: "serve"},
		{name: "migrate false then true", args: []string{"migrate", "--config", "/nope", "--help=false", "--help"}, path: "migrate"},
		{name: "migrate true then false", args: []string{"migrate", "--config", "/nope", "--help", "--help=false"}, path: "migrate"},
		{name: "migrate false then assigned true", args: []string{"migrate", "--config", "/nope", "--help=false", "--help=true"}, path: "migrate"},
		{name: "migrate assigned true then false", args: []string{"migrate", "--config", "/nope", "--help=true", "--help=false"}, path: "migrate"},
		{name: "migrate short false then long true", args: []string{"migrate", "--config", "/nope", "-h=false", "--help"}, path: "migrate"},
		{name: "migrate long false then short true", args: []string{"migrate", "--config", "/nope", "--help=false", "-h=true"}, path: "migrate"},
		{name: "migrate repeated false", args: []string{"migrate", "--config", "/nope", "--help=false", "--help=false"}, path: "migrate"},
		{name: "reencrypt true then short false", args: []string{"reencrypt", "--config", "/nope", "--help", "-h=false"}, path: "reencrypt"},
		{name: "gate group false then true", args: []string{"gate", "--help=false", "--help"}, path: "gate"},
		{name: "gate group true then false", args: []string{"gate", "--help", "--help=false"}, path: "gate"},
		{name: "gate leaf false then true", args: []string{"gate", "check", "--project", "p", "--service", "s", "--help=false", "--help"}, path: "gate check"},
		{name: "change group short false then true", args: []string{"change", "-h=false", "--help"}, path: "change"},
		{name: "change leaf true then false", args: append(append([]string{}, changeBase...), "--help", "--help=false"), path: "change record"},
		{name: "help root false then true", args: []string{"help", "--help=false", "--help"}, path: "help"},
		{name: "help root true then false", args: []string{"help", "--help", "--help=false"}, path: "help"},
		{name: "help serve false then true", args: []string{"help", "serve", "--help=false", "--help"}, path: "help"},
		{name: "help nested true then false", args: []string{"help", "gate", "check", "--help", "--help=false"}, path: "help"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, calls := runInstrumentedRoot(t, tc.args)
			if got.code != 2 {
				t.Errorf("exit = %d, want 2; stdout=%q stderr=%q", got.code, got.stdout, got.stderr)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
			want := tc.path + ": explicitly false help is not allowed\n"
			if got.stderr != want {
				t.Errorf("stderr = %q, want %q", got.stderr, want)
			}
			if calls != 0 {
				t.Errorf("runnable command calls = %d, want 0", calls)
			}
		})
	}
}

func TestFalseLookingStringValuesRemainValuesBeforeRealHelp(t *testing.T) {
	changeBase := []string{
		"change", "record", "--project", "p", "--service", "s", "--kind", "deploy",
		"--phase", "started", "--source", "ci",
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "config separate long false", args: []string{"migrate", "--config", "--help=false", "--help"}, want: "cerbix migrate —"},
		{name: "config equals long false", args: []string{"migrate", "--config=--help=false", "--help"}, want: "cerbix migrate —"},
		{name: "external separate long false", args: append(append([]string{}, changeBase...), "--external-id", "--help=false", "--help"), want: "cerbix change record —"},
		{name: "external equals long false", args: append(append([]string{}, changeBase...), "--external-id=--help=false", "--help"), want: "cerbix change record —"},
		{name: "external separate short false", args: append(append([]string{}, changeBase...), "--external-id", "-h=false", "--help"), want: "cerbix change record —"},
		{name: "external equals short false", args: append(append([]string{}, changeBase...), "--external-id=-h=false", "--help"), want: "cerbix change record —"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, calls := runInstrumentedRoot(t, tc.args)
			if got.code != 0 || got.stderr != "" || !strings.HasPrefix(got.stdout, tc.want) {
				t.Fatalf("exit=%d calls=%d stdout=%q stderr=%q", got.code, calls, got.stdout, got.stderr)
			}
			if calls != 0 {
				t.Fatalf("runnable command calls = %d, want 0", calls)
			}
		})
	}
}

func TestFalseLookingStringValuesReachExecutorsWithoutRealHelp(t *testing.T) {
	for _, args := range [][]string{{"--config", "--help=false"}, {"--config=--help=false"}} {
		var got string
		calls := 0
		cmd := newMigrateCommand(func(opts migrateOptions, _, _ io.Writer) error {
			calls++
			got = opts.ConfigPath
			return nil
		})
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		if code := executeCommand(cmd, args, io.Discard); code != 0 {
			t.Fatalf("%v: exit = %d, want 0", args, code)
		}
		if calls != 1 || got != "--help=false" {
			t.Fatalf("%v: calls=%d config=%q", args, calls, got)
		}
	}
}

func TestVersionRepeatedHelpPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantCode   int
		wantCalls  int
		wantOutput string
		wantStderr string
	}{
		{name: "long false", args: []string{"version", "--help=false"}, wantCalls: 1, wantOutput: "json"},
		{name: "short false", args: []string{"version", "-h=false"}, wantCalls: 1, wantOutput: "json"},
		{name: "repeated long false", args: []string{"version", "--help=false", "--help=false"}, wantCalls: 1, wantOutput: "json"},
		{name: "long and short false", args: []string{"version", "--help=false", "-h=false"}, wantCalls: 1, wantOutput: "json"},
		{name: "false then true", args: []string{"version", "--help=false", "--help"}, wantOutput: "help"},
		{name: "true then false", args: []string{"version", "--help", "--help=false"}, wantOutput: "help"},
		{name: "false then assigned true", args: []string{"version", "--help=false", "--help=true"}, wantOutput: "help"},
		{name: "assigned true then false", args: []string{"version", "--help=true", "--help=false"}, wantOutput: "help"},
		{name: "short false then true", args: []string{"version", "-h=false", "--help"}, wantOutput: "help"},
		{name: "short true then false", args: []string{"version", "-h", "--help=false"}, wantOutput: "help"},
		{name: "unknown", args: []string{"version", "--bogus"}, wantCalls: 1, wantOutput: "json"},
		{name: "unknown and false", args: []string{"version", "--bogus", "--help=false"}, wantCalls: 1, wantOutput: "json"},
		{name: "unknown and repeated false", args: []string{"version", "--bogus", "--help=false", "--help=false"}, wantCalls: 1, wantOutput: "json"},
		{name: "unknown false then true", args: []string{"version", "--bogus", "--help=false", "--help"}, wantCode: 2, wantStderr: "unknown flag: --bogus"},
		{name: "unknown true then false", args: []string{"version", "--bogus", "--help", "--help=false"}, wantCode: 2, wantStderr: "unknown flag: --bogus"},
		{name: "false positional", args: []string{"version", "--help=false", "extra"}, wantCode: 2, wantStderr: `version: unexpected argument "extra"`},
		{name: "true false positional", args: []string{"version", "--help", "--help=false", "extra"}, wantCode: 2, wantStderr: `version: unexpected argument "extra"`},
		{name: "unknown false positional", args: []string{"version", "--bogus", "--help=false", "extra"}, wantCode: 2, wantStderr: `version: unexpected argument "extra"`},
		{name: "unknown false true positional", args: []string{"version", "--bogus", "--help=false", "--help", "extra"}, wantCode: 2, wantStderr: `version: unexpected argument "extra"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			root := newRootCommandWithVersionExecutor(&stdout, &stderr, func(w io.Writer) error {
				calls++
				_, err := io.WriteString(w, "version-json\n")
				return err
			})
			code := executeCommand(root, tc.args, &stderr)
			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d; stdout=%q stderr=%q", code, tc.wantCode, stdout.String(), stderr.String())
			}
			if calls != tc.wantCalls {
				t.Errorf("encoder calls = %d, want %d", calls, tc.wantCalls)
			}
			switch tc.wantOutput {
			case "json":
				if stdout.String() != "version-json\n" || stderr.Len() != 0 {
					t.Errorf("JSON output = stdout %q stderr %q", stdout.String(), stderr.String())
				}
			case "help":
				if !strings.HasPrefix(stdout.String(), "cerbix version —") || stderr.Len() != 0 {
					t.Errorf("help output = stdout %q stderr %q", stdout.String(), stderr.String())
				}
			default:
				if stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.wantStderr) {
					t.Errorf("error output = stdout %q stderr %q, want stderr containing %q", stdout.String(), stderr.String(), tc.wantStderr)
				}
			}
		})
	}
}

func TestVersionInvalidHelpCannotHidePositionals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		positional string
	}{
		{name: "long invalid before positional", args: []string{"version", "--help=invalid", "extra"}, positional: "extra"},
		{name: "false invalid positional", args: []string{"version", "--help=false", "--help=invalid", "extra"}, positional: "extra"},
		{name: "invalid false positional", args: []string{"version", "--help=invalid", "--help=false", "extra"}, positional: "extra"},
		{name: "short invalid positional", args: []string{"version", "-h=invalid", "extra"}, positional: "extra"},
		{name: "positional before invalid", args: []string{"version", "extra", "--help=invalid"}, positional: "extra"},
		{name: "invalid end-of-options positional", args: []string{"version", "--help=invalid", "--", "extra"}, positional: "extra"},
		{name: "invalid post-dash help-looking positional", args: []string{"version", "--help=invalid", "--", "--help=false"}, positional: "--help=false"},
		{name: "unknown invalid positional", args: []string{"version", "--bogus", "--help=invalid", "extra"}, positional: "extra"},
		{name: "unknown repeated help invalid positional", args: []string{"version", "--bogus", "--help=false", "--help", "--help=invalid", "extra"}, positional: "extra"},
		{name: "literal dash positional", args: []string{"version", "--help=invalid", "-"}, positional: "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			root := newRootCommandWithVersionExecutor(&stdout, &stderr, func(io.Writer) error {
				calls++
				return nil
			})
			code := executeCommand(root, tc.args, &stderr)
			if code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			want := fmt.Sprintf("version: unexpected argument %q\n", tc.positional)
			if stderr.String() != want {
				t.Errorf("stderr = %q, want %q", stderr.String(), want)
			}
			if calls != 0 {
				t.Errorf("version encoder calls = %d, want 0", calls)
			}
		})
	}

	t.Run("invalid controls without positional", func(t *testing.T) {
		for _, args := range [][]string{
			{"version", "--help=invalid"},
			{"version", "-h=invalid"},
			{"version", "--help=false", "--help=invalid"},
			{"version", "--help=invalid", "--help=false"},
			{"version", "--help", "--help=invalid"},
			{"version", "--help=invalid", "--help"},
		} {
			var stdout, stderr bytes.Buffer
			calls := 0
			root := newRootCommandWithVersionExecutor(&stdout, &stderr, func(io.Writer) error {
				calls++
				return nil
			})
			code := executeCommand(root, args, &stderr)
			if code != 2 || stdout.Len() != 0 || calls != 0 {
				t.Errorf("%v: exit=%d calls=%d stdout=%q stderr=%q", args, code, calls, stdout.String(), stderr.String())
			}
			invalidToken := "--help=invalid"
			for _, arg := range args {
				if arg == "-h=invalid" {
					invalidToken = arg
				}
			}
			if want := fmt.Sprintf("version: invalid help assignment %q\n", invalidToken); stderr.String() != want {
				t.Errorf("%v: stderr=%q, want %q", args, stderr.String(), want)
			}
		}
	})

	t.Run("unknown help controls without positional", func(t *testing.T) {
		for _, args := range [][]string{
			{"version", "--bogus", "--help"},
			{"version", "--bogus", "--help=false", "--help"},
		} {
			var stdout, stderr bytes.Buffer
			calls := 0
			root := newRootCommandWithVersionExecutor(&stdout, &stderr, func(io.Writer) error {
				calls++
				return nil
			})
			code := executeCommand(root, args, &stderr)
			if code != 2 || stdout.Len() != 0 || stderr.String() != "unknown flag: --bogus\n" || calls != 0 {
				t.Errorf("%v: exit=%d calls=%d stdout=%q stderr=%q", args, code, calls, stdout.String(), stderr.String())
			}
		}
	})

	t.Run("unknown false compatibility without positional", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		calls := 0
		root := newRootCommandWithVersionExecutor(&stdout, &stderr, func(w io.Writer) error {
			calls++
			_, err := io.WriteString(w, "version-json\n")
			return err
		})
		args := []string{"version", "--bogus", "--help=false"}
		code := executeCommand(root, args, &stderr)
		if code != 0 || stdout.String() != "version-json\n" || stderr.Len() != 0 || calls != 1 {
			t.Errorf("%v: exit=%d calls=%d stdout=%q stderr=%q", args, code, calls, stdout.String(), stderr.String())
		}
	})
}

func TestRepeatedFalseHelpReachesNeitherConfigNorHTTP(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", "repeated-help-test")
	t.Setenv("CERBIX_CA_FILE", "")

	for _, args := range [][]string{
		{"migrate", "--config", "/nope", "--help=false", "--help"},
		{"serve", "--config", "/nope", "-h=false", "--help"},
		{"gate", "check", "--project", "p", "--service", "s", "--help=false", "--help"},
		{"change", "record", "--project", "p", "--service", "s", "--kind", "deploy", "--phase", "started", "--source", "ci", "--external-id", "1", "--help=false", "--help"},
	} {
		var stdout, stderr bytes.Buffer
		before := hits.Load()
		code := mainWithWriters(args, &stdout, &stderr)
		if code != 2 || stdout.Len() != 0 {
			t.Errorf("%v: exit=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "explicitly false help is not allowed") {
			t.Errorf("%v: stderr=%q", args, stderr.String())
		}
		if strings.Contains(stderr.String(), "config_load_failed") {
			t.Errorf("%v reached config loader: %q", args, stderr.String())
		}
		if after := hits.Load(); after != before {
			t.Errorf("%v sent %d HTTP request(s)", args, after-before)
		}
	}
}

func TestRepeatedHelpWriterPolicy(t *testing.T) {
	for _, args := range [][]string{
		{"migrate", "--help", "--help=true"},
		{"help", "serve", "--help", "--help=true"},
	} {
		var stderr bytes.Buffer
		if code := mainWithWriters(args, rejectingHelpWriter{}, &stderr); code != 1 {
			t.Errorf("%v: exit=%d stderr=%q, want writer exit 1", args, code, stderr.String())
		}
	}
	for _, args := range [][]string{
		{"migrate", "--config", "/nope", "--help=false", "--help"},
		{"help", "serve", "--help=false", "--help"},
	} {
		var stderr bytes.Buffer
		if code := mainWithWriters(args, rejectingHelpWriter{}, &stderr); code != 2 {
			t.Errorf("%v: exit=%d stderr=%q, want false-help exit 2", args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "explicitly false help is not allowed") {
			t.Errorf("%v: stderr=%q", args, stderr.String())
		}
	}
}
