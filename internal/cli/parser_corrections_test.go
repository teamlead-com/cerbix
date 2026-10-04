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
)

func TestVersionPositionalsPrecedeShorthandAndHelp(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		positional string
	}{
		{[]string{"--bogus", "-hh=false", ""}, ""},
		{[]string{"-hh=false", ""}, ""},
		{[]string{"-hh=true", ""}, ""},
		{[]string{"-hh", ""}, ""},
		{[]string{"-abc", ""}, ""},
		{[]string{"-config", ""}, ""},
		{[]string{"-hh=false", "extra"}, "extra"},
		{[]string{"--bogus", "-hh=false", "extra"}, "extra"},
		{[]string{"--help", "-hh=false", ""}, ""},
		{[]string{"-hh=false", "--help", ""}, ""},
		{[]string{"--help=false", "-hh=true", ""}, ""},
		{[]string{"-hh=true", "--help=false", ""}, ""},
		{[]string{"-hh=false", "--", ""}, ""},
		{[]string{"--", "-hh=false"}, "-hh=false"},
	} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/direct=%t", tc.args, direct), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				calls := 0
				execute := func(io.Writer) error { calls++; return nil }
				var code int
				if direct {
					cmd := newVersionCommand(execute)
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SilenceUsage, cmd.SilenceErrors = true, true
					code = executeCommand(cmd, tc.args, &stderr)
				} else {
					root := newRootCommandWithVersionExecutor(&stdout, &stderr, execute)
					code = executeCommand(root, append([]string{"version"}, tc.args...), &stderr)
				}
				want := fmt.Sprintf("version: unexpected argument %q\n", tc.positional)
				if code != 2 || stdout.Len() != 0 || stderr.String() != want || calls != 0 {
					t.Errorf("exit=%d stdout=%q stderr=%q calls=%d, want exit 2, %q and zero calls", code, stdout.String(), stderr.String(), calls, want)
				}
			})
		}
	}
}

func TestDirectVersionTreatsItsNameAsAPositional(t *testing.T) {
	for _, args := range [][]string{
		{"version"}, {"version", "--bogus"}, {"version", ""},
		{"version", "extra"}, {"version", "--help=false"}, {"version", "--", "extra"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			cmd := newVersionCommand(func(io.Writer) error { calls++; return nil })
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			code := executeCommand(cmd, args, &stderr)
			if code != 2 || stdout.Len() != 0 || stderr.String() != "version: unexpected argument \"version\"\n" || calls != 0 {
				t.Errorf("exit=%d stdout=%q stderr=%q calls=%d", code, stdout.String(), stderr.String(), calls)
			}
		})
	}
}

func TestRootVersionCommandTokenIsNotPositional(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		code  int
		calls int
		want  string
	}{
		{[]string{"version"}, 0, 1, ""},
		{[]string{"version", "--bogus"}, 0, 1, ""},
		{[]string{"version", "--help"}, 0, 0, "cerbix version —"},
		{[]string{"version", ""}, 2, 0, "version: unexpected argument \"\"\n"},
	} {
		var stdout, stderr bytes.Buffer
		calls := 0
		root := newRootCommandWithVersionExecutor(&stdout, &stderr, func(io.Writer) error { calls++; return nil })
		code := executeCommand(root, tc.args, &stderr)
		if code != tc.code || calls != tc.calls || (tc.code == 2 && stderr.String() != tc.want) || (tc.want == "cerbix version —" && !strings.HasPrefix(stdout.String(), tc.want)) {
			t.Errorf("%q: exit=%d calls=%d stdout=%q stderr=%q", tc.args, code, calls, stdout.String(), stderr.String())
		}
	}
}

func TestUnsupportedShorthandWithoutVersionPositional(t *testing.T) {
	for _, flag := range []string{"-hh=false", "-hhh=false", "-abc"} {
		for _, direct := range []bool{false, true} {
			var stdout, stderr bytes.Buffer
			calls := 0
			execute := func(io.Writer) error { calls++; return nil }
			var code int
			if direct {
				cmd := newVersionCommand(execute)
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				cmd.SilenceUsage, cmd.SilenceErrors = true, true
				code = executeCommand(cmd, []string{flag}, &stderr)
			} else {
				root := newRootCommandWithVersionExecutor(&stdout, &stderr, execute)
				code = executeCommand(root, []string{"version", flag}, &stderr)
			}
			want := fmt.Sprintf("version: unsupported shorthand flag %q\n", flag)
			if code != 2 || stdout.Len() != 0 || stderr.String() != want || calls != 0 {
				t.Errorf("%q direct=%t: exit=%d calls=%d stdout=%q stderr=%q", flag, direct, code, calls, stdout.String(), stderr.String())
			}
		}
	}
}

