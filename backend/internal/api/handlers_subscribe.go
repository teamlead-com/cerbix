package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"git.example.com/monitoring/cerbix/internal/domain"
	"git.example.com/monitoring/cerbix/internal/store"
)

// subscribe registers an email subscriber to a public/unlisted status page and
// sends a double opt-in confirmation email. Internal pages are hidden (404).
func (h *Handler) subscribe(w http.ResponseWriter, r *http.Request) {
	if h.mailer == nil {
		writeError(w, http.StatusServiceUnavailable, "email subscriptions are not enabled")
		return
	}
	sp, err := h.store.GetStatusPageBySlug(r.Context(), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_status_page_by_slug", err)
		return
	}
	// Same visibility gate as the public render: public open, unlisted needs the
	// token, internal hidden.
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

	var body struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	sub := domain.Subscriber{StatusPageID: sp.ID, Email: strings.TrimSpace(body.Email), ConfirmToken: randomToken()}
	if err := sub.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := h.store.CreateSubscriber(r.Context(), sub)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not subscribe")
		return
	}
	if created.ConfirmedAt != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "already_subscribed"})
		return
	}
	base := h.mailer.BaseURL()
	confirm := fmt.Sprintf("%s/status/%s?confirm=%s", base, sp.Slug, created.ConfirmToken)
	unsub := fmt.Sprintf("%s/status/%s?unsubscribe=%s", base, sp.Slug, created.ConfirmToken)
	msg := fmt.Sprintf("Confirm your subscription to %s status updates:\n\n%s\n\nIf you didn't request this, ignore this email or unsubscribe:\n%s\n", sp.Title, confirm, unsub)
	payload, err := json.Marshal(domain.SubscriberConfirm{
		To: created.Email, Subject: "Confirm your subscription to " + sp.Title, Body: msg,
	})
	if err != nil {
		h.serverError(w, "encode_confirm_email", err)
		return
	}
	// Queue the confirmation email rather than sending it inline: a slow or failing
	// SMTP must never block — or fail — the subscribe request. The outbox worker
	// delivers it with retry/backoff, so a transient mail outage self-heals.
	if err := h.store.EnqueueOutbox(r.Context(), domain.TopicSubscriberConfirm, payload); err != nil {
		h.logger.Error("api_error", "op", "queue_confirm_email", "error", err.Error())
		writeError(w, http.StatusServiceUnavailable, "could not send confirmation right now, please try again later")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
}

// confirmSubscription confirms a subscriber by token (idempotent).
func (h *Handler) confirmSubscription(w http.ResponseWriter, r *http.Request) {
	if _, err := h.store.ConfirmSubscriber(r.Context(), r.PathValue("token")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		h.serverError(w, "confirm_subscriber", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "confirmed"})
}

// unsubscribe removes a subscriber by token.
func (h *Handler) unsubscribe(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteSubscriberByToken(r.Context(), r.PathValue("token")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		h.serverError(w, "delete_subscriber", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
