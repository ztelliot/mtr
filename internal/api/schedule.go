package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
)

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.scheduleWriteAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "schedule write access is not allowed")
		return
	}
	var req model.CreateScheduledJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	details, ok := s.validateScheduleRequest(w, r, req, true)
	if !ok {
		return
	}
	now := time.Now().UTC()
	sched := model.ScheduledJob{
		ID:              uuid.NewString(),
		Revision:        1,
		Name:            strings.TrimSpace(req.Name),
		Enabled:         details.enabled,
		Tool:            details.createReq.Tool,
		Target:          details.createReq.Target,
		Args:            details.createReq.Args,
		IPVersion:       details.scheduleVersion,
		AgentID:         details.createReq.AgentID,
		ResolveOnAgent:  details.createReq.ResolveOnAgent,
		IntervalSeconds: req.IntervalSeconds,
		NextRunAt:       now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.store.CreateScheduledJob(r.Context(), sched); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sched)
}

func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.scheduleWriteAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "schedule write access is not allowed")
		return
	}
	existing, err := s.store.GetScheduledJob(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	var req model.CreateScheduledJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	details, ok := s.validateScheduleRequest(w, r, req, existing.Enabled)
	if !ok {
		return
	}
	now := time.Now().UTC()
	nextRunAt := existing.NextRunAt
	definitionChanged := scheduleDefinitionChanged(existing, req, details)
	intervalChanged := existing.IntervalSeconds != req.IntervalSeconds
	if nextRunAt.IsZero() || (details.enabled && (!existing.Enabled || definitionChanged || intervalChanged)) {
		nextRunAt = now
	}
	revision := existing.Revision
	if revision <= 0 {
		revision = 1
	}
	if definitionChanged {
		revision++
	}
	sched := model.ScheduledJob{
		ID:              existing.ID,
		Revision:        revision,
		Name:            strings.TrimSpace(req.Name),
		Enabled:         details.enabled,
		Tool:            details.createReq.Tool,
		Target:          details.createReq.Target,
		Args:            details.createReq.Args,
		IPVersion:       details.scheduleVersion,
		AgentID:         details.createReq.AgentID,
		ResolveOnAgent:  details.createReq.ResolveOnAgent,
		IntervalSeconds: req.IntervalSeconds,
		NextRunAt:       nextRunAt,
		LastRunAt:       existing.LastRunAt,
		CreatedAt:       existing.CreatedAt,
		UpdatedAt:       now,
	}
	if err := s.store.UpdateScheduledJob(r.Context(), sched); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sched)
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.scheduleWriteAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "schedule write access is not allowed")
		return
	}
	if err := s.store.DeleteScheduledJob(r.Context(), chi.URLParam(r, "id")); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type scheduleRequestDetails struct {
	createReq       model.CreateJobRequest
	scheduleVersion model.IPVersion
	enabled         bool
}

func (s *Server) validateScheduleRequest(w http.ResponseWriter, r *http.Request, req model.CreateScheduledJobRequest, defaultEnabled bool) (scheduleRequestDetails, bool) {
	if req.IntervalSeconds < 10 || req.IntervalSeconds > 86400 {
		writeError(w, http.StatusBadRequest, "interval_seconds must be between 10 and 86400")
		return scheduleRequestDetails{}, false
	}
	createReq := model.CreateJobRequest{
		Tool:           req.Tool,
		Target:         req.Target,
		Args:           req.Args,
		IPVersion:      req.IPVersion,
		AgentID:        strings.TrimSpace(req.AgentID),
		ResolveOnAgent: req.ResolveOnAgent,
	}
	if !s.limiter.AllowTool(string(req.Tool), s.clientIP.Resolve(r)) {
		writeError(w, http.StatusTooManyRequests, "tool rate limit exceeded")
		return scheduleRequestDetails{}, false
	}
	createReq.Args = s.policies.ServerArgs(createReq.Tool, createReq.Args)
	if _, err := s.policies.Validate(createReq); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return scheduleRequestDetails{}, false
	}
	if err := s.authorizeJob(r.Context(), createReq); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return scheduleRequestDetails{}, false
	}
	options, err := s.dispatchOptions(r.Context(), createReq)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return scheduleRequestDetails{}, false
	}
	if createReq.AgentID != "" {
		if _, _, err := s.pinnedDispatchTarget(r.Context(), createReq.AgentID, createReq.Tool, options); err != nil {
			writeError(w, agentDispatchErrorStatus(err), err.Error())
			return scheduleRequestDetails{}, false
		}
	}
	scheduleVersion := createReq.IPVersion
	if scheduleVersion == model.IPAny {
		literalVersion, literal, err := policy.LiteralIPVersion(createReq.Tool, createReq.Target)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return scheduleRequestDetails{}, false
		}
		if literal {
			scheduleVersion = literalVersion
		}
	}
	enabled := defaultEnabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return scheduleRequestDetails{createReq: createReq, scheduleVersion: scheduleVersion, enabled: enabled}, true
}