func TestHelpAssignmentsUseOnlyCanonicalBooleanSpellings(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", "local-review-only")
	t.Setenv("CERBIX_CA_FILE", "")

	paths := []struct {
		name string
		args []string
	}{
		{"cerbix", nil},
		{"migrate", []string{"migrate", "--config", "/nonexistent/cerbix-0197"}},
		{"gate", []string{"gate"}},
		{"gate check", []string{"gate", "check", "--project", "p", "--service", "s"}},
		{"change record", []string{"change", "record", "--project", "p", "--service", "s", "--kind", "deploy", "--phase", "started", "--source", "ci", "--external-id", "1"}},
		{"version", []string{"version"}},
		{"help", []string{"help"}},
		{"compound help", []string{"help", "gate", "check"}},
	}
	for _, path := range paths {
		for _, spelling := range []string{
			"-h=", "-h=1", "-h=0", "-h=t", "-h=f", "-h=T", "-h=F", "-h=TRUE", "-h=FALSE", "-h=True", "-h=False",
			"--help=", "--help=1", "--help=0", "--help=t", "--help=f", "--help=T", "--help=F", "--help=TRUE", "--help=FALSE", "--help=True", "--help=False",
		} {
			t.Run(path.name+"/"+spelling, func(t *testing.T) {
				args := append(append([]string{}, path.args...), spelling)
				var stdout, stderr bytes.Buffer
				versionCalls := 0
				root := newRootCommandWithVersionExecutor(&stdout, &stderr, func(io.Writer) error { versionCalls++; return nil })
				var executorCalls atomic.Int32
				if path.name != "version" {
					instrumentRunnableCommands(root, &executorCalls)
				}
				before := hits.Load()
				code := executeCommand(root, args, &stderr)
				display := path.name
				if display == "compound help" {
					display = "help"
				}
				want := fmt.Sprintf("%s: invalid help assignment %q\n", display, spelling)
				if code != 2 || stdout.Len() != 0 || stderr.String() != want || versionCalls != 0 || executorCalls.Load() != 0 || hits.Load() != before {
					t.Errorf("args=%q exit=%d stdout=%q stderr=%q version=%d executor=%d HTTP delta=%d, want exit 2 and %q", args, code, stdout.String(), stderr.String(), versionCalls, executorCalls.Load(), hits.Load()-before, want)
				}
				var realOut, realErr bytes.Buffer
				before = hits.Load()
				realCode := mainWithWriters(args, &realOut, &realErr)
				if realCode != 2 || realOut.Len() != 0 || realErr.String() != want || hits.Load() != before || strings.Contains(realErr.String(), "config_load_failed") {
					t.Errorf("real args=%q exit=%d stdout=%q stderr=%q HTTP delta=%d", args, realCode, realOut.String(), realErr.String(), hits.Load()-before)
				}
			})
		}
	}
}

func TestFalseHelpStillWinsOverInvalidForOrdinaryCommands(t *testing.T) {
	for _, suffix := range [][]string{{"-h=false", "--help=1"}, {"--help=1", "-h=false"}, {"--help=false", "-h="}} {
		args := append([]string{"migrate", "--config", "/nonexistent/cerbix-0197"}, suffix...)
		got, calls := runInstrumentedRoot(t, args)
		if got.code != 2 || got.stdout != "" || got.stderr != "migrate: explicitly false help is not allowed\n" || calls != 0 {
			t.Errorf("%q: exit=%d calls=%d stdout=%q stderr=%q", args, got.code, calls, got.stdout, got.stderr)
		}
	}
}

