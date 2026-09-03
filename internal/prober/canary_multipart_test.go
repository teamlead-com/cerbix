package prober

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-029 phase E: the multipart submit, which is the shape the first use case uses. The fixture is a
// registry KEY resolved to embedded bytes with a verified digest — never a path, a URL or an operator
// file — and the runner owns the boundary, which is why the schema refuses an author's own
// `content-type` on this kind.
func TestCanaryMultipartUploadsThePinnedFixture(t *testing.T) {
	var gotDigest, gotField, gotCT, gotFileName string
	mux := http.NewServeMux()
	mux.HandleFunc("/files/upload", func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		defer file.Close()
		gotFileName = hdr.Filename
		body, _ := io.ReadAll(file)
		sum := sha256.Sum256(body)
		gotDigest = hex.EncodeToString(sum[:])
		gotField = r.FormValue("only_audio")
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-42"})
	})
	mux.HandleFunc("/tasks/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "s3_path": "canary/x.wav", "byte_size": 1})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := canaryMonitor(t, srv.URL, func(w *domain.CanaryWorkflow) {
		w.Submit.Kind = domain.CanarySubmitMultipartFixture
		w.Submit.Body = nil
		w.Submit.FixtureRef = "small_wav_v1"
		w.Submit.Multipart = &domain.CanaryMultipart{
			FileField: "file",
			Fields:    map[string]domain.CanaryValue{"only_audio": {Kind: domain.CanaryValueBool, Bool: false}},
		}
	})
	res := canaryTestProber().Probe(context.Background(), m)
	if res.Msg != "" {
		t.Fatalf("a multipart journey must pass: %s", res.Msg)
	}

	fixture, _ := domain.CanaryFixtureByRef("small_wav_v1")
	if gotDigest != fixture.SHA256 {
		t.Fatalf("uploaded digest = %s, want the pinned %s", gotDigest, fixture.SHA256)
	}
	if gotFileName != fixture.FileName {
		t.Fatalf("file name = %q, want %q", gotFileName, fixture.FileName)
	}
	// A boolean field is written as text a target can read, not as Go's formatting of an interface.
	if gotField != "false" {
		t.Fatalf("only_audio = %q, want \"false\"", gotField)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data; boundary=") {
		t.Fatalf("content type = %q, want the runner's own multipart boundary", gotCT)
	}
}

func TestCanaryMultipartRefusesAFixtureTheBinaryDoesNotCarry(t *testing.T) {
	f := newCanaryFixture(t)
	m := canaryMonitor(t, f.URL, func(w *domain.CanaryWorkflow) {
		w.Submit.Kind = domain.CanarySubmitMultipartFixture
		w.Submit.Body = nil
		w.Submit.FixtureRef = "not_in_the_registry"
		w.Submit.Multipart = &domain.CanaryMultipart{FileField: "file"}
	})
	res := canaryTestProber().Probe(context.Background(), m)
	if !strings.Contains(res.Msg, "fixture unavailable") {
		t.Fatalf("msg = %q, want a fixture refusal", res.Msg)
	}
	if f.submits != 0 {
		t.Fatalf("the target was contacted %d times before the refusal", f.submits)
	}
}
