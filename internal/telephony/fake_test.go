package telephony

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestFakeProviderRecordsAndAnswersSentByDefault(t *testing.T) {
	f := NewFakeProvider()
	var _ Provider = f
	if f.Name() != "fake" {
		t.Errorf("Name = %q", f.Name())
	}
	if !f.SupportsVoice() {
		t.Errorf("a new fake supports voice")
	}
	r1 := f.SendSMS(context.Background(), testTo, "hello")
	r2 := f.PlaceCall(context.Background(), testTo, "<Response/>")
	if r1.Outcome != Sent || r1.HTTPStatus != 201 || r1.Ref == "" {
		t.Errorf("sms result = %+v", r1)
	}
	if r2.Outcome != Sent || r2.Ref == "" || r2.Ref == r1.Ref {
		t.Errorf("call result = %+v (sms ref %q)", r2, r1.Ref)
	}
	want := []FakeCall{{Kind: "sms", To: testTo, Body: "hello"}, {Kind: "voice", To: testTo, Body: "<Response/>"}}
	got := f.Calls()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("calls = %+v, want %+v", got, want)
	}
}

func TestFakeProviderScriptedResultsInOrderThenDefault(t *testing.T) {
	f := NewFakeProvider()
	f.ScriptSMS(Result{Outcome: Unreachable, HTTPStatus: 503}, Result{Outcome: Rejected, HTTPStatus: 400, Code: "21211"})
	f.ScriptCall(Result{Outcome: Rejected, HTTPStatus: 400, Code: "13224"})

	if r := f.SendSMS(context.Background(), testTo, "a"); r != (Result{Outcome: Unreachable, HTTPStatus: 503}) {
		t.Errorf("first sms = %+v", r)
	}
	if r := f.PlaceCall(context.Background(), testTo, "x"); r != (Result{Outcome: Rejected, HTTPStatus: 400, Code: "13224"}) {
		t.Errorf("first call = %+v", r)
	}
	if r := f.SendSMS(context.Background(), testTo, "b"); r != (Result{Outcome: Rejected, HTTPStatus: 400, Code: "21211"}) {
		t.Errorf("second sms = %+v", r)
	}
	if r := f.SendSMS(context.Background(), testTo, "c"); r.Outcome != Sent {
		t.Errorf("third sms = %+v, want the default", r)
	}
	if r := f.PlaceCall(context.Background(), testTo, "y"); r.Outcome != Sent {
		t.Errorf("second call = %+v, want the default", r)
	}
	if n := len(f.Calls()); n != 5 {
		t.Errorf("recorded %d calls, want 5", n)
	}
}

// Without voice the fake answers as generic_http does: rejected, no request recorded.
func TestFakeProviderWithoutVoice(t *testing.T) {
	f := NewFakeProvider()
	f.SetSupportsVoice(false)
	if f.SupportsVoice() {
		t.Fatal("SupportsVoice after SetSupportsVoice(false)")
	}
	if r := f.PlaceCall(context.Background(), testTo, "x"); r != (Result{Outcome: Rejected, Code: CodeVoiceUnsupported}) {
		t.Errorf("result = %+v", r)
	}
	if n := len(f.Calls()); n != 0 {
		t.Errorf("recorded %d calls, want 0", n)
	}
}

func TestFakeProviderCallsIsACopy(t *testing.T) {
	f := NewFakeProvider()
	f.SendSMS(context.Background(), testTo, "a")
	c := f.Calls()
	if len(c) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(c))
	}
	c[0].To = "changed"
	if f.Calls()[0].To != testTo {
		t.Error("Calls exposes the fake's own slice")
	}
}

// Run under -race.
func TestFakeProviderIsSafeForConcurrentUse(t *testing.T) {
	f := NewFakeProvider()
	f.ScriptSMS(Result{Outcome: Unreachable}, Result{Outcome: Unreachable})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			to := fmt.Sprintf("+1202555%04d", i)
			f.SendSMS(context.Background(), to, "x")
			f.PlaceCall(context.Background(), to, "y")
			f.SetSupportsVoice(true)
			_ = f.SupportsVoice()
			_ = f.Calls()
			f.ScriptCall(Result{Outcome: Sent})
		}(i)
	}
	wg.Wait()
	if n := len(f.Calls()); n != 100 {
		t.Errorf("recorded %d calls, want 100", n)
	}
}
