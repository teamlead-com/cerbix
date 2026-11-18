package telephony

import (
	"encoding/xml"
	"reflect"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

const (
	exampleBase   = "https://cerbix.example.com"
	longMonitor   = "payments-api-eu-central-1-primary-card-authorisation-gateway"
	longProject   = "production-europe-central-cluster-tenant-acme-retail-division-b"
	longBase      = "https://monitoring.internal.example.com/platform/reliability/cerbix"
	longerBase    = "https://monitoring.internal.example.com/platform/reliability/oncall/cerbix"
	monitorRef    = "X7K2P9QM"
	serviceRef    = "7MQ4H2ZD"
	cyrillicName  = "Платёжный шлюз"
	cyrillicProj  = "прод"
	translit      = EncodingTransliterate
	unicodeEnc    = EncodingUnicode
	noteLinkOmit  = domain.PhoneNoteLinkOmitted
	defaultMaxSeg = 2
)

func monitorDown(name, project, ref, base string) PhoneAlert {
	return PhoneAlert{Kind: KindMonitorDown, SubjectName: name, ProjectName: project, IncidentRef: ref, PublicBaseURL: base}
}

// TestRenderSMSGolden pins the texts and counts of the owner-approved mock §6
// (docs/design/mock-phone-alerts-a.html), verbatim.
func TestRenderSMSGolden(t *testing.T) {
	cases := []struct {
		name        string
		alert       PhoneAlert
		enc         Encoding
		max         int
		wantText    string
		wantUnits   int
		wantSegs    int
		wantUnicode bool
		wantNotes   []string
	}{
		{
			name:  "monitor down",
			alert: monitorDown("payments-api", "prod", monitorRef, exampleBase),
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] DOWN payments-api (prod) - incident X7K2P9QM https://cerbix.example.com/i/X7K2P9QM",
			wantUnits: 93, wantSegs: 1,
		},
		{
			name:  "monitor recovered",
			alert: PhoneAlert{Kind: KindMonitorRecovered, SubjectName: "payments-api", ProjectName: "prod", IncidentRef: monitorRef, PublicBaseURL: exampleBase},
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] RECOVERED payments-api (prod) - incident X7K2P9QM https://cerbix.example.com/i/X7K2P9QM",
			wantUnits: 98, wantSegs: 1,
		},
		{
			name:  "service down",
			alert: PhoneAlert{Kind: KindServiceHealth, State: "down", SubjectName: "checkout", IsService: true, ProjectName: "prod", IncidentRef: serviceRef, PublicBaseURL: exampleBase},
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] DOWN service checkout (prod) - incident 7MQ4H2ZD https://cerbix.example.com/i/7MQ4H2ZD",
			wantUnits: 97, wantSegs: 1,
		},
		{
			name:  "service degraded",
			alert: PhoneAlert{Kind: KindServiceHealth, State: "degraded", SubjectName: "checkout", IsService: true, ProjectName: "prod", IncidentRef: serviceRef, PublicBaseURL: exampleBase},
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] DEGRADED service checkout (prod) - incident 7MQ4H2ZD https://cerbix.example.com/i/7MQ4H2ZD",
			wantUnits: 101, wantSegs: 1,
		},
		{
			name:  "service unknown",
			alert: PhoneAlert{Kind: KindServiceHealth, State: "unknown", SubjectName: "checkout", IsService: true, ProjectName: "prod", IncidentRef: serviceRef, PublicBaseURL: exampleBase},
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] UNKNOWN service checkout (prod) - incident 7MQ4H2ZD https://cerbix.example.com/i/7MQ4H2ZD",
			wantUnits: 100, wantSegs: 1,
		},
		{
			name:  "service recovered",
			alert: PhoneAlert{Kind: KindServiceRecovered, SubjectName: "checkout", IsService: true, ProjectName: "prod", IncidentRef: serviceRef, PublicBaseURL: exampleBase},
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] RECOVERED service checkout (prod) - incident 7MQ4H2ZD https://cerbix.example.com/i/7MQ4H2ZD",
			wantUnits: 102, wantSegs: 1,
		},
		{
			name:  "escalation step",
			alert: PhoneAlert{Kind: KindEscalationStep, Step: 2, SubjectName: "payments-api", ProjectName: "prod", IncidentRef: monitorRef, PublicBaseURL: exampleBase},
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] ESCALATION step 2: payments-api (prod) - incident X7K2P9QM https://cerbix.example.com/i/X7K2P9QM",
			wantUnits: 107, wantSegs: 1,
		},
		{
			name:  "escalation repeat of a service",
			alert: PhoneAlert{Kind: KindEscalationStep, Step: 3, Repeat: true, SubjectName: "checkout", IsService: true, ProjectName: "prod", IncidentRef: serviceRef, PublicBaseURL: exampleBase},
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] ESCALATION step 3 (repeat): service checkout (prod) - incident 7MQ4H2ZD https://cerbix.example.com/i/7MQ4H2ZD",
			wantUnits: 120, wantSegs: 1,
		},
		{
			name:  "monitor burn",
			alert: PhoneAlert{Kind: KindMonitorBurn, BurnRate: 14.4, Window: "1h", SubjectName: "payments-api", ProjectName: "prod", IncidentRef: monitorRef, PublicBaseURL: exampleBase},
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] BUDGET BURN payments-api (prod) - burn rate 14.4x over 1h",
			wantUnits: 68, wantSegs: 1,
		},
		{
			name:  "service burn",
			alert: PhoneAlert{Kind: KindServiceBurn, BurnRate: 6, Window: "6h", SubjectName: "checkout", IsService: true, ProjectName: "prod"},
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] BUDGET BURN service checkout (prod) - burn rate 6x over 6h",
			wantUnits: 69, wantSegs: 1,
		},
		{
			name:  "6a no incident ref",
			alert: monitorDown("payments-api", "prod", "", exampleBase),
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] DOWN payments-api (prod)",
			wantUnits: 35, wantSegs: 1,
		},
		{
			name:  "6c no public URL",
			alert: monitorDown("payments-api", "prod", monitorRef, ""),
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] DOWN payments-api (prod) - incident X7K2P9QM",
			wantUnits: 55, wantSegs: 1,
		},
		{
			name:  "6d unicode, one segment: the link goes first, the names then fit whole",
			alert: monitorDown(cyrillicName, cyrillicProj, monitorRef, exampleBase),
			enc:   unicodeEnc, max: 1,
			wantText:  "[cerbix] DOWN Платёжный шлюз (прод) - incident X7K2P9QM",
			wantUnits: 55, wantSegs: 1, wantUnicode: true, wantNotes: []string{noteLinkOmit},
		},
		{
			name:  "6e the name is shortened",
			alert: monitorDown(longMonitor, longProject, monitorRef, exampleBase),
			enc:   translit, max: 1,
			wantText:  "[cerbix] DOWN payments-api-eu-c... (production-europe-central-cluster-tenant-acme-retail-division-b) - incident X7K2P9QM https://cerbix.example.com/i/X7K2P9QM",
			wantUnits: 160, wantSegs: 1,
		},
		{
			name:  "6f then the project",
			alert: PhoneAlert{Kind: KindEscalationStep, Step: 2, SubjectName: longMonitor, ProjectName: longProject, IncidentRef: monitorRef, PublicBaseURL: longBase},
			enc:   translit, max: 1,
			wantText:  "[cerbix] ESCALATION step 2: payments-... (production-eu...) - incident X7K2P9QM https://monitoring.internal.example.com/platform/reliability/cerbix/i/X7K2P9QM",
			wantUnits: 160, wantSegs: 1,
		},
		{
			name:  "6g then the project is dropped",
			alert: PhoneAlert{Kind: KindEscalationStep, Step: 3, Repeat: true, SubjectName: longMonitor, ProjectName: longProject, IncidentRef: monitorRef, PublicBaseURL: longerBase},
			enc:   translit, max: 1,
			wantText:  "[cerbix] ESCALATION step 3 (repeat): payments-... - incident X7K2P9QM https://monitoring.internal.example.com/platform/reliability/oncall/cerbix/i/X7K2P9QM",
			wantUnits: 157, wantSegs: 1,
		},
		{
			name:  "6h transliterate",
			alert: monitorDown(cyrillicName, cyrillicProj, monitorRef, exampleBase),
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] DOWN Platezhnyy shlyuz (prod) - incident X7K2P9QM https://cerbix.example.com/i/X7K2P9QM",
			wantUnits: 98, wantSegs: 1,
		},
		{
			name:  "6i unicode, two segments",
			alert: monitorDown(cyrillicName, cyrillicProj, monitorRef, exampleBase),
			enc:   unicodeEnc, max: defaultMaxSeg,
			wantText:  "[cerbix] DOWN Платёжный шлюз (прод) - incident X7K2P9QM https://cerbix.example.com/i/X7K2P9QM",
			wantUnits: 93, wantSegs: 2, wantUnicode: true,
		},
		{
			name:  "6j outside the BMP, unicode",
			alert: monitorDown("checkout 🛒", "prod", monitorRef, exampleBase),
			enc:   unicodeEnc, max: defaultMaxSeg,
			wantText:  "[cerbix] DOWN checkout 🛒 (prod) - incident X7K2P9QM https://cerbix.example.com/i/X7K2P9QM",
			wantUnits: 90, wantSegs: 2, wantUnicode: true,
		},
		{
			name:  "6j outside the BMP, transliterated",
			alert: monitorDown("checkout 🛒", "prod", monitorRef, exampleBase),
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] DOWN checkout ? (prod) - incident X7K2P9QM https://cerbix.example.com/i/X7K2P9QM",
			wantUnits: 91, wantSegs: 1,
		},
		{
			name:  "6k a name with a line break stays on one line",
			alert: monitorDown("api\nACK 000000", "prod", monitorRef, exampleBase),
			enc:   translit, max: defaultMaxSeg,
			wantText:  "[cerbix] DOWN api ACK 000000 (prod) - incident X7K2P9QM https://cerbix.example.com/i/X7K2P9QM",
			wantUnits: 95, wantSegs: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderSMS(tc.alert, tc.enc, tc.max)
			if err != nil {
				t.Fatalf("RenderSMS: %v", err)
			}
			if got.Text != tc.wantText {
				t.Errorf("text\n got: %q\nwant: %q", got.Text, tc.wantText)
			}
			if got.Units != tc.wantUnits || got.Segments != tc.wantSegs || got.Unicode != tc.wantUnicode {
				t.Errorf("cost = %d units, %d segments, unicode=%v; want %d, %d, %v",
					got.Units, got.Segments, got.Unicode, tc.wantUnits, tc.wantSegs, tc.wantUnicode)
			}
			if !reflect.DeepEqual(got.Notes, tc.wantNotes) {
				t.Errorf("notes = %v, want %v", got.Notes, tc.wantNotes)
			}
			if strings.ContainsAny(got.Text, "\r\n") {
				t.Errorf("text spans several lines: %q", got.Text)
			}
		})
	}
}