func TestOrdinaryPositionalsPrecedeAllHelpIntent(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", "local-review-only")
	t.Setenv("CERBIX_CA_FILE", "")

	migrate := []string{"migrate", "--config", "/nonexistent/cerbix-0197"}
	gate := []string{"gate", "check", "--project", "p", "--service", "s"}
	change := []string{"change", "record", "--project", "p", "--service", "s", "--kind", "deploy", "--phase", "started", "--source", "ci", "--external-id", "1"}
	for _, tc := range []struct {
		base, suffix []string
		want         string
	}{
		{migrate, []string{"extra", "-h=1"}, "migrate: unexpected argument \"extra\"\n"},
		{migrate, []string{"-h=1", "extra"}, "migrate: unexpected argument \"extra\"\n"},
		{migrate, []string{"extra", "--help=1"}, "migrate: unexpected argument \"extra\"\n"},
		{migrate, []string{"--help=1", "extra"}, "migrate: unexpected argument \"extra\"\n"},
		{migrate, []string{"extra", "--help"}, "migrate: unexpected argument \"extra\"\n"},
		{migrate, []string{"--help", "extra"}, "migrate: unexpected argument \"extra\"\n"},
		{migrate, []string{"extra", "--help=false"}, "migrate: unexpected argument \"extra\"\n"},
		{migrate, []string{"--help=false", "extra"}, "migrate: unexpected argument \"extra\"\n"},
		{migrate, []string{"-h=1", ""}, "migrate: unexpected argument \"\"\n"},
		{migrate, []string{"--help=false", ""}, "migrate: unexpected argument \"\"\n"},
		{migrate, []string{"--", "--help=1"}, "migrate: unexpected argument \"--help=1\"\n"},
		{gate, []string{"extra", "-h=1"}, "gate check: unexpected argument \"extra\"\n"},
		{gate, []string{"-h=1", "extra"}, "gate check: unexpected argument \"extra\"\n"},
		{gate, []string{"extra", "--help=false"}, "gate check: unexpected argument \"extra\"\n"},
		{change, []string{"extra", "--help=1"}, "change record: unexpected argument \"extra\"\n"},
		{change, []string{"--help=1", "extra"}, "change record: unexpected argument \"extra\"\n"},
		{change, []string{"--help", "extra"}, "change record: unexpected argument \"extra\"\n"},
		{[]string{"help"}, []string{"serve", "extra", "-h=1"}, "help: unknown command path \"serve extra\"\n"},
		{[]string{"help"}, []string{"gate", "bogus", "--help=1"}, "help: unknown command path \"gate bogus\"\n"},
		{[]string{"help"}, []string{"gate", "check", "extra", "-h=1"}, "help: unknown command path \"gate check extra\"\n"},
	} {
		args := append(append([]string{}, tc.base...), tc.suffix...)
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			got, calls := runInstrumentedRoot(t, args)
			if got.code != 2 || got.stdout != "" || got.stderr != tc.want || calls != 0 {
				t.Errorf("injected: exit=%d stdout=%q stderr=%q calls=%d, want %q", got.code, got.stdout, got.stderr, calls, tc.want)
			}
			var stdout, stderr bytes.Buffer
			before := hits.Load()
			code := mainWithWriters(args, &stdout, &stderr)
			if code != 2 || stdout.Len() != 0 || stderr.String() != tc.want || hits.Load() != before || strings.Contains(stderr.String(), "config_load_failed") {
				t.Errorf("real: exit=%d stdout=%q stderr=%q HTTP delta=%d, want %q", code, stdout.String(), stderr.String(), hits.Load()-before, tc.want)
			}
		})
	}
}

