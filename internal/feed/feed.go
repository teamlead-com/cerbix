// Package feed renders a small syndication feed (a status page's incidents) in
// three formats — JSON Feed v1, RSS 2.0, and Atom — using only the standard
// library, so it stays dependency-free and hermetically testable.
package feed

import (
	"encoding/json"
	"encoding/xml"
	"time"
)

// Item is one entry in a feed (an incident).
type Item struct {
	ID        string
	Title     string
	Summary   string
	Link      string
	Published time.Time
	Updated   time.Time
}

// Feed is a renderable syndication feed.
type Feed struct {
	Title       string
	Link        string // the status page URL
	FeedLink    string // this feed's own URL
	Description string
	Updated     time.Time
	Items       []Item
}

// Content types for each format.
const (
	TypeJSON = "application/feed+json; charset=utf-8"
	TypeRSS  = "application/rss+xml; charset=utf-8"
	TypeAtom = "application/atom+xml; charset=utf-8"
)

// --- JSON Feed v1 (https://jsonfeed.org/version/1) ---

type jsonFeed struct {
	Version     string     `json:"version"`
	Title       string     `json:"title"`
	HomePageURL string     `json:"home_page_url,omitempty"`
	FeedURL     string     `json:"feed_url,omitempty"`
	Description string     `json:"description,omitempty"`
	Items       []jsonItem `json:"items"`
}

type jsonItem struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	ContentText   string `json:"content_text"`
	URL           string `json:"url,omitempty"`
	DatePublished string `json:"date_published,omitempty"`
	DateModified  string `json:"date_modified,omitempty"`
}

// JSON renders the feed as JSON Feed v1.
func (f Feed) JSON() ([]byte, string, error) {
	jf := jsonFeed{
		Version:     "https://jsonfeed.org/version/1",
		Title:       f.Title,
		HomePageURL: f.Link,
		FeedURL:     f.FeedLink,
		Description: f.Description,
		Items:       make([]jsonItem, 0, len(f.Items)),
	}
	for _, it := range f.Items {
		jf.Items = append(jf.Items, jsonItem{
			ID:            it.ID,
			Title:         it.Title,
			ContentText:   it.Summary,
			URL:           it.Link,
			DatePublished: rfc3339(it.Published),
			DateModified:  rfc3339(it.Updated),
		})
	}
	b, err := json.MarshalIndent(jf, "", "  ")
	if err != nil {
		return nil, "", err
	}
	return b, TypeJSON, nil
}

// --- RSS 2.0 ---

type rss struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Updated     string    `xml:"lastBuildDate,omitempty"`
	Items       []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link,omitempty"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate,omitempty"`
	Description string `xml:"description"`
}

// RSS renders the feed as RSS 2.0.
func (f Feed) RSS() ([]byte, string, error) {
	ch := rssChannel{
		Title: f.Title, Link: f.Link, Description: f.Description,
		Updated: rfc1123z(f.Updated),
		Items:   make([]rssItem, 0, len(f.Items)),
	}
	for _, it := range f.Items {
		ch.Items = append(ch.Items, rssItem{
			Title: it.Title, Link: it.Link, GUID: it.ID,
			PubDate: rfc1123z(it.Published), Description: it.Summary,
		})
	}
	return marshalXML(rss{Version: "2.0", Channel: ch}, TypeRSS)
}

// --- Atom ---

type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	Title   string      `xml:"title"`
	ID      string      `xml:"id"`
	Updated string      `xml:"updated"`
	Links   []atomLink  `xml:"link"`
	Entries []atomEntry `xml:"entry"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr,omitempty"`
}

type atomEntry struct {
	Title   string     `xml:"title"`
	ID      string     `xml:"id"`
	Updated string     `xml:"updated"`
	Links   []atomLink `xml:"link"`
	Summary string     `xml:"summary"`
}

// Atom renders the feed as an Atom document.
func (f Feed) Atom() ([]byte, string, error) {
	af := atomFeed{
		Title: f.Title, ID: firstNonEmpty(f.Link, f.Title), Updated: rfc3339(f.Updated),
		Links:   []atomLink{{Href: f.Link}, {Href: f.FeedLink, Rel: "self"}},
		Entries: make([]atomEntry, 0, len(f.Items)),
	}
	for _, it := range f.Items {
		af.Entries = append(af.Entries, atomEntry{
			Title: it.Title, ID: firstNonEmpty(it.Link, it.ID), Updated: rfc3339(it.Updated),
			Links:   []atomLink{{Href: it.Link}},
			Summary: it.Summary,
		})
	}
	return marshalXML(af, TypeAtom)
}

func marshalXML(v any, contentType string) ([]byte, string, error) {
	b, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, "", err
	}
	return append([]byte(xml.Header), b...), contentType, nil
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func rfc1123z(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC1123Z)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
