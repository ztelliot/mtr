package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/store"
)

func (s *Server) streamJobEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	jobID := chi.URLParam(r, "id")
	ctx := r.Context()
	if _, err := s.store.GetJob(ctx, jobID); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	events := s.hub.SubscribeJob(ctx, jobID)
	history, err := s.store.ListJobEvents(ctx, jobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	seen := map[string]struct{}{}
	for _, event := range history {
		seen[event.ID] = struct{}{}
		if err := writeSSE(w, event); err != nil {
			return
		}
	}
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case event, ok := <-events:
			if !ok {
				return
			}
			if _, ok := seen[event.ID]; ok {
				continue
			}
			seen[event.ID] = struct{}{}
			if err := writeSSE(w, event); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, event model.JobEvent) error {
	b, err := json.Marshal(ssePayload(event))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", event.ID, event.EventType(), b)
	return err
}

func ssePayload(event model.JobEvent) map[string]any {
	payload := map[string]any{}
	if raw, ok := event.Payload().(map[string]any); ok {
		for key, value := range raw {
			payload[key] = value
		}
	}
	if _, ok := payload["type"]; !ok {
		payload["type"] = event.EventType()
	}
	if event.AgentID != "" {
		payload["agent_id"] = event.AgentID
	}
	return payload
}

func eventPayloads(events []model.JobEvent) []map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, event := range events {
		out = append(out, ssePayload(event))
	}
	return out
}
