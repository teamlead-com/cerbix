package dispatch_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// The carrier -> envelope mapping, and the rule that it is EXACT rather than a floor
// (`func-secret-inventory.md` §4.7, D-0160; reviewer P0 found auditing `0a1557c..d0a8fed`).
//
// What was wrong: three places agreed that an envelope must not ride a generation-1 carrier and
// none of them checked which envelope the carrier actually defines. `Jobs()` tested only the
// capability CEILING, the test consumer tested only PRESENCE, and `ValidateAndMaterialize` tested
// only `carrier < 2` — while the producer had been exact since generation 3 shipped. So a
// generation-1 envelope on the generation-3 carrier opened normally and its credential reached the
// prober, WITHOUT the execution-body binding that generation 3 exists to add: `bindingFor` computes
// no digest below envelope v2, so the credential is not tied to the target it was sealed for.
//
// Under this specification's own threat model — the body is attacker-editable, the carrier is the
// trusted out-of-band signal — that is a downgrade an attacker selects, not an accident.

// carrierEnvelope is the mapping, written once here and asserted against the product's own
// function in both directions, so this table cannot drift into being a second opinion.
// Generation 1 is deliberately ABSENT: it carries no envelope, so it has no mapping and
// `EnvelopeForCarrier` errors on it. Listing it with `EnvelopeV1` is what the lifted switch did,
// and it made the function contradict `CarrierEnvelopeAdmissible`, which refuses every envelope
// there (reviewer P1). It is asserted as an error below, beside the unknown generations.
var carrierEnvelope = map[int]int{
	dispatch.ProtocolV2: dispatch.EnvelopeV1,
	dispatch.ProtocolV3: dispatch.EnvelopeV2,
	dispatch.ProtocolV4: dispatch.EnvelopeV2,
}

func TestTheCarrierEnvelopeMappingIsExactAndHasOneOwner(t *testing.T) {
	for carrier, want := range carrierEnvelope {
		got, err := dispatch.EnvelopeForCarrier(carrier)
		if err != nil {
			t.Fatalf("carrier %d: %v", carrier, err)
		}
		if got != want {
			t.Errorf("carrier %d carries envelope %d, want %d", carrier, got, want)
		}
	}
	// A carrier nobody defined is an ERROR, never a default. A silent zero here would make every
	// envelope on an unknown carrier "wrong" in a way that reads as a mismatch rather than as a
	// carrier the product does not know.
	for _, none := range []int{dispatch.ProtocolV1, 0, 5, 99, -1} {
		if _, err := dispatch.EnvelopeForCarrier(none); err == nil {
			t.Errorf("carrier %d was given an envelope generation instead of an error", none)
		}
	}
}

func TestAnEnvelopeMustBeTheONEItsCarrierDefines(t *testing.T) {
	// A missing envelope is always admissible here; whether the monitor REQUIRED one is a
	// different question, answered further into the gate.
	for _, carrier := range []int{dispatch.ProtocolV1, dispatch.ProtocolV2, dispatch.ProtocolV3, dispatch.ProtocolV4} {
		if err := dispatch.CarrierEnvelopeAdmissible(carrier, nil); err != nil {
			t.Errorf("carrier %d refused a delivery with no envelope: %v", carrier, err)
		}
	}
	// The legacy carrier takes no envelope at all, whichever generation the envelope claims.
	for _, v := range []int{dispatch.EnvelopeV1, dispatch.EnvelopeV2} {
		if err := dispatch.CarrierEnvelopeAdmissible(dispatch.ProtocolV1, &dispatch.CredentialEnvelope{V: v}); err == nil {
			t.Errorf("carrier 1 admitted an envelope v%d", v)
		}
	}
	for carrier, want := range carrierEnvelope {
		for _, v := range []int{dispatch.EnvelopeV1, dispatch.EnvelopeV2} {
			err := dispatch.CarrierEnvelopeAdmissible(carrier, &dispatch.CredentialEnvelope{V: v})
			switch {
			case v == want && err != nil:
				t.Errorf("carrier %d refused its own envelope v%d: %v", carrier, v, err)
			case v != want && err == nil:
				t.Errorf("carrier %d admitted envelope v%d, but it carries v%d", carrier, v, want)
			}
		}
	}
}

