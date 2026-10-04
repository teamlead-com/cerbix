package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestServeExitMappingPreservesZeroAndPrintedRuntimeErrors(t *testing.T) {
	opts := serveOptions{ConfigPath: "config.yaml", Role: "worker", Region: "eu-west"}
	for _, code := range []int{0, 1, 2, 4} {
		t.Run(string(rune('0'+code)), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			run := func(got serveOptions, out, errOut io.Writer) int {
				calls++
				if got != opts || out != &stdout || errOut != &stderr {
					t.Errorf("runtime received options=%+v, stdout=%v, stderr=%v", got, out, errOut)
				}
				_, _ = io.WriteString(out, "runtime stdout\n")
				_, _ = io.WriteString(errOut, "runtime stderr\n")
				return code
			}
			err := executeServeWithRuntime(opts, &stdout, &stderr, run)
			if calls != 1 || stdout.String() != "runtime stdout\n" || stderr.String() != "runtime stderr\n" {
				t.Errorf("runtime calls=%d stdout=%q stderr=%q", calls, stdout.String(), stderr.String())
			}
			if code == 0 {
				if err != nil {
					t.Errorf("successful serve returned %v, want nil", err)
				}
				return
			}
			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != code || !exitErr.Printed {
				t.Errorf("serve runtime code %d: error=%v, want printed ExitError", code, err)
			}
		})
	}

	var stdout, stderr bytes.Buffer
	err := executeServe(serveOptions{ConfigPath: "/nonexistent/cerbix-0197"}, &stdout, &stderr)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 || !exitErr.Printed || stdout.Len() != 0 || !strings.Contains(stderr.String(), "config_load_failed") {
		t.Errorf("executeServe: error=%v stdout=%q stderr=%q, want printed runtime exit 1", err, stdout.String(), stderr.String())
	}
}
