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

func TestUnsupportedShorthandClustersFailBeforeRuntime(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", "cluster-test")
	t.Setenv("CERBIX_CA_FILE", "")

	paths := []struct {
		name string
		args []string
	}{
		{name: "migrate", args: []string{"migrate", "--config", "/nonexistent/cerbix-0197"}},
		{name: "adopt-fact-month", args: []string{"adopt-fact-month", "--config", "/nonexistent/cerbix-0197", "--month", "2026-01"}},
		{name: "enqueue-service-repair", args: []string{"enqueue-service-repair", "--config", "/nonexistent/cerbix-0197", "--project", "p", "--service", "s", "--from", "2026-01-01T00:00:00Z", "--to", "2026-01-01T01:00:00Z"}},
		{name: "gate check", args: []string{"gate", "check", "--project", "p", "--service", "s"}},
		{name: "change record", args: []string{"change", "record", "--project", "p", "--service", "s", "--kind", "deploy", "--phase", "started", "--source", "ci", "--external-id", "1"}},
	}
	for _, path := range paths {
		for _, token := range []string{"-hh=false", "-hhh=false", "-hh=true", "-hhh=true", "-hh", "-hhh", "-hfalse", "-htrue", "-abc", "-config"} {
			t.Run(path.name+"/"+token, func(t *testing.T) {
				args := append(append([]string{}, path.args...), token)
				want := fmt.Sprintf("%s: unsupported shorthand flag %q\n", path.name, token)
				assertUnsupportedShorthand(t, args, want, &hits)
			})
		}
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "root position", args: []string{"-hh=false", "migrate"}, want: "cerbix: unsupported shorthand flag \"-hh=false\"\n"},
		{name: "group position", args: []string{"gate", "-hh=false", "check", "--project", "p", "--service", "s"}, want: "gate: unsupported shorthand flag \"-hh=false\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) { assertUnsupportedShorthand(t, tc.args, tc.want, &hits) })
	}
}

func assertUnsupportedShorthand(t *testing.T, args []string, want string, hits *atomic.Int32) {
	t.Helper()
	instrumented, calls := runInstrumentedRoot(t, args)
	if instrumented.code != 2 || instrumented.stdout != "" || instrumented.stderr != want || calls != 0 {
		t.Errorf("instrumented %v: exit=%d calls=%d stdout=%q stderr=%q, want exit 2, zero calls and %q", args, instrumented.code, calls, instrumented.stdout, instrumented.stderr, want)
	}
	var stdout, stderr bytes.Buffer
	before := hits.Load()
	code := mainWithWriters(args, &stdout, &stderr)
	if code != 2 || stdout.Len() != 0 || stderr.String() != want {
		t.Errorf("real %v: exit=%d stdout=%q stderr=%q, want exit 2 and %q", args, code, stdout.String(), stderr.String(), want)
	}
	if strings.Contains(stderr.String(), "config_load_failed") {
		t.Errorf("%v reached config loader: %q", args, stderr.String())
	}
	if after := hits.Load(); after != before {
		t.Errorf("%v: HTTP delta=%d, want 0", args, after-before)
	}
}

func TestClusterLookingStringValuesReachExecutors(t *testing.T) {
	for _, args := range [][]string{{"--config", "-hh=false"}, {"--config=-hh=false"}} {
		var stdout, stderr bytes.Buffer
		var value string
		calls := 0
		cmd := newMigrateCommand(func(opts migrateOptions, _, _ io.Writer) error {
			calls++
			value = opts.ConfigPath
			return nil
		})
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		if code := executeCommand(cmd, args, &stderr); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 || calls != 1 || value != "-hh=false" {
			t.Errorf("migrate %v: exit=%d calls=%d config=%q stdout=%q stderr=%q", args, code, calls, value, stdout.String(), stderr.String())
		}
	}

	base := []string{"record", "--project", "p", "--service", "s", "--kind", "deploy", "--phase", "started", "--source", "ci"}
	for _, suffix := range [][]string{{"--external-id", "-hh=false"}, {"--external-id=-hh=false"}} {
		var stdout, stderr bytes.Buffer
		var value string
		calls := 0
		cmd := newChangeCommand(func(opts changeRecordOptions, _, _ io.Writer) error {
			calls++
			value = opts.ExternalID
			return nil
		})
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		args := append(append([]string{}, base...), suffix...)
		if code := executeCommand(cmd, args, &stderr); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 || calls != 1 || value != "-hh=false" {
			t.Errorf("change %v: exit=%d calls=%d external-id=%q stdout=%q stderr=%q", args, code, calls, value, stdout.String(), stderr.String())
		}
	}
}

func TestClusterLookingStringValuesReachHTTPBodyAndRealHelp(t *testing.T) {
	srv, fake := newChangeServer(t, http.StatusCreated, changeBodyRecorded, nil)
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", changeTestToken)
	t.Setenv("CERBIX_CA_FILE", "")
	base := []string{"change", "record", "--project", changeTestProject, "--service", changeTestService, "--kind", "deploy", "--phase", "started", "--source", "github-actions"}
	for _, suffix := range [][]string{{"--external-id", "-hh=false"}, {"--external-id=-hh=false"}} {
		var stdout, stderr bytes.Buffer
		args := append(append([]string{}, base...), suffix...)
		before := fake.hits.Load()
		if code := mainWithWriters(args, &stdout, &stderr); code != 0 || stderr.Len() != 0 || fake.hits.Load() != before+1 {
			t.Errorf("%v: exit=%d stdout=%q stderr=%q HTTP delta=%d", args, code, stdout.String(), stderr.String(), fake.hits.Load()-before)
		}
		if got := fake.last()["external_id"]; got != "-hh=false" {
			t.Errorf("%v: external_id=%v, want literal -hh=false", args, got)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"migrate", "--config", "-hh=false", "--help"}, want: "cerbix migrate —"},
		{args: []string{"migrate", "--config=-hh=false", "--help"}, want: "cerbix migrate —"},
		{args: append(append([]string{}, base...), "--external-id", "-hh=false", "--help"), want: "cerbix change record —"},
		{args: append(append([]string{}, base...), "--external-id=-hh=false", "--help"), want: "cerbix change record —"},
	} {
		var stdout, stderr bytes.Buffer
		before := fake.hits.Load()
		code := mainWithWriters(tc.args, &stdout, &stderr)
		if code != 0 || !strings.HasPrefix(stdout.String(), tc.want) || stderr.Len() != 0 || fake.hits.Load() != before {
			t.Errorf("%v: exit=%d stdout=%q stderr=%q HTTP delta=%d", tc.args, code, stdout.String(), stderr.String(), fake.hits.Load()-before)
		}
	}
}