// TestADowngradedEnvelopeNeverReachesTheRunner is the reproduction, at the ONE gate every executor
// path crosses (`internal/worker`, `internal/agent` twice, and the in-process runner in
// `internal/cli` twice all call it). Before the fix the mismatched case returned a Materialized
// with UsedCredential true.
var ledgerInstant = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

func TestADowngradedEnvelopeNeverReachesTheRunner(t *testing.T) {
	ring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{
		ID: "carrier-envelope-key", Key: bytes.Repeat([]byte{0x44}, 32),
	}, nil)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	monitor := domain.Monitor{
		ID: "00000000-0000-4000-8000-0000000009a1", Region: "core", Type: "postgres",
		Target: "postgres:5432", ExecutionRevision: 3,
		Config: map[string]string{"username": "u", "database": "d"},
	}
	seal := func(v int) *dispatch.CredentialEnvelope {
		env, sealErr := ring.Seal(dispatch.SealContext{
			EnvelopeVersion: v, Region: monitor.Region, JobID: "job-carrier-envelope",
			MonitorID: monitor.ID, Revision: monitor.ExecutionRevision, Body: monitor,
		}, map[string][]byte{"password": []byte("s3cret")})
		if sealErr != nil {
			t.Fatalf("seal v%d: %v", v, sealErr)
		}
		return env
	}
	for carrier, want := range carrierEnvelope {
		for _, v := range []int{dispatch.EnvelopeV1, dispatch.EnvelopeV2} {
			job := dispatch.CheckJob{
				Monitor: monitor, ProtocolVersion: carrier, CredentialEnvelope: seal(v),
			}
			if dispatch.RequireLedgerFields(carrier) {
				// A generation-4 delivery is DEFINED by carrying job identity, so without these
				// the gate refuses it for that reason and the envelope rule is never reached.
				job.JobID = "job-carrier-envelope"
				job.IssuedAt = ledgerInstant
				job.DueAt = ledgerInstant
			}
			m, err := dispatch.ValidateAndMaterialize(ring, dispatch.DeliveredJob{
				Job: job, CarrierGeneration: carrier,
			})
			if v == want {
				if err != nil {
					t.Errorf("carrier %d refused its own envelope v%d: %v", carrier, v, err)
					continue
				}
				if !m.UsedCredential {
					t.Errorf("carrier %d with envelope v%d materialized no credential", carrier, v)
				}
				m.Cleanup()
				continue
			}
			if err == nil {
				m.Cleanup()
				t.Errorf("carrier %d ADMITTED envelope v%d (it carries v%d) — the credential reached the runner", carrier, v, want)
			}
		}
	}
}

