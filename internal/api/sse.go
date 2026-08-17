package api

import (
	"fmt"
	"net/http"
	"time"
)

// heartbeatInterval keeps idle SSE connections alive through proxies that
// close quiet sockets. It is shorter than the typical 60s idle timeout on an
// ingress controller.
const heartbeatInterval = 25 * time.Second

// handleEvents streams live updates to the dashboard.
//
// This replaces polling: the dashboard subscribes once and receives health
// recomputations, weight changes, fail-open transitions, incidents, and
// rollout changes as they happen.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disables response buffering in nginx-based ingress controllers, which
	// would otherwise hold events until the buffer fills.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Tell the browser how long to wait before reconnecting if the stream
	// drops, so a rolling deploy does not produce a reconnect storm.
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	ch, unsubscribe := s.hub.Subscribe()
	defer unsubscribe()

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return

		case ev, open := <-ch:
			if !open {
				return
			}
			payload, err := ev.Encode()
			if err != nil {
				s.log.Error("encode SSE event", "type", ev.Type, "error", err)
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload); err != nil {
				return
			}
			flusher.Flush()

		case <-heartbeat.C:
			// A comment line is a valid SSE frame the client ignores.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
