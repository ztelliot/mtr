package api

import (
	"context"
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
	if err := decodeLimitedJSON(w, r, &req); err != nil {
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
		ResolveOnAgent:  details.createReq.ResolveOnAgent,
		NextRunAt:       now,
		ScheduleTargets: details.scheduleTargets,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	normalizeScheduleRunFields(&sched)
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
	if err := decodeLimitedJSON(w, r, &req); err != nil {
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
	intervalChanged := scheduleIntervalsChanged(existing, details.scheduleTargets)
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
		ResolveOnAgent:  details.createReq.ResolveOnAgent,
		NextRunAt:       nextRunAt,
		LastRunAt:       existing.LastRunAt,
		ScheduleTargets: details.scheduleTargets,
		CreatedAt:       existing.CreatedAt,
		UpdatedAt:       now,
	}
	if len(sched.ScheduleTargets) > 0 && !definitionChanged {
		mergeScheduleTargetRunState(sched.ScheduleTargets, existing.EffectiveScheduleTargets(), intervalChanged || !existing.Enabled)
	}
	normalizeScheduleRunFields(&sched)
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
	scheduleTargets []model.ScheduleTarget
}

func (s *Server) validateScheduleRequest(w http.ResponseWriter, r *http.Request, req model.CreateScheduledJobRequest, defaultEnabled bool) (scheduleRequestDetails, bool) {
	now := time.Now().UTC()
	createReq := model.CreateJobRequest{
		Tool:           req.Tool,
		Target:         req.Target,
		Args:           req.Args,
		IPVersion:      req.IPVersion,
		ResolveOnAgent: req.ResolveOnAgent,
	}
	scheduleTargets, err := scheduleTargetsFromRequest(req, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return scheduleRequestDetails{}, false
	}
	if !s.limiterSnapshot().AllowTool(string(req.Tool), s.clientIP.Resolve(r)) {
		writeError(w, http.StatusTooManyRequests, "tool rate limit exceeded")
		return scheduleRequestDetails{}, false
	}
	policies := s.policiesForJob(r.Context(), createReq)
	createReq.Args = policies.ServerArgs(createReq.Tool, createReq.Args)
	if _, err := policies.ValidateSchedule(createReq); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return scheduleRequestDetails{}, false
	}
	if err := s.authorizeJob(r.Context(), createReq); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return scheduleRequestDetails{}, false
	}
	if _, err := s.dispatchOptions(r.Context(), createReq, policies); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return scheduleRequestDetails{}, false
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
	scheduleTargets, err = s.authorizeScheduleTargets(r.Context(), createReq, scheduleVersion, scheduleTargets)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return scheduleRequestDetails{}, false
	}
	enabled := defaultEnabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return scheduleRequestDetails{createReq: createReq, scheduleVersion: scheduleVersion, enabled: enabled, scheduleTargets: scheduleTargets}, true
}

func (s *Server) authorizeScheduleTargets(ctx context.Context, req model.CreateJobRequest, version model.IPVersion, targets []model.ScheduleTarget) ([]model.ScheduleTarget, error) {
	scope := principalFromContext(ctx).scope
	agents, err := s.agentsWithManagedLabels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]model.ScheduleTarget, 0, len(targets))
	for _, target := range targets {
		matched := false
		target.AllowedAgentIDs = nil
		for _, agent := range agents {
			if !scheduleTargetMatchesAgent(target.Label, agent) {
				continue
			}
			if !scopeAllowsAgentRecord(scope, agent) {
				continue
			}
			agentPolicies := s.policiesForLabels(agent.Labels)
			if !policy.AgentSupports(agent, req.Tool, version) {
				continue
			}
			validateReq := req
			validateReq.AgentID = agent.ID
			if _, err := agentPolicies.ValidateSchedule(validateReq); err != nil {
				continue
			}
			matched = true
			target.AllowedAgentIDs = append(target.AllowedAgentIDs, agent.ID)
		}
		if !matched {
			return nil, fmt.Errorf("schedule target %q has no allowed online agents for %s", target.Label, req.Tool)
		}
		sort.Strings(target.AllowedAgentIDs)
		out = append(out, target)
	}
	return out, nil
}

func scheduleTargetMatchesAgent(label string, agent model.Agent) bool {
	label = strings.TrimSpace(label)
	for _, item := range agent.Labels {
		if strings.TrimSpace(item) == label {
			return true
		}
	}
	return false
}

