package prober

import "testing"

func TestEvaluateNumeric(t *testing.T) {
	v := Values{Status: 200, ResponseTimeMS: 120, Connected: true}
	cases := []struct {
		cond string
		want bool
	}{
		{"[STATUS] == 200", true},
		{"[STATUS] != 500", true},
		{"[STATUS] < 300", true},
		{"[STATUS] >= 201", false},
		{"[RESPONSE_TIME] < 500", true},
		{"[RESPONSE_TIME] > 500", false},
		{"[CONNECTED] == 1", true},
		{"[CONNECTED] == true", true},
	}
	for _, c := range cases {
		ok, _, err := EvaluateAll([]string{c.cond}, v)
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.cond, err)
			continue
		}
		if ok != c.want {
			t.Errorf("%q = %v, want %v", c.cond, ok, c.want)
		}
	}
}

func TestEvaluateBody(t *testing.T) {
	v := Values{Body: `{"status":"UP","n":5}`, Connected: true}
	cases := []struct {
		cond string
		want bool
	}{
		{`[BODY] contains "UP"`, true},
		{`[BODY] contains "DOWN"`, false},
		{`[BODY] matches "status.*UP"`, true},
		{`[BODY] != "x"`, true},
	}
	for _, c := range cases {
		ok, _, err := EvaluateAll([]string{c.cond}, v)
		if err != nil || ok != c.want {
			t.Errorf("%q = %v (err %v), want %v", c.cond, ok, err, c.want)
		}
	}
}

func TestEvaluateAllShortCircuitsOnFailure(t *testing.T) {
	v := Values{Status: 200, ResponseTimeMS: 1000}
	ok, failed, err := EvaluateAll([]string{"[STATUS] == 200", "[RESPONSE_TIME] < 500"}, v)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || failed != "[RESPONSE_TIME] < 500" {
		t.Fatalf("expected failure on response time, got ok=%v failed=%q", ok, failed)
	}
}

func TestEvaluateErrors(t *testing.T) {
	v := Values{Status: 200}
	for _, cond := range []string{
		"STATUS == 200",    // no placeholder
		"[STATUS 200",      // unterminated
		"[STATUS] 200",     // no operator/value split
		"[STATUS] === 200", // bad operator
		"[STATUS] == abc",  // non-int
		"[UNKNOWN] == 1",   // unknown placeholder
		`[BODY] weird "x"`, // bad string operator
	} {
		if _, _, err := EvaluateAll([]string{cond}, v); err == nil {
			t.Errorf("%q: expected error", cond)
		}
	}
}