func TestMalformedHelpLookingStringValuesRemainLiteral(t *testing.T) {
	for _, value := range []string{"-h=1", "--help=1"} {
		for _, args := range [][]string{{"--config", value}, {"--config=" + value}} {
			var stdout, stderr bytes.Buffer
			calls := 0
			var configPath string
			cmd := newMigrateCommand(func(opts migrateOptions, _, _ io.Writer) error {
				calls++
				configPath = opts.ConfigPath
				return nil
			})
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			if code := executeCommand(cmd, args, &stderr); code != 0 || calls != 1 || configPath != value || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Errorf("config args=%q: exit=%d calls=%d config=%q stdout=%q stderr=%q", args, code, calls, configPath, stdout.String(), stderr.String())
			}
			stdout.Reset()
			stderr.Reset()
			if code := mainWithWriters(append(append([]string{"migrate"}, args...), "--help"), &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "cerbix migrate —") || stderr.Len() != 0 {
				t.Errorf("help after config value %q: exit=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
			}
		}
	}

	srv, fake := newChangeServer(t, http.StatusCreated, changeBodyRecorded, nil)
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", changeTestToken)
	t.Setenv("CERBIX_CA_FILE", "")
	base := []string{"change", "record", "--project", changeTestProject, "--service", changeTestService, "--kind", "deploy", "--phase", "started", "--source", "ci", "--external-id", "1"}
	for _, tc := range []struct {
		field, value string
		spelling     []string
	}{
		{"external_id", "--help=1", []string{"--external-id", "--help=1"}},
		{"external_id", "--help=1", []string{"--external-id=--help=1"}},
		{"ref", "--help=False", []string{"--ref", "--help=False"}},
		{"ref", "--help=False", []string{"--ref=--help=False"}},
	} {
		args := append(append([]string{}, base...), tc.spelling...)
		var stdout, stderr bytes.Buffer
		before := fake.hits.Load()
		if code := mainWithWriters(args, &stdout, &stderr); code != 0 || fake.hits.Load() != before+1 || fake.last()[tc.field] != tc.value || stderr.Len() != 0 {
			t.Errorf("change %q: exit=%d HTTP delta=%d body=%v stderr=%q", args, code, fake.hits.Load()-before, fake.last(), stderr.String())
		}
		stdout.Reset()
		stderr.Reset()
		before = fake.hits.Load()
		if code := mainWithWriters(append(args, "--help"), &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "cerbix change record —") || stderr.Len() != 0 || fake.hits.Load() != before {
			t.Errorf("help after change value %q: stdout=%q stderr=%q HTTP delta=%d", args, stdout.String(), stderr.String(), fake.hits.Load()-before)
		}
	}
}

func TestInvalidHelpLookingStringValuesStayLiteral(t *testing.T) {
	for _, spelling := range [][]string{{"--config", "-h=1"}, {"--config=-h=1"}} {
		var stdout, stderr bytes.Buffer
		calls := 0
		var value string
		cmd := newMigrateCommand(func(opts migrateOptions, _, _ io.Writer) error {
			calls++
			value = opts.ConfigPath
			return nil
		})
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if code := executeCommand(cmd, spelling, &stderr); code != 0 || calls != 1 || value != "-h=1" || stdout.Len() != 0 || stderr.Len() != 0 {
			t.Errorf("%q: exit=%d calls=%d config=%q stdout=%q stderr=%q", spelling, code, calls, value, stdout.String(), stderr.String())
		}
		stdout.Reset()
		stderr.Reset()
		if code := mainWithWriters(append(append([]string{"migrate"}, spelling...), "--help"), &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "cerbix migrate —") || stderr.Len() != 0 {
			t.Errorf("help after %q: exit=%d stdout=%q stderr=%q", spelling, code, stdout.String(), stderr.String())
		}
	}

	srv, fake := newChangeServer(t, http.StatusCreated, changeBodyRecorded, nil)
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", changeTestToken)
	t.Setenv("CERBIX_CA_FILE", "")
	base := []string{"change", "record", "--project", changeTestProject, "--service", changeTestService, "--kind", "deploy", "--phase", "started", "--source", "ci"}
	for _, spelling := range [][]string{{"--external-id", "-h=t"}, {"--external-id=-h=t"}} {
		args := append(append([]string{}, base...), spelling...)
		var stdout, stderr bytes.Buffer
		before := fake.hits.Load()
		if code := mainWithWriters(args, &stdout, &stderr); code != 0 || stderr.Len() != 0 || fake.hits.Load() != before+1 || fake.last()["external_id"] != "-h=t" {
			t.Errorf("%q: stdout=%q stderr=%q HTTP delta=%d body=%v", args, stdout.String(), stderr.String(), fake.hits.Load()-before, fake.last())
		}
		stdout.Reset()
		stderr.Reset()
		before = fake.hits.Load()
		if code := mainWithWriters(append(args, "--help"), &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "cerbix change record —") || stderr.Len() != 0 || fake.hits.Load() != before {
			t.Errorf("help after %q: stdout=%q stderr=%q HTTP delta=%d", args, stdout.String(), stderr.String(), fake.hits.Load()-before)
		}
	}
}

