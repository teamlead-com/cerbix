package domain

import (
	"errors"
	"reflect"
	"testing"
)

// sec-hardening-2 H-1 fix 1: the explicit list of machine clients, normalized identically on every
// path (YAML, DB, API, runtime) so a padded copy of the interactive client id cannot slip in.
func TestNormalizeMachineClients(t *testing.T) {
	got, err := NormalizeMachineClients([]string{" svc ", "svc", "", "  ", "ci"}, "cerbix")
	if err != nil || !reflect.DeepEqual(got, []string{"svc", "ci"}) {
		t.Fatalf("got %v (%v), want [svc ci]", got, err)
	}
	if _, err := NormalizeMachineClients([]string{"svc", "  cerbix "}, " cerbix"); !errors.Is(err, ErrOwnClientInMachineClients) {
		t.Fatalf("own client id: err %v, want ErrOwnClientInMachineClients", err)
	}
	if got, err := NormalizeMachineClients(nil, "cerbix"); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("nil list: got %#v (%v), want an empty non-nil list", got, err)
	}
	if got, err := NormalizeMachineClients([]string{"svc"}, ""); err != nil || !reflect.DeepEqual(got, []string{"svc"}) {
		t.Fatalf("no own client id: got %v (%v)", got, err)
	}
}
