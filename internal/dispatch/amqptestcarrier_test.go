package dispatch_test

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"sort"
	"testing"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestEveryTestCarrierAnswersOnItsOwnQueue publishes one Test Connection on EVERY test
// carrier this executor serves and requires each to come back.
//
// It exists because generation 3 did not. `serveEnvelopeTestsOnce` serves both the v2 and
// the v3 test queue, and its admission check compared the delivery's own ProtocolVersion
// against a hardcoded ProtocolV2 — so a generation-3 Test Connection was dead-lettered by
// the consumer bound to its own queue. The caller is answered only on success, so it waited
// out its timeout and reported `no worker responded in region "core"`: the same message an
// operator gets when the region is genuinely empty.
//
// Nothing caught it. The v2 carrier had a test and passed; the v3 carrier had none; the
// inproc dev stack never takes this path at all; and `make dev-test-distributed`, the one
// gate that does, was red for an unrelated wiring reason that masked it.
//
// So the assertion is over the SET of carriers, not over the one that broke:
// TestTheTestCarrierTableCoversEveryServeEntryPoint below fails if a fourth is added and
// left out of this table.
func TestEveryTestCarrierAnswersOnItsOwnQueue(t *testing.T) {
	url := os.Getenv("CERBIX_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("set CERBIX_TEST_RABBITMQ_URL to run the AMQP test-carrier test")
	}
	const region = "testcarrier"
	d, err := dispatch.NewAMQP(url, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = d.Close()
		deleteTestQueue(t, url, "checks.tests."+region)
		deleteTestQueue(t, url, "checks.tests.v2."+region)
		deleteTestQueue(t, url, "checks.tests.v3."+region)
	})
	d.WithJobRegion(region).WithCredentialCapability(dispatch.EnvelopeV2)
	ctx := context.Background()

	keyring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{
		ID:  "testcarrier-key",
		Key: bytes.Repeat([]byte{0x33}, 32),
	}, nil)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}

	// ONE runner behind every carrier, exactly as `cli.go` wires it. It reports the
	// generation the CONSUMER stamped, which is the property under test: a reply proves the
	// delivery was admitted, and the stamp proves it was admitted by the right consumer.
	runner := func(_ context.Context, delivered dispatch.DeliveredJob) (domain.Heartbeat, error) {
		return domain.Heartbeat{
			MonitorID: delivered.Job.Monitor.ID,
			Up:        true,
			Code:      200 + delivered.CarrierGeneration,
		}, nil
	}
	if err := d.ServeTests(runner); err != nil {
		t.Fatalf("start v1 test consumer: %v", err)
	}
	if err := d.ServeTestsV2(runner); err != nil {
		t.Fatalf("start v2 test consumer: %v", err)
	}
	if err := d.ServeTestsV3(runner); err != nil {
		t.Fatalf("start v3 test consumer: %v", err)
	}

	for _, tc := range testCarrierTable {
		t.Run(tc.name, func(t *testing.T) {
			// A real type and target, not a zero monitor: from envelope v2 the AAD binds a
			// digest of the execution body, and sealing consults the type's credential
			// settings schema. A typeless monitor cannot be sealed at all, which is a
			// property of the binding rather than of this test.
			m := domain.Monitor{
				ID: tc.monitorID, Region: region, Type: "postgres", Target: "postgres:5432",
				ExecutionRevision: 7, TimeoutSeconds: 2,
			}
			job := dispatch.CheckJob{Monitor: m, ProtocolVersion: tc.generation}
			if tc.envelopeVersion != 0 {
				env, sealErr := keyring.Seal(dispatch.SealContext{
					EnvelopeVersion: tc.envelopeVersion,
					Region:          region,
					JobID:           "test-" + tc.name,
					MonitorID:       m.ID,
					Revision:        m.ExecutionRevision,
					Body:            m,
				}, map[string][]byte{"password": []byte("carrier-secret")})
				if sealErr != nil {
					t.Fatalf("seal: %v", sealErr)
				}
				job.CredentialEnvelope = env
			}
			hb, err := d.RunJobTest(ctx, job)
			if err != nil {
				t.Fatalf("test RPC on carrier %d: %v (a timeout here means the consumer bound to this queue refused the delivery)", tc.generation, err)
			}
			if !hb.Up || hb.Code != 200+tc.generation {
				t.Fatalf("carrier %d answered %+v, want up with code %d — a reply stamped with another generation means the wrong consumer took it", tc.generation, hb, 200+tc.generation)
			}
		})
	}
}

// testCarrierTable is the enumerated set of test carriers, and the thing the source scan
// below holds the product to. `serveMethod` is the exported entry point that starts the
// consumer for this generation.
var testCarrierTable = []struct {
	name            string
	serveMethod     string
	generation      int
	envelopeVersion int // 0 = this carrier takes no credential envelope
	monitorID       string
}{
	{name: "v1_plain", serveMethod: "ServeTests", generation: dispatch.ProtocolV1, monitorID: "00000000-0000-4000-8000-0000000001a1"},
	{name: "v2_envelope_v1", serveMethod: "ServeTestsV2", generation: dispatch.ProtocolV2, envelopeVersion: dispatch.EnvelopeV1, monitorID: "00000000-0000-4000-8000-0000000001a2"},
	{name: "v3_envelope_v2", serveMethod: "ServeTestsV3", generation: dispatch.ProtocolV3, envelopeVersion: dispatch.EnvelopeV2, monitorID: "00000000-0000-4000-8000-0000000001a3"},
}

// TestTheTestCarrierTableCoversEveryServeEntryPoint reads the product source for every
// exported `ServeTests…` method on *AMQP and requires the table above to name it.
//
// The count is not the guard — the SET is. A fourth test carrier added and wired in
// `cli.go` fails this by name here, before it can repeat generation 3's history of being
// declared, consumed, and reachable by nothing. This runs with no broker.
func TestTheTestCarrierTableCoversEveryServeEntryPoint(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "amqp.go", nil, 0)
	if err != nil {
		t.Fatalf("parse amqp.go: %v", err)
	}
	var inSource []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || !fn.Name.IsExported() {
			return true
		}
		if len(fn.Name.Name) < len("ServeTests") || fn.Name.Name[:len("ServeTests")] != "ServeTests" {
			return true
		}
		inSource = append(inSource, fn.Name.Name)
		return true
	})
	if len(inSource) == 0 {
		t.Fatal("found no ServeTests entry point in amqp.go — the scan is looking in the wrong place")
	}
	covered := map[string]bool{}
	for _, tc := range testCarrierTable {
		covered[tc.serveMethod] = true
	}
	var missing []string
	for _, name := range inSource {
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("test carriers with no row in testCarrierTable: %v — every one of them is a queue an executor consumes and no test ever publishes to", missing)
	}
}
