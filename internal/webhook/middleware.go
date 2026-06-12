package webhook

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	protocol "github.com/mhersson/contextmatrix-protocol"
)

// auth verifies the HMAC signature over METHOD\nURI\nTS.BODY for both GET and
// POST, then re-injects the consumed body so the wrapped handler can read it.
//
// The signed value is exactly what protocol.SignPayloadWithTimestamp produces:
// the method, the full request-target (r.URL.RequestURI(), query included), the
// timestamp, and the body. GETs sign with an empty body but the same URI — so a
// signed GET /logs?project=x must include the query in the base string.
//
// On any verification failure (missing headers, bad signature, stale or future
// timestamp, replay) the response is a fixed 401 ErrorResponse with the
// unauthorized code; the specific reason is logged server-side only so a
// scanner cannot fingerprint which check failed. An oversized body short-
// circuits with 413 before verification, because a body truncated at the cap
// would otherwise fail the signature and surface as a misleading 401.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Reject oversized bodies up front: a declared Content-Length past the
		// cap can never verify (we would truncate it), so a 413 is the honest
		// answer rather than a 401.
		if r.ContentLength > maxRequestBodyBytes {
			s.logger.Debug("auth: content-length exceeds cap",
				"remote_addr", r.RemoteAddr, "content_length", r.ContentLength)
			writeError(w, http.StatusRequestEntityTooLarge, protocol.CodeTooLarge, "request body too large")

			return
		}

		sigHeader := r.Header.Get(protocol.SignatureHeader)

		tsHeader := r.Header.Get(protocol.TimestampHeader)
		if sigHeader == "" || tsHeader == "" {
			s.logger.Debug("auth: missing signature or timestamp header", "remote_addr", r.RemoteAddr)
			writeUnauthorized(w)

			return
		}

		// MaxBytesReader bounds the read AND, on overflow for streaming bodies
		// without a declared Content-Length, makes the read error so we fail
		// rather than silently truncate and mis-verify.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
		if err != nil {
			s.logger.Debug("auth: body read failed", "remote_addr", r.RemoteAddr, "error", err)
			writeError(w, http.StatusRequestEntityTooLarge, protocol.CodeTooLarge, "request body too large")

			return
		}

		sig := strings.TrimPrefix(sigHeader, "sha256=")

		if !protocol.VerifySignatureWithTimestamp(
			s.apiKey, r.Method, r.URL.RequestURI(), sig, tsHeader, body, s.skew, s.replay,
		) {
			s.logger.Warn("webhook authentication failed",
				"remote_addr", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
			writeUnauthorized(w)

			return
		}

		// Re-inject the consumed body so the handler reads it normally.
		r.Body = io.NopCloser(bytes.NewReader(body))

		next(w, r)
	}
}

// drainGate refuses mutating requests with 503 once graceful shutdown has
// begun, so the shutdown sequence can finish in-flight work without CM pushing
// more onto a draining backend.
func (s *Server) drainGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() {
			writeError(w, http.StatusServiceUnavailable, protocol.CodeDraining, "backend is draining")

			return
		}

		next(w, r)
	}
}

// writeUnauthorized returns the single fixed 401 shape for every authentication
// failure. The specific cause is logged in auth, never echoed to the client.
func writeUnauthorized(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, protocol.CodeUnauthorized, "unauthorized")
}
