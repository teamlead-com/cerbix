package prober

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// The redirect rule (sec-hardening-2 H-13, invariant S13). net/http re-copies the FIRST request's
// headers onto every hop before CheckRedirect runs, re-sends a 307/308 body from GetBody, drops only
// Authorization and Cookie on a host change, does not treat https → http on one host as a change,
// and adds a Referer carrying the previous URL. So a comparison with the previous hop — what the
// canary did — loses on a two-hop chain: the header deleted on hop 1 comes back on hop 2, whose
// origin equals hop 1's. The one predicate here compares every hop with the first request
// (`via[0]`), and leaving that origin is sticky.

// runnerMaxRedirects is net/http's own limit, kept for the Runner's clients.
const runnerMaxRedirects = 10

// requestOrigin is the lower-cased scheme, host and port (the port defaulted by scheme).
func requestOrigin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// strictUpgrade reports the one hop that does not count as leaving for the Runner: the first
// request's own host moving from http on port 80 to https on port 443.
func strictUpgrade(first, hop *url.URL) bool {
	return requestOrigin(first) == "http://"+net.JoinHostPort(strings.ToLower(first.Hostname()), "80") &&
		requestOrigin(hop) == "https://"+net.JoinHostPort(strings.ToLower(first.Hostname()), "443")
}

// chainLeftOrigin reports whether any hop of the chain — the current one included — has an origin
// other than the first request's. With allowUpgrade (the Runner), the strict http:80 → https:443
// upgrade of the first request's host does not count; the canary never allows it.
func chainLeftOrigin(req *http.Request, via []*http.Request, allowUpgrade bool) bool {
	if len(via) == 0 {
		return false
	}
	first := via[0].URL
	origin := requestOrigin(first)
	hops := make([]*url.URL, 0, len(via))
	for _, v := range via[1:] {
		hops = append(hops, v.URL)
	}
	hops = append(hops, req.URL)
	for _, hop := range hops {
		if requestOrigin(hop) == origin {
			continue
		}
		if allowUpgrade && strictUpgrade(first, hop) {
			continue
		}
		return true
	}
	return false
}

// hasBody reports a request body net/http is about to re-send on this hop (a 307/308).
func hasBody(req *http.Request) bool {
	return req.Body != nil && req.Body != http.NoBody
}

// runnerCheckRedirect is the CheckRedirect of every HTTP client the Runner builds (http, promql,
// rabbitmq management, synthetic). Refusal reasons start with "redirect refused:" and carry no URL,
// so failureClass keeps them.
func runnerCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= runnerMaxRedirects {
		return errors.New("redirect refused: too many redirects")
	}
	switch strings.ToLower(req.URL.Scheme) {
	case "http", "https":
	default:
		return errors.New("redirect refused: not an http or https location")
	}
	if req.URL.User != nil {
		// The transport would turn the userinfo into an Authorization header.
		return errors.New("redirect refused: location carries credentials")
	}
	if strings.EqualFold(req.URL.Scheme, "http") {
		for _, v := range via {
			if strings.EqualFold(v.URL.Scheme, "https") {
				return errors.New("redirect refused: https downgraded to http")
			}
		}
	}
	if chainLeftOrigin(req, via, true) {
		if hasBody(req) {
			// The Runner cannot tell a secret-bearing body from another (the synthetic secret is
			// substituted before the prober runs, and a {{var}} may carry what a secret obtained),
			// so no body crosses the origin at all.
			return errors.New("redirect refused: request body to another origin")
		}
		for name := range via[0].Header {
			req.Header.Del(name)
		}
	}
	// The library sets Referer to the previous hop's full URL, query included; a prober has no use
	// for it.
	req.Header.Del("Referer")
	return nil
}