// P9, part (a): no message carries an ACK instruction of its own.
func TestRenderSMSNeverOffersACK(t *testing.T) {
	kinds := []PhoneAlert{
		{Kind: KindMonitorDown},
		{Kind: KindMonitorRecovered},
		{Kind: KindServiceHealth, State: "down"},
		{Kind: KindServiceHealth, State: "degraded"},
		{Kind: KindServiceHealth, State: "unknown"},
		{Kind: KindServiceRecovered},
		{Kind: KindEscalationStep, Step: 1},
		{Kind: KindEscalationStep, Step: 4, Repeat: true, IsService: true},
		{Kind: KindMonitorBurn, BurnRate: 2, Window: "3d"},
		{Kind: KindServiceBurn, BurnRate: 2, Window: "3d"},
	}
	for _, a := range kinds {
		a.SubjectName, a.ProjectName, a.IncidentRef, a.PublicBaseURL = "payments-api", "prod", monitorRef, exampleBase
		for _, enc := range []Encoding{translit, unicodeEnc} {
			sms, err := RenderSMS(a, enc, 1)
			if err != nil {
				t.Fatalf("%s: %v", a.Kind, err)
			}
			if strings.Contains(sms.Text, "ACK") || strings.Contains(strings.ToLower(sms.Text), "press") {
				t.Errorf("%s: message offers an action: %q", a.Kind, sms.Text)
			}
			if sms.Segments != 1 {
				t.Errorf("%s: %d segments for a short message", a.Kind, sms.Segments)
			}
		}
	}
}