func TestOrdinaryPositionalPreflightPrecedesInvalidDuration(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", "local-review-only")
	t.Setenv("CERBIX_CA_FILE", "")

	base := []string{"gate", "check", "--project", "p", "--service", "s"}
	for _, tc := range []struct {
		suffix []string
		want   string
	}{
		{[]string{"--timeout=broken", "extra"}, "gate check: unexpected argument \"extra\"\n"},
		{[]string{"extra", "--timeout=broken"}, "gate check: unexpected argument \"extra\"\n"},
		{[]string{"--unknown", "value"}, "unknown flag: --unknown\n"},
	} {
		args := append(append([]string{}, base...), tc.suffix...)
		got, calls := runInstrumentedRoot(t, args)
		if got.code != 2 || got.stdout != "" || got.stderr != tc.want || calls != 0 {
			t.Errorf("injected %q: exit=%d stdout=%q stderr=%q calls=%d", args, got.code, got.stdout, got.stderr, calls)
		}
		var stdout, stderr bytes.Buffer
		before := hits.Load()
		code := mainWithWriters(args, &stdout, &stderr)
		if code != 2 || stdout.Len() != 0 || stderr.String() != tc.want || hits.Load() != before {
			t.Errorf("real %q: exit=%d stdout=%q stderr=%q HTTP delta=%d", args, code, stdout.String(), stderr.String(), hits.Load()-before)
		}
	}
	var stdout, stderr bytes.Buffer
	before := hits.Load()
	code := mainWithWriters(append(base, "--timeout=broken"), &stdout, &stderr)
	if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), `invalid duration "broken"`) || hits.Load() != before {
		t.Errorf("invalid duration without positional: exit=%d stdout=%q stderr=%q HTTP delta=%d", code, stdout.String(), stderr.String(), hits.Load()-before)
	}
}

func TestVersionMalformedHelpPrecedesTrueAndFalseHelp(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		code       int
		wantErr    string
		wantPrefix string
		calls      int
	}{
		{[]string{"--help", "-h=1"}, 2, "version: invalid help assignment \"-h=1\"\n", "", 0},
		{[]string{"-h=1", "--help"}, 2, "version: invalid help assignment \"-h=1\"\n", "", 0},
		{[]string{"--help=true", "--help=bad"}, 2, "version: invalid help assignment \"--help=bad\"\n", "", 0},
		{[]string{"--help=bad", "--help=true"}, 2, "version: invalid help assignment \"--help=bad\"\n", "", 0},
		{[]string{"--help=false", "--help"}, 0, "", "cerbix version —", 0},
		{[]string{"--help", "--help=false"}, 0, "", "cerbix version —", 0},
		{[]string{"--bogus", "--help"}, 2, "unknown flag: --bogus\n", "", 0},
		{[]string{"--help", "--bogus"}, 2, "unknown flag: --bogus\n", "", 0},
		{[]string{"--help", "-h=1", ""}, 2, "version: unexpected argument \"\"\n", "", 0},
	} {
		for _, direct := range []bool{false, true} {
			if direct && tc.wantPrefix != "" {
				continue // public version help rendering is exercised through the root command.
			}
			t.Run(fmt.Sprintf("%q/direct=%t", tc.args, direct), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				calls := 0
				execute := func(io.Writer) error { calls++; return nil }
				var code int
				if direct {
					cmd := newVersionCommand(execute)
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SilenceUsage, cmd.SilenceErrors = true, true
					code = executeCommand(cmd, tc.args, &stderr)
				} else {
					root := newRootCommandWithVersionExecutor(&stdout, &stderr, execute)
					code = executeCommand(root, append([]string{"version"}, tc.args...), &stderr)
				}
				if code != tc.code || stdout.Len() != 0 && tc.wantPrefix == "" || !strings.HasPrefix(stdout.String(), tc.wantPrefix) || stderr.String() != tc.wantErr || calls != tc.calls {
					t.Errorf("exit=%d stdout=%q stderr=%q calls=%d, want exit=%d prefix=%q stderr=%q calls=%d", code, stdout.String(), stderr.String(), calls, tc.code, tc.wantPrefix, tc.wantErr, tc.calls)
				}
			})
		}
	}
}
