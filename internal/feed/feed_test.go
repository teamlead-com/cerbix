package feed

import (
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func sampleFeed() Feed {
	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	return Feed{
		Title:       "Acme Status",
		Link:        "https://status.example/acme",
		FeedLink:    "https://status.example/acme/feed",
		Description: "Incident history",
		Updated:     t0.Add(time.Hour),
		Items: []Item{
			{ID: "inc-1", Title: "API latency", Summary: "resolved · minor", Link: "https://status.example/acme#inc-1", Published: t0, Updated: t0.Add(time.Hour)},
		},
	}
}

func TestJSONFeed(t *testing.T) {
	b, ct, err := sampleFeed().JSON()
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if ct != TypeJSON {
		t.Fatalf("content-type = %q", ct)
	}
	var jf struct {
		Version string `json:"version"`
		Title   string `json:"title"`
		Items   []struct {
			ID            string `json:"id"`
			Title         string `json:"title"`
			ContentText   string `json:"content_text"`
			DatePublished string `json:"date_published"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &jf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(jf.Version, "jsonfeed.org") || jf.Title != "Acme Status" {
		t.Fatalf("bad header: %+v", jf)
	}
	if len(jf.Items) != 1 || jf.Items[0].ID != "inc-1" || jf.Items[0].ContentText != "resolved · minor" {
		t.Fatalf("bad items: %+v", jf.Items)
	}
	if jf.Items[0].DatePublished == "" {
		t.Fatal("date_published should be set")
	}
}

func TestRSSFeed(t *testing.T) {
	b, ct, err := sampleFeed().RSS()
	if err != nil {
		t.Fatalf("rss: %v", err)
	}
	if ct != TypeRSS {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.HasPrefix(string(b), xml.Header) {
		t.Fatal("missing XML header")
	}
	var doc struct {
		Channel struct {
			Title string `xml:"title"`
			Items []struct {
				Title string `xml:"title"`
				GUID  string `xml:"guid"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Channel.Title != "Acme Status" || len(doc.Channel.Items) != 1 || doc.Channel.Items[0].GUID != "inc-1" {
		t.Fatalf("bad rss: %+v", doc)
	}
}

func TestAtomFeed(t *testing.T) {
	b, ct, err := sampleFeed().Atom()
	if err != nil {
		t.Fatalf("atom: %v", err)
	}
	if ct != TypeAtom {
		t.Fatalf("content-type = %q", ct)
	}
	var doc struct {
		Title   string `xml:"title"`
		Entries []struct {
			Title string `xml:"title"`
			ID    string `xml:"id"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Title != "Acme Status" || len(doc.Entries) != 1 || doc.Entries[0].Title != "API latency" {
		t.Fatalf("bad atom: %+v", doc)
	}
}

func TestEmptyAndZeroTimes(t *testing.T) {
	f := Feed{Title: "Empty"}
	for _, render := range []func() ([]byte, string, error){f.JSON, f.RSS, f.Atom} {
		b, _, err := render()
		if err != nil {
			t.Fatalf("render empty: %v", err)
		}
		if len(b) == 0 {
			t.Fatal("empty render produced no bytes")
		}
	}
}