// TestAnUnknownEnvelopeVersionKeepsItsOwnProbeErrorReason pins the ORDER of two refusals, which
// is a wire-visible property rather than an implementation detail.
//
// The probe-error taxonomy is bounded and CHECK'd on the ingest side, so no reason is minted for
// the carrier rule: a known envelope on the wrong carrier lands in `decrypt_auth_failed` with
// every other structural rejection, deliberately, so the executor never tells a prober which way
// its forgery was wrong. But an envelope version this executor does not implement AT ALL already
// had its own honest member, `unsupported_version`, and answering it as a carrier mismatch would
// report a FUTURE envelope during a rolling upgrade as a decryption failure — sending an operator
// after a key problem that does not exist. So the unknown-version question is asked first.
func TestAnUnknownEnvelopeVersionKeepsItsOwnProbeErrorReason(t *testing.T) {
	// EVERY carrier, generation 1 included. The first version of this test enumerated 2..4 while
	// its comment said "first" without exception, and the code refused a generation-1 carrier
	// before it ever asked about the version — so the described order and the real one differed
	// exactly where the test did not look (reviewer P1).
	future := &dispatch.CredentialEnvelope{V: dispatch.EnvelopeV2 + 1}
	for _, carrier := range []int{dispatch.ProtocolV1, dispatch.ProtocolV2, dispatch.ProtocolV3, dispatch.ProtocolV4} {
		err := dispatch.CarrierEnvelopeAdmissible(carrier, future)
		if err == nil {
			t.Fatalf("carrier %d admitted envelope v%d", carrier, future.V)
		}
		if got := dispatch.CredentialProbeErrorReason(err); got != domain.ProbeErrorUnsupportedVersion {
			t.Errorf("carrier %d, envelope v%d reported %q, want %q — a future envelope must not read as a key failure",
				carrier, future.V, got, domain.ProbeErrorUnsupportedVersion)
		}
	}
	// A KNOWN envelope on the wrong carrier stays non-oracular, as the taxonomy's own comment
	// requires. Asserted so that "unsupported_version" cannot quietly swallow this case too.
	// Both shapes of it: the wrong generation, and the carrier that takes none at all.
	for _, tc := range []struct {
		name    string
		carrier int
		version int
	}{
		{"a known envelope on the wrong carrier", dispatch.ProtocolV3, dispatch.EnvelopeV1},
		{"a known envelope on the carrier that takes none", dispatch.ProtocolV1, dispatch.EnvelopeV2},
	} {
		err := dispatch.CarrierEnvelopeAdmissible(tc.carrier, &dispatch.CredentialEnvelope{V: tc.version})
		if err == nil {
			t.Fatalf("%s: carrier %d admitted envelope v%d", tc.name, tc.carrier, tc.version)
		}
		if got := dispatch.CredentialProbeErrorReason(err); got != domain.ProbeErrorDecryptAuthFailed {
			t.Errorf("%s reported %q, want %q", tc.name, got, domain.ProbeErrorDecryptAuthFailed)
		}
	}
}

// TestEveryDeliveredJobOutsideTheTransportCrossesTheGate is the reason the fix above lives in
// `ValidateAndMaterialize` and not in three consumers.
//
// The carrier->envelope rule is enforced once, at the gate every executor path crosses; the two
// AMQP consumers additionally dead-letter, because a poison body on a broker must survive for
// inspection. That design is only sound while the gate really is unavoidable — so this scans the
// product for every construction of a `DeliveredJob` outside the transport package and requires
// `ValidateAndMaterialize` in the SAME function. A future path that builds one and hands it
// straight to a prober fails here by function name, before it can carry a rule of its own.
func TestEveryDeliveredJobOutsideTheTransportCrossesTheGate(t *testing.T) {
	// The transport package itself is exempt BY ROLE: `Jobs()` and the test consumers hand a
	// DeliveredJob to a channel or a callback, and the executor behind it calls the gate.
	files := []string{
		"../worker/worker.go",
		"../agent/agent.go",
		"../cli/cli.go",
	}
	seen := 0
	for _, path := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			builds, gates := false, false
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				switch v := m.(type) {
				case *ast.CompositeLit:
					if sel, ok := v.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "DeliveredJob" {
						builds = true
					}
				case *ast.CallExpr:
					if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ValidateAndMaterialize" {
						gates = true
					}
				}
				return true
			})
			if builds {
				seen++
				if !gates {
					t.Errorf("%s: %s builds a DeliveredJob and never calls ValidateAndMaterialize — it would carry the carrier rules of its own", path, fn.Name.Name)
				}
			}
			return true
		})
	}
	if seen == 0 {
		t.Fatal("the scan found no DeliveredJob construction at all — it is looking in the wrong files")
	}
}