func TestRenderSMSCostBoundaries(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		wantUnits int
		wantSegs  int
		wantUCS2  bool
	}{
		{"160 septets is one segment", strings.Repeat("a", 160), 160, 1, false},
		{"161 septets are two", strings.Repeat("a", 161), 161, 2, false},
		{"306 septets are two", strings.Repeat("a", 306), 306, 2, false},
		{"307 septets are three", strings.Repeat("a", 307), 307, 3, false},
		{"the extension table costs two septets each", "^{}\\[~]|€", 18, 1, false},
		{"80 extension characters overflow one segment", strings.Repeat("{", 81), 162, 2, false},
		{"a GSM-7 letter outside ASCII is one septet", "café Ñ ß", 8, 1, false},
		{"70 UTF-16 units are one segment", strings.Repeat("ж", 70), 70, 1, true},
		{"71 are two", strings.Repeat("ж", 71), 71, 2, true},
		{"134 are two", strings.Repeat("ж", 134), 134, 2, true},
		{"135 are three", strings.Repeat("ж", 135), 135, 3, true},
		{"a rune outside the BMP is two units", "🛒", 2, 1, true},
		{"one non-GSM rune makes the whole text UCS-2", "[cerbix] ж", 10, 1, true},
		{"the backtick is not GSM-7", "`", 1, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			units, segs, ucs2 := smsCost(tc.text)
			if units != tc.wantUnits || segs != tc.wantSegs || ucs2 != tc.wantUCS2 {
				t.Errorf("smsCost(%q) = %d, %d, %v; want %d, %d, %v", tc.text, units, segs, ucs2, tc.wantUnits, tc.wantSegs, tc.wantUCS2)
			}
		})
	}
}

