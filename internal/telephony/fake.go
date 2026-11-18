package telephony

import (
	"context"
	"fmt"
	"sync"
)

// FakeCall is one request a FakeProvider received.
type FakeCall struct {
	Kind string // "sms" | "voice"
	To   string
	Body string // the SMS text or the call's TwiML
}

// FakeProvider is a Provider for other packages' tests: no network, every request recorded, results
// scriptable per kind. Unscripted requests are Sent with status 201 and a unique Ref. Safe for
// concurrent use.
type FakeProvider struct {
	mu          sync.Mutex
	name        string
	voice       bool
	calls       []FakeCall
	smsScript   []Result
	callScript  []Result
	nextRefSeed int
}

// NewFakeProvider returns a fake named "fake" that supports voice.
func NewFakeProvider() *FakeProvider { return &FakeProvider{name: "fake", voice: true} }

func (f *FakeProvider) Name() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.name
}

// SetName changes what Name reports.
func (f *FakeProvider) SetName(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.name = name
}

func (f *FakeProvider) SupportsVoice() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.voice
}

// SetSupportsVoice makes the fake behave as a provider with or without voice. Without it PlaceCall
// answers as generic_http does — Rejected with CodeVoiceUnsupported — and records nothing.
func (f *FakeProvider) SetSupportsVoice(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.voice = v
}

// ScriptSMS queues results for the next SendSMS calls, in order.
func (f *FakeProvider) ScriptSMS(results ...Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.smsScript = append(f.smsScript, results...)
}

// ScriptCall queues results for the next PlaceCall calls, in order.
func (f *FakeProvider) ScriptCall(results ...Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callScript = append(f.callScript, results...)
}

// Calls returns a copy of the recorded requests, oldest first.
func (f *FakeProvider) Calls() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeCall(nil), f.calls...)
}

func (f *FakeProvider) SendSMS(_ context.Context, to, text string) Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, FakeCall{Kind: "sms", To: to, Body: text})
	return f.next(&f.smsScript, "SM")
}

func (f *FakeProvider) PlaceCall(_ context.Context, to, twiml string) Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.voice {
		return Result{Outcome: Rejected, Code: CodeVoiceUnsupported}
	}
	f.calls = append(f.calls, FakeCall{Kind: "voice", To: to, Body: twiml})
	return f.next(&f.callScript, "CA")
}

// next pops a scripted result or makes the default one. Called with f.mu held.
func (f *FakeProvider) next(script *[]Result, refPrefix string) Result {
	if len(*script) > 0 {
		r := (*script)[0]
		*script = (*script)[1:]
		return r
	}
	f.nextRefSeed++
	return Result{Outcome: Sent, HTTPStatus: 201, Ref: fmt.Sprintf("%sfake%d", refPrefix, f.nextRefSeed)}
}