func TestLiteralDashAndEndOfOptionsAreNotClusters(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"migrate", "--config", "/nonexistent/cerbix-0197", "--", "-hh=false"}, want: "migrate: unexpected argument \"-hh=false\"\n"},
		{args: []string{"version", "--", "-hh=false"}, want: "version: unexpected argument \"-hh=false\"\n"},
		{args: []string{"version", "-"}, want: "version: unexpected argument \"-\"\n"},
	} {
		var stdout, stderr bytes.Buffer
		code := mainWithWriters(tc.args, &stdout, &stderr)
		if code != 2 || stdout.Len() != 0 || stderr.String() != tc.want {
			t.Errorf("%v: exit=%d stdout=%q stderr=%q, want positional usage %q", tc.args, code, stdout.String(), stderr.String(), tc.want)
		}
	}
}

func TestVersionEmptyPositionalsFailBeforeEncoder(t *testing.T) {
	for _, args := range [][]string{
		{""}, {"", "extra"}, {"--bogus", ""}, {"--bogus", "", "extra"},
		{"--", ""}, {"--", "", "extra"}, {"--help=invalid", ""}, {"--help=invalid", "", "extra"},
		{"--bogus", "--help=false", ""}, {"--bogus", "--help=false", "", "extra"},
	} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/direct=%t", args, direct), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				calls := 0
				execute := func(w io.Writer) error { calls++; _, err := io.WriteString(w, "version-json\n"); return err }
				var code int
				if direct {
					cmd := newVersionCommand(execute)
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SilenceErrors, cmd.SilenceUsage = true, true
					code = executeCommand(cmd, args, &stderr)
				} else {
					root := newRootCommandWithVersionExecutor(&stdout, &stderr, execute)
					code = executeCommand(root, append([]string{"version"}, args...), &stderr)
				}
				if code != 2 || stdout.Len() != 0 || stderr.String() != "version: unexpected argument \"\"\n" || calls != 0 {
					t.Errorf("%v direct=%t: exit=%d calls=%d stdout=%q stderr=%q", args, direct, code, calls, stdout.String(), stderr.String())
				}
			})
		}
	}
}

func TestVersionWithoutPositionalKeepsHelpAndUnknownFlagPolicy(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		code  int
		out   string
		err   string
		calls int
	}{
		{args: []string{"--bogus"}, out: "json\n", calls: 1},
		{args: []string{"--bogus", "--help=false"}, out: "json\n", calls: 1},
		{args: []string{"--help=invalid"}, code: 2, err: `version: invalid help assignment "--help=invalid"`, calls: 0},
		{args: []string{"--help=false", "--help"}, out: "cerbix version —", calls: 0},
	} {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			root := newRootCommandWithVersionExecutor(&stdout, &stderr, func(w io.Writer) error {
				calls++
				_, err := io.WriteString(w, "json\n")
				return err
			})
			code := executeCommand(root, append([]string{"version"}, tc.args...), &stderr)
			if code != tc.code || calls != tc.calls {
				t.Errorf("exit=%d calls=%d stdout=%q stderr=%q", code, calls, stdout.String(), stderr.String())
			}
			if tc.err != "" {
				if stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.err) {
					t.Errorf("stdout=%q stderr=%q, want error %q", stdout.String(), stderr.String(), tc.err)
				}
			} else if !strings.HasPrefix(stdout.String(), tc.out) || stderr.Len() != 0 {
				t.Errorf("stdout=%q stderr=%q, want output prefix %q", stdout.String(), stderr.String(), tc.out)
			}
		})
	}
}

type rejectingVersionWriter struct{ attempts int }

func (w *rejectingVersionWriter) Write([]byte) (int, error) {
	w.attempts++
	return 0, fmt.Errorf("write failed")
}

func TestVersionWriterFailureIsRuntimeErrorNotUsage(t *testing.T) {
	writer := &rejectingVersionWriter{}
	var stderr bytes.Buffer
	calls := 0
	root := newRootCommandWithVersionExecutor(writer, &stderr, func(w io.Writer) error {
		calls++
		_, err := io.WriteString(w, "version-json\n")
		return err
	})
	code := executeCommand(root, []string{"version"}, &stderr)
	if code != 1 || calls != 1 || writer.attempts != 1 || stderr.String() != "version: write failed\n" {
		t.Errorf("exit=%d calls=%d write attempts=%d stderr=%q, want runtime exit 1, one encoder attempt and version diagnostic", code, calls, writer.attempts, stderr.String())
	}

	writer = &rejectingVersionWriter{}
	stderr.Reset()
	code = mainWithWriters([]string{"version"}, writer, &stderr)
	if code != 1 || writer.attempts != 1 || stderr.String() != "version: write failed\n" {
		t.Errorf("real JSON: exit=%d writes=%d stderr=%q", code, writer.attempts, stderr.String())
	}
}
