package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
)

// defaultKeepaliveInterval is the SSE comment heartbeat period. Unexported and
// overridable per-server so tests can shrink it.
const defaultKeepaliveInterval = 15 * time.Second

// handleLogs streams protocol.LogEntry frames as Server-Sent Events, filtered
// by the ?project= query (empty = all projects).
//
// It writes ": connected\n\n" and flushes IMMEDIATELY: ContextMatrix's
// session-log client gives up after a handful of rapid connect failures, so the
// instant comment marks the stream healthy before any real frame arrives.
// Thereafter each entry is one "data: <json>\n\n" event; a keepalive comment is
// emitted on the ticker; and client disconnect (r.Context().Done()) unsubscribes
// and returns.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, protocol.CodeInternal, "streaming not supported")

		return
	}

	project := r.URL.Query().Get("project")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Subscribe before the connected comment so receiving that line is a
	// client-observable guarantee that the subscription is already live.
	id, ch := s.hub.Subscribe(project)
	defer s.hub.Unsubscribe(id)

	// Mark the stream healthy immediately so the CM client's rapid-failure
	// counter never trips on a slow first frame.
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		s.logger.Debug("SSE initial write failed", "error", err)

		return
	}

	flusher.Flush()

	interval := s.keepaliveInterval
	if interval <= 0 {
		interval = defaultKeepaliveInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.logger.Info("SSE log client connected", "project_filter", project, "remote_addr", r.RemoteAddr)

	for {
		select {
		case <-r.Context().Done():
			s.logger.Info("SSE log client disconnected", "project_filter", project, "remote_addr", r.RemoteAddr)

			return

		case <-s.sseShutdown:
			s.logger.Info("SSE log client closed on drain", "project_filter", project, "remote_addr", r.RemoteAddr)

			return

		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				s.logger.Debug("SSE keepalive write failed", "error", err)

				return
			}

			flusher.Flush()

		case entry, ok := <-ch:
			if !ok {
				return
			}

			data, err := json.Marshal(entry)
			if err != nil {
				s.logger.Debug("SSE marshal failed", "error", err)

				continue
			}

			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				s.logger.Debug("SSE event write failed", "error", err)

				return
			}

			flusher.Flush()
		}
	}
}