func TestRenderSMSUnicodeEncodingStaysGSMWhenNamesAllowIt(t *testing.T) {
	sms, err := RenderSMS(monitorDown("café", "prod", monitorRef, exampleBase), unicodeEnc, 1)
	if err != nil {
		t.Fatal(err)
	}
	if sms.Text != "[cerbix] DOWN café (prod) - incident X7K2P9QM https://cerbix.example.com/i/X7K2P9QM" {
		t.Errorf("text = %q", sms.Text)
	}
	if sms.Unicode || sms.Units != 85 || sms.Segments != 1 {
		t.Errorf("cost = %d, %d, unicode=%v; want 85, 1, false", sms.Units, sms.Segments, sms.Unicode)
	}
}

func TestRenderSMSSeveralSegments(t *testing.T) {
	name := strings.Repeat("n", 200)
	sms, err := RenderSMS(monitorDown(name, "prod", monitorRef, exampleBase), translit, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sms.Text, name) {
		t.Errorf("a name that fits four segments was shortened: %q", sms.Text)
	}
	if sms.Units != 281 || sms.Segments != 2 {
		t.Errorf("cost = %d, %d; want 281, 2", sms.Units, sms.Segments)
	}
}

func TestRenderSMSShortNameIsNotPaddedOrShortened(t *testing.T) {
	// The link alone does not fit one UCS-2 segment, so it goes; the
	// 5-character name is under the floor and stays whole.
	sms, err := RenderSMS(monitorDown("шлюзы", "prod", monitorRef, longerBase), unicodeEnc, 1)
	if err != nil {
		t.Fatal(err)
	}
	if sms.Text != "[cerbix] DOWN шлюзы (prod) - incident X7K2P9QM" {
		t.Errorf("text = %q", sms.Text)
	}
	if !reflect.DeepEqual(sms.Notes, []string{noteLinkOmit}) {
		t.Errorf("notes = %v", sms.Notes)
	}
}