func scheduleDefinitionChanged(existing model.ScheduledJob, req model.CreateScheduledJobRequest, details scheduleRequestDetails) bool {
	return existing.Tool != details.createReq.Tool ||
		existing.Target != details.createReq.Target ||
		existing.IPVersion != details.scheduleVersion ||
		existing.ResolveOnAgent != details.createReq.ResolveOnAgent ||
		!scheduleTargetsSameDefinition(existing.EffectiveScheduleTargets(), details.scheduleTargets) ||
		!maps.Equal(existing.Args, details.createReq.Args)
}

func scheduleTargetsFromRequest(req model.CreateScheduledJobRequest, now time.Time) ([]model.ScheduleTarget, error) {
	targets := req.ScheduleTargets
	if len(targets) == 0 {
		return nil, fmt.Errorf("schedule_targets is required")
	}
	out := make([]model.ScheduleTarget, 0, len(targets))
	seen := map[string]struct{}{}
	for i, target := range targets {
		label := strings.TrimSpace(target.Label)
		if label == "" {
			return nil, fmt.Errorf("schedule_targets[%d].label is required", i)
		}
		if target.IntervalSeconds < 10 || target.IntervalSeconds > 86400 {
			return nil, fmt.Errorf("schedule_targets[%d].interval_seconds must be between 10 and 86400", i)
		}
		if _, ok := seen[label]; ok {
			return nil, fmt.Errorf("duplicate schedule target %q", label)
		}
		seen[label] = struct{}{}
		out = append(out, model.ScheduleTarget{
			ID:              uuid.NewString(),
			Label:           label,
			IntervalSeconds: target.IntervalSeconds,
			NextRunAt:       now,
		})
	}
	return out, nil
}

func scheduleTargetsSameDefinition(left []model.ScheduleTarget, right []model.ScheduleTarget) bool {
	if len(left) != len(right) {
		return false
	}
	leftKeys := make([]string, 0, len(left))
	rightKeys := make([]string, 0, len(right))
	for _, target := range left {
		leftKeys = append(leftKeys, scheduleTargetDefinitionKey(target))
	}
	for _, target := range right {
		rightKeys = append(rightKeys, scheduleTargetDefinitionKey(target))
	}
	sort.Strings(leftKeys)
	sort.Strings(rightKeys)
	for i := range leftKeys {
		if leftKeys[i] != rightKeys[i] {
			return false
		}
	}
	return true
}

func scheduleTargetDefinitionKey(target model.ScheduleTarget) string {
	return target.Label + "\x00" + strings.Join(target.AllowedAgentIDs, ",")
}

func scheduleIntervalsChanged(existing model.ScheduledJob, targets []model.ScheduleTarget) bool {
	left := existing.EffectiveScheduleTargets()
	if len(left) != len(targets) {
		return true
	}
	leftByKey := map[string]int{}
	for _, target := range left {
		leftByKey[scheduleTargetDefinitionKey(target)] = target.IntervalSeconds
	}
	for _, target := range targets {
		if leftByKey[scheduleTargetDefinitionKey(target)] != target.IntervalSeconds {
			return true
		}
	}
	return false
}

func mergeScheduleTargetRunState(next []model.ScheduleTarget, existing []model.ScheduleTarget, reset bool) {
	if reset {
		return
	}
	byKey := map[string]model.ScheduleTarget{}
	for _, target := range existing {
		byKey[target.Label] = target
	}
	for i := range next {
		if old, ok := byKey[next[i].Label]; ok {
			next[i].ID = old.ID
			next[i].NextRunAt = old.NextRunAt
			next[i].LastRunAt = old.LastRunAt
		}
	}
}

func normalizeScheduleRunFields(sched *model.ScheduledJob) {
	if len(sched.ScheduleTargets) == 0 {
		return
	}
	earliest := sched.ScheduleTargets[0].NextRunAt
	var latestLast *time.Time
	for _, target := range sched.ScheduleTargets {
		if target.NextRunAt.Before(earliest) {
			earliest = target.NextRunAt
		}
		if target.LastRunAt != nil && (latestLast == nil || target.LastRunAt.After(*latestLast)) {
			t := *target.LastRunAt
			latestLast = &t
		}
	}
	sched.NextRunAt = earliest
	sched.LastRunAt = latestLast
	sched.IntervalSeconds = sched.ScheduleTargets[0].IntervalSeconds
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