func scheduleDefinitionChanged(existing model.ScheduledJob, req model.CreateScheduledJobRequest, details scheduleRequestDetails) bool {
	return existing.Tool != details.createReq.Tool ||
		existing.Target != details.createReq.Target ||
		existing.IPVersion != details.scheduleVersion ||
		existing.AgentID != details.createReq.AgentID ||
		existing.ResolveOnAgent != details.createReq.ResolveOnAgent ||
		!maps.Equal(existing.Args, details.createReq.Args)
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	if !s.scheduleReadAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "schedule read access is not allowed")
		return
	}
	schedules, err := s.store.ListScheduledJobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, schedules)
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.scheduleReadAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "schedule read access is not allowed")
		return
	}
	sched, err := s.store.GetScheduledJob(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sched)
}

func (s *Server) listScheduleHistory(w http.ResponseWriter, r *http.Request) {
	if !s.scheduleReadAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "schedule read access is not allowed")
		return
	}
	scheduleID := chi.URLParam(r, "id")
	sched, err := s.store.GetScheduledJob(r.Context(), scheduleID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	jobs, err := s.store.ListScheduledJobHistory(r.Context(), scheduleID, scheduleHistoryFilterForSchedule(r, sched, false))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) listScheduleHistorySummary(w http.ResponseWriter, r *http.Request) {
	if !s.scheduleReadAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "schedule read access is not allowed")
		return
	}
	scheduleID := chi.URLParam(r, "id")
	sched, err := s.store.GetScheduledJob(r.Context(), scheduleID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	jobs, err := s.store.ListScheduledJobHistory(r.Context(), scheduleID, scheduleHistoryFilterForSchedule(r, sched, true))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]model.JobEvent, 0, len(jobs))
	lastDNSRecordsByAgent := map[string]string{}
	for _, job := range jobs {
		events, err := s.store.ListJobEvents(r.Context(), job.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		event := singleSummaryEvent(job, events)
		if job.Tool == model.ToolDNS {
			key := dnsRecordsKey(event)
			agentID := event.AgentID
			if agentID == "" {
				agentID = job.AgentID
			}
			if key != "" && lastDNSRecordsByAgent[agentID] == key {
				continue
			}
			lastDNSRecordsByAgent[agentID] = key
		}
		out = append(out, event)
	}
	writeJSON(w, http.StatusOK, out)
}

func scheduleHistoryFilterForSchedule(r *http.Request, sched model.ScheduledJob, enableBucket bool) store.ScheduledJobHistoryFilter {
	f := scheduleHistoryFilter(r, enableBucket)
	f.Revision = sched.Revision
	return f
}

func scheduleHistoryFilter(r *http.Request, enableBucket bool) store.ScheduledJobHistoryFilter {
	f := store.ScheduledJobHistoryFilter{Limit: 2000}
	if from, ok := parseScheduleTime(r.URL.Query().Get("from")); ok {
		f.From = from
		f.HasFrom = true
	}
	if to, ok := parseScheduleTime(r.URL.Query().Get("to")); ok {
		f.To = to
		f.HasTo = true
	}
	if f.HasFrom {
		if enableBucket {
			f.BucketSeconds = customScheduleBucketSeconds(f)
		}
		return f
	}
	if duration, bucketSize, ok := scheduleRangeWindow(r.URL.Query().Get("range")); ok {
		f.From = time.Now().UTC().Add(-duration)
		f.HasFrom = true
		if enableBucket {
			f.BucketSeconds = int64(bucketSize / time.Second)
		}
	}
	return f
}

func parseScheduleTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed, true
	}
	var unix int64
	if _, err := fmt.Sscanf(raw, "%d", &unix); err == nil && unix > 0 {
		return time.Unix(unix, 0).UTC(), true
	}
	return time.Time{}, false
}

func scheduleRangeWindow(raw string) (time.Duration, time.Duration, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1h":
		return time.Hour, 10 * time.Second, true
	case "6h":
		return 6 * time.Hour, 30 * time.Second, true
	case "24h":
		return 24 * time.Hour, time.Minute, true
	case "7d":
		return 7 * 24 * time.Hour, 10 * time.Minute, true
	case "30d":
		return 30 * 24 * time.Hour, 30 * time.Minute, true
	default:
		return 0, 0, false
	}
}

func customScheduleBucketSeconds(f store.ScheduledJobHistoryFilter) int64 {
	if !f.HasFrom || !f.HasTo {
		return 0
	}
	span := f.To.Sub(f.From)
	if span <= 0 {
		return 0
	}
	bucket := span / 2000
	if bucket*2000 < span {
		bucket++
	}
	if bucket < time.Second {
		return 0
	}
	return int64(bucket / time.Second)
}

func singleSummaryEvent(job model.Job, events []model.JobEvent) model.JobEvent {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Parsed != nil || event.Stream == "summary" || (event.Event != nil && event.Event.Type == "summary") {
			return enrichScheduleSummaryEvent(job, event)
		}
	}
	return model.JobEvent{
		ID:      job.ID + "-summary",
		JobID:   job.ID,
		AgentID: job.AgentID,
		Stream:  "summary",
		Parsed: &model.ToolResult{
			Type:      "summary",
			Tool:      job.Tool,
			Target:    job.Target,
			IPVersion: job.IPVersion,
			ExitCode:  scheduleSummaryExitCode(job),
			Summary:   scheduleSummaryMetric(job),
		},
		CreatedAt: scheduleHistoryEventTime(job),
	}
}

func enrichScheduleSummaryEvent(job model.Job, event model.JobEvent) model.JobEvent {
	if event.AgentID == "" {
		event.AgentID = job.AgentID
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = scheduleHistoryEventTime(job)
	}
	if event.Parsed != nil {
		parsed := *event.Parsed
		parsed.Summary = cloneSummary(parsed.Summary)
		for key, value := range scheduleSummaryMetric(job) {
			if _, ok := parsed.Summary[key]; !ok {
				parsed.Summary[key] = value
			}
		}
		event.Parsed = &parsed
		return event
	}
	if event.Event != nil && event.Event.Type == "summary" {
		streamEvent := *event.Event
		streamEvent.Metric = cloneSummary(streamEvent.Metric)
		for key, value := range scheduleSummaryMetric(job) {
			if _, ok := streamEvent.Metric[key]; !ok {
				streamEvent.Metric[key] = value
			}
		}
		event.Event = &streamEvent
	}
	return event
}

func cloneSummary(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+3)
	for key, value := range in {
		out[key] = value
	}
	return out
}

func scheduleSummaryMetric(job model.Job) map[string]any {
	metric := map[string]any{"status": string(job.Status)}
	if job.ResolvedTarget != "" {
		metric["target_ip"] = job.ResolvedTarget
	}
	if job.StartedAt != nil && !job.StartedAt.IsZero() {
		metric["started_at"] = job.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	return metric
}

func scheduleHistoryEventTime(job model.Job) time.Time {
	if job.StartedAt != nil && !job.StartedAt.IsZero() {
		return *job.StartedAt
	}
	return job.CreatedAt
}

func scheduleSummaryExitCode(job model.Job) int {
	if job.Status == model.JobFailed || job.Status == model.JobCanceled {
		return 1
	}
	return 0
}

func dnsRecordsKey(event model.JobEvent) string {
	payload, ok := event.Payload().(map[string]any)
	if !ok {
		return ""
	}
	raw, ok := payload["records"]
	if !ok {
		return ""
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	var records []model.DNSRecord
	if err := json.Unmarshal(b, &records); err != nil || len(records) == 0 {
		return ""
	}
	parts := make([]string, 0, len(records))
	for _, record := range records {
		parts = append(parts, record.Type+" "+record.Value)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}