func TestRenderSMSFrameLongerThanTheBudget(t *testing.T) {
	// Without the link, at the name floor and without the project the text
	// still exceeds one UCS-2 segment: it is sent at its shortest, and the
	// cost reports the segments it really takes.
	a := PhoneAlert{Kind: KindEscalationStep, Step: 3, Repeat: true, IsService: true,
		SubjectName: "проверка-платежей", ProjectName: "продакшн", IncidentRef: serviceRef, PublicBaseURL: exampleBase}
	sms, err := RenderSMS(a, unicodeEnc, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := "[cerbix] ESCALATION step 3 (repeat): service проверка-... - incident 7MQ4H2ZD"
	if sms.Text != want {
		t.Errorf("text\n got: %q\nwant: %q", sms.Text, want)
	}
	if sms.Units != 77 || sms.Segments != 2 || !sms.Unicode {
		t.Errorf("cost = %d, %d, unicode=%v; want 77, 2, true", sms.Units, sms.Segments, sms.Unicode)
	}
	if !reflect.DeepEqual(sms.Notes, []string{noteLinkOmit}) {
		t.Errorf("notes = %v", sms.Notes)
	}
}

func TestRenderSMSBurnHasNoIncidentPart(t *testing.T) {
	sms, err := RenderSMS(PhoneAlert{Kind: KindMonitorBurn, BurnRate: 0.5, Window: "30d", SubjectName: "api", ProjectName: "prod", IncidentRef: monitorRef, PublicBaseURL: exampleBase}, translit, 1)
	if err != nil {
		t.Fatal(err)
	}
	if sms.Text != "[cerbix] BUDGET BURN api (prod) - burn rate 0.5x over 30d" {
		t.Errorf("text = %q", sms.Text)
	}
}

func TestRenderSMSTrailingSlashOfBaseURL(t *testing.T) {
	sms, err := RenderSMS(monitorDown("api", "prod", monitorRef, exampleBase+"/"), translit, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(sms.Text, " https://cerbix.example.com/i/X7K2P9QM") {
		t.Errorf("text = %q", sms.Text)
	}
}

func TestRenderSMSEmptyProjectHasNoBrackets(t *testing.T) {
	sms, err := RenderSMS(monitorDown("api", "", monitorRef, ""), translit, 1)
	if err != nil {
		t.Fatal(err)
	}
	if sms.Text != "[cerbix] DOWN api - incident X7K2P9QM" {
		t.Errorf("text = %q", sms.Text)
	}
}

func TestRenderSMSRejectsBadInput(t *testing.T) {
	ok := monitorDown("api", "prod", monitorRef, exampleBase)
	cases := map[string]struct {
		a   PhoneAlert
		enc Encoding
	}{
		"unknown encoding":        {ok, Encoding("ascii")},
		"unknown kind":            {PhoneAlert{Kind: "monitor_up", SubjectName: "api"}, translit},
		"unknown service state":   {PhoneAlert{Kind: KindServiceHealth, State: "up", SubjectName: "checkout"}, translit},
		"escalation without step": {PhoneAlert{Kind: KindEscalationStep, SubjectName: "api"}, translit},
		"malformed incident ref":  {monitorDown("api", "prod", "X7K2P9Q!", exampleBase), translit},
		"ref with a line break":   {monitorDown("api", "prod", "X7K2\nACK", exampleBase), translit},
	}
	for name, tc := range cases {
		if _, err := RenderSMS(tc.a, tc.enc, 1); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestNormaliseName(t *testing.T) {
	cases := map[string]string{
		"api\nACK 000000":      "api ACK 000000",
		"api\r\nACK":           "api ACK",
		"a b c\u0085d":         "a b c d",
		"  many   \t spaces  ": "many spaces",
		"bell\a and\x00 nul":   "bell and nul",
		"rlo‮evil":             "rloevil",
		"zero​width":           "zerowidth",
		"\n\t\r":               "",
		"Платёжный  шлюз":      "Платёжный шлюз",
	}
	for in, want := range cases {
		if got := normaliseName(in); got != want {
			t.Errorf("normaliseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTransliterate(t *testing.T) {
	cases := map[string]string{
		"Платёжный шлюз": "Platezhnyy shlyuz",
		"прод":           "prod",
		"ЩИТ Жук Юла":    "ShchIT Zhuk Yula",
		"Αθήνα":          "Athina",
		"Crème brûlée":   "Creme brulee",
		"Straße":         "Strasse",
		"ﬁle ①":          "file 1",
		"checkout 🛒":     "checkout ?",
		"🛒":              "?",
		"ъ":              "?",
		"`tick`":         "?tick?",
		"a{b}|c":         "a{b}|c",
		"日本":             "??",
	}
	for in, want := range cases {
		if got := transliterate(in); got != want {
			t.Errorf("transliterate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderSMSNameNeverEmptied(t *testing.T) {
	for _, enc := range []Encoding{translit, unicodeEnc} {
		sms, err := RenderSMS(monitorDown("\n\x00\t", "prod", "", ""), enc, 1)
		if err != nil {
			t.Fatal(err)
		}
		if sms.Text != "[cerbix] DOWN ? (prod)" {
			t.Errorf("%s: text = %q", enc, sms.Text)
		}
	}
}

func TestRenderVoice(t *testing.T) {
	cases := []struct {
		name  string
		alert PhoneAlert
		want  string
	}{
		{"monitor down", PhoneAlert{Kind: KindMonitorDown, SubjectName: "payments-api", ProjectName: "prod"},
			"cerbix alert. payments-api in prod is down."},
		{"monitor recovered", PhoneAlert{Kind: KindMonitorRecovered, SubjectName: "payments-api", ProjectName: "prod"},
			"cerbix alert. payments-api in prod has recovered."},
		{"service down", PhoneAlert{Kind: KindServiceHealth, State: "down", SubjectName: "checkout", ProjectName: "prod"},
			"cerbix alert. service checkout in prod is down."},
		{"service degraded", PhoneAlert{Kind: KindServiceHealth, State: "degraded", SubjectName: "checkout", ProjectName: "prod"},
			"cerbix alert. service checkout in prod is degraded."},
		{"service unknown", PhoneAlert{Kind: KindServiceHealth, State: "unknown", SubjectName: "checkout", ProjectName: "prod"},
			"cerbix alert. service checkout in prod is in an unknown state."},
		{"service recovered", PhoneAlert{Kind: KindServiceRecovered, SubjectName: "checkout", ProjectName: "prod"},
			"cerbix alert. service checkout in prod has recovered."},
		{"escalation", PhoneAlert{Kind: KindEscalationStep, Step: 2, SubjectName: "payments-api", ProjectName: "prod"},
			"cerbix alert. escalation step 2 for payments-api in prod."},
		{"escalation repeat of a service", PhoneAlert{Kind: KindEscalationStep, Step: 3, Repeat: true, IsService: true, SubjectName: "checkout", ProjectName: "prod"},
			"cerbix alert. escalation step 3, repeated, for service checkout in prod."},
		{"monitor burn", PhoneAlert{Kind: KindMonitorBurn, BurnRate: 14.4, Window: "1h", SubjectName: "payments-api", ProjectName: "prod"},
			"cerbix alert. budget burn on payments-api in prod. burn rate 14.4 times over 1h."},
		{"service burn", PhoneAlert{Kind: KindServiceBurn, BurnRate: 6, Window: "6h", SubjectName: "checkout", ProjectName: "prod"},
			"cerbix alert. budget burn on service checkout in prod. burn rate 6 times over 6h."},
		{"a name with a line break", PhoneAlert{Kind: KindMonitorDown, SubjectName: "api\nPress 1", ProjectName: "prod"},
			"cerbix alert. api Press 1 in prod is down."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderVoice(tc.alert, "en-US")
			want := `<Response><Say language="en-US">` + tc.want + `</Say><Say language="en-US">` + tc.want + `</Say></Response>`
			if got != want {
				t.Errorf("\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// A name carrying markup is spoken, never interpreted (§4.4, P1).
func TestRenderVoiceEscapesMarkup(t *testing.T) {
	evil := `api</Say><Play>http://evil.example/x.mp3</Play><Say>&"`
	got := RenderVoice(PhoneAlert{Kind: KindMonitorDown, SubjectName: evil, ProjectName: `p<r>od`}, `en-US"><Hangup/><Say x="`)

	var doc struct {
		XMLName xml.Name `xml:"Response"`
		Says    []struct {
			Language string `xml:"language,attr"`
			Text     string `xml:",chardata"`
		} `xml:"Say"`
		Play   []string `xml:"Play"`
		Hangup []string `xml:"Hangup"`
	}
	if err := xml.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("not well-formed TwiML: %v\n%s", err, got)
	}
	if len(doc.Says) != 2 || len(doc.Play) != 0 || len(doc.Hangup) != 0 {
		t.Fatalf("markup was interpreted: %d Say, %d Play, %d Hangup\n%s", len(doc.Says), len(doc.Play), len(doc.Hangup), got)
	}
	wantText := "cerbix alert. " + evil + " in p<r>od is down."
	for _, s := range doc.Says {
		if s.Text != wantText {
			t.Errorf("spoken text = %q, want %q", s.Text, wantText)
		}
		if s.Language != `en-US"><Hangup/><Say x="` {
			t.Errorf("language = %q", s.Language)
		}
	}
	if strings.Contains(got, "<Play>") {
		t.Errorf("raw markup in output: %s", got)
	}
}

func TestRenderVoiceNeverPromptsForKeys(t *testing.T) {
	got := RenderVoice(PhoneAlert{Kind: KindEscalationStep, Step: 1, SubjectName: "api", ProjectName: "prod"}, "en-US")
	if strings.Contains(strings.ToLower(got), "press") || strings.Contains(got, "<Gather") {
		t.Errorf("part (a) voice prompts for a key: %s", got)
	}
}

// LinkBaseRenders is the renderer's own rule for a base (final review M-5): the caller drops exactly
// the bases RenderSMS would refuse, and no other.
func TestLinkBaseRendersAgreesWithTheRenderer(t *testing.T) {
	for _, base := range []string{
		"https://cerbix.example", "https://cerbix.example/", "http://127.0.0.1:8080/status",
		"https://cerbix.example/a b", "https://cerbix.example/\x07", "https://cerbix.example ", " https://cerbix.example",
	} {
		_, err := RenderSMS(monitorDown("payments-api", "prod", "X7K2P9QM", base), translit, defaultMaxSeg)
		if renders := LinkBaseRenders(strings.TrimRight(base, "/")); renders != (err == nil) {
			t.Errorf("base %q: LinkBaseRenders = %v, RenderSMS error = %v", base, renders, err)
		}
	}
}
