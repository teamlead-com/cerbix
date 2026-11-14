package auth

import (
	"mime"
	"net/http"
)

// JSONContentTypeError is the refusal text of HasJSONContentType's 415, shared by the API and
// the /auth/local/* handlers so the two answer alike.
const JSONContentTypeError = "Content-Type must be application/json"

// HasJSONContentType is THE content-type rule of sec-hardening-2 H-4 (invariant S4: a JSON body
// is decoded only under a JSON content type), shared by the API's decoders and the three
// /auth/local/* handlers — one rule, not two copies. It reports true when the request declares
// `application/json` (media type compared case-insensitively, parameters such as charset
// allowed) or declares an empty body (Content-Length: 0 — nothing is decoded, and the caller's own
// 400 for a missing body stands). A cross-site form can only send text/plain, urlencoded or
// multipart without a preflight, so it never reaches a decoder.
func HasJSONContentType(r *http.Request) bool {
	if r.ContentLength == 0 {
		return true
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// refuseNonJSON answers 415 unless HasJSONContentType holds. The /auth/local/* handlers call it
// FIRST — before every limiter and before the body is read — so a refused request spends no
// attempt and answers the same for an account that exists and one that does not.
func refuseNonJSON(w http.ResponseWriter, r *http.Request) bool {
	if HasJSONContentType(r) {
		return false
	}
	writeJSONError(w, http.StatusUnsupportedMediaType, JSONContentTypeError)
	return true
}
