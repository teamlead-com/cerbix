package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/feed"
	"github.com/teamlead-com/cerbix/internal/store"
)

const feedMaxItems = 20

// renderFeedAuthed serves a status page's incident feed to an org member.
func (h *Handler) renderFeedAuthed(w http.ResponseWriter, r *http.Request) {
	sp, ok := h.statusPageAccess(w, r, false)
	if !ok {
		return
	}
	h.writeFeed(w, r, sp)
}

// renderFeedPublic serves the incident feed without a session, enforcing the same
// visibility gate as the public render.
func (h *Handler) renderFeedPublic(w http.ResponseWriter, r *http.Request) {
	sp, err := h.store.GetStatusPageBySlug(r.Context(), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_status_page_by_slug", err)
		return
	}
	switch sp.Visibility {
	case domain.VisibilityPublic:
	case domain.VisibilityUnlisted:
		if tok := r.URL.Query().Get("token"); tok == "" || tok != sp.UnlistedToken {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
	default:
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	h.writeFeed(w, r, sp)
}

// writeFeed assembles and writes the page's incident feed in the requested format
// (?format=rss|atom|json, default rss).
func (h *Handler) writeFeed(w http.ResponseWriter, r *http.Request, sp domain.StatusPage) {
	f, err := h.buildFeed(r, sp)
	if err != nil {
		h.serverError(w, "build_feed", err)
		return
	}
	var (
		body []byte
		ct   string
	)
	switch r.URL.Query().Get("format") {
	case "json":
		body, ct, err = f.JSON()
	case "atom":
		body, ct, err = f.Atom()
	default:
		body, ct, err = f.RSS()
	}
	if err != nil {
		h.serverError(w, "render_feed", err)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// buildFeed gathers the page's incidents (across the projects its components draw
// from, plus its own project) and maps them to feed items, newest first.
func (h *Handler) buildFeed(r *http.Request, sp domain.StatusPage) (feed.Feed, error) {
	ctx := r.Context()
	projects, err := h.statusPageProjectIDs(ctx, sp)
	if err != nil {
		return feed.Feed{}, err
	}
	var incidents []domain.Incident
	for pid := range projects {
		list, err := h.store.ListIncidentsByProject(ctx, pid)
		if err != nil {
			return feed.Feed{}, err
		}
		incidents = append(incidents, list...)
	}
	sort.Slice(incidents, func(i, j int) bool { return incidents[i].StartedAt.After(incidents[j].StartedAt) })
	if len(incidents) > feedMaxItems {
		incidents = incidents[:feedMaxItems]
	}

	base := baseURL(r)
	pageURL := base + "/status/" + sp.Slug
	f := feed.Feed{
		Title:       sp.Title + " — Incidents",
		Link:        pageURL,
		FeedLink:    pageURL + "/feed",
		Description: "Incident history for " + sp.Title,
		Updated:     time.Now(),
		Items:       make([]feed.Item, 0, len(incidents)),
	}
	for _, inc := range incidents {
		f.Items = append(f.Items, feed.Item{
			ID:        inc.ID,
			Title:     inc.Title,
			Summary:   fmt.Sprintf("status: %s · impact: %s", inc.Status, inc.Impact),
			Link:      pageURL + "#" + inc.ID,
			Published: inc.StartedAt,
			Updated:   inc.UpdatedAt,
		})
	}
	return f, nil
}

// statusPageProjectIDs returns the set of project ids a page draws incidents from.
//
// It is `store.StatusPageProjectIDs` and nothing else. This used to be its own walk — the page's own
// project plus `components → monitors`, one `GetMonitor` per component — which meant a service-backed
// component contributed nothing, so a Service-only page rendered incidents its own feed did not
// carry. One owner, asked by the render, the feed and the subscriber fan-out alike (D-0180).
func (h *Handler) statusPageProjectIDs(ctx context.Context, sp domain.StatusPage) (map[string]struct{}, error) {
	ids, err := h.store.StatusPageProjectIDs(ctx, sp.ID)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set, nil
}

// baseURL reconstructs the request's external base (scheme://host), honoring a
// reverse proxy's X-Forwarded-Proto.
func baseURL(r *http.Request) string {
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
