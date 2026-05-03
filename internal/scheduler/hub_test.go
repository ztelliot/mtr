package scheduler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/grpcwire"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
)

func upsertHubTestAgent(t *testing.T, ctx context.Context, st store.Store, agent model.Agent) {
	t.Helper()
	if err := st.UpsertAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
}

func upsertHubTestAgentConfigLabels(t *testing.T, ctx context.Context, st store.Store, id string, labels []string) {
	t.Helper()
	cfg, err := st.GetAgentConfig(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Labels = labels
	if err := st.UpsertAgentConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterTokenAllowedRequiresConfiguredToken(t *testing.T) {
	hub := NewHub(store.NewMemory(), policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())

	if hub.registerTokenAllowed("change-me-agent-token") {
		t.Fatal("unconfigured register token should not allow agent registration")
	}
}

func TestApplySettingsRegisterTokensRestrictRegistration(t *testing.T) {
	hub := NewHub(store.NewMemory(), policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())
	hub.ApplySettings(config.ManagedSettings{RegisterTokens: []config.RegisterToken{{Name: "managed", Token: "managed-token"}}})

	if !hub.registerTokenAllowed("managed-token") {
		t.Fatal("managed register token should allow agent registration")
	}
	if hub.registerTokenAllowed("change-me-agent-token") {
		t.Fatal("unexpected token should not allow agent registration")
	}
}

func TestFailTimedOutQueuedJobPublishesFailure(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	runtime := config.DefaultRuntime()
	runtime.Count = 1
	runtime.ProbeStepTimeoutSec = 1
	policies := policy.PoliciesWithRuntime(runtime)
	hub := NewHub(st, policies, time.Minute, time.Millisecond, 4, slog.Default())

	job := model.Job{
		ID:        "job-timeout",
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		AgentID:   "edge-1",
		Status:    model.JobQueued,
		CreatedAt: time.Now().UTC().Add(-2 * time.Second),
		UpdatedAt: time.Now().UTC().Add(-2 * time.Second),
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := hub.SubscribeJob(subCtx, job.ID)

	hub.failTimedOutJobs(ctx)

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobFailed || got.Error != model.JobErrorTimeout || got.CompletedAt == nil {
		t.Fatalf("timed out job = %#v", got)
	}
	storedEvents, err := st.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(storedEvents) != 2 {
		t.Fatalf("stored events = %#v", storedEvents)
	}
	if storedEvents[0].Event == nil || storedEvents[0].Event.Message != model.JobErrorTimeout {
		t.Fatalf("failure event = %#v", storedEvents[0])
	}
	if storedEvents[1].Event == nil || storedEvents[1].Event.Message != "failed" {
		t.Fatalf("terminal event = %#v", storedEvents[1])
	}

	if event := waitHubTestEvent(t, events); event.Event == nil || event.Event.Message != model.JobErrorTimeout {
		t.Fatalf("stream failure event = %#v", event)
	}
	if event := waitHubTestEvent(t, events); event.Event == nil || event.Event.Message != "failed" {
		t.Fatalf("stream terminal event = %#v", event)
	}
}

func TestRunDueSchedulesUsesLabelTargetsWithIndependentIntervals(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	now := time.Now().UTC().Add(-time.Minute)
	for _, agent := range []model.Agent{
		{ID: "edge-blue", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-red", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-green", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		upsertHubTestAgent(t, ctx, st, agent)
	}
	upsertHubTestAgentConfigLabels(t, ctx, st, "edge-blue", []string{"blue", "red"})
	upsertHubTestAgentConfigLabels(t, ctx, st, "edge-red", []string{"red"})
	upsertHubTestAgentConfigLabels(t, ctx, st, "edge-green", []string{"green"})
	sched := model.ScheduledJob{
		ID:             "sched-labels",
		Revision:       1,
		Enabled:        true,
		Tool:           model.ToolPing,
		Target:         "1.1.1.1",
		ResolveOnAgent: true,
		NextRunAt:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
		ScheduleTargets: []model.ScheduleTarget{
			{ID: "blue", Label: "blue", IntervalSeconds: 30, NextRunAt: now},
			{ID: "red", Label: "red", IntervalSeconds: 60, NextRunAt: now},
		},
	}
	if err := st.CreateScheduledJob(ctx, sched); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())

	hub.runDueSchedules(ctx)

	jobs, err := st.ListActiveJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	gotAgents := map[string]bool{}
	for _, job := range jobs {
		gotAgents[job.AgentID] = true
	}
	if len(jobs) != 2 || !gotAgents["edge-blue"] || !gotAgents["edge-red"] {
		t.Fatalf("scheduled label jobs = %#v", jobs)
	}
	loaded, err := st.GetScheduledJob(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastRunAt == nil || len(loaded.ScheduleTargets) != 2 {
		t.Fatalf("schedule targets not updated: %#v", loaded)
	}
	for _, target := range loaded.ScheduleTargets {
		if target.LastRunAt == nil || !target.NextRunAt.After(*target.LastRunAt) {
			t.Fatalf("target run state not advanced: %#v", target)
		}
	}
}

func TestScheduledIDLabelsMatchOnlyRealAgentID(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	now := time.Now().UTC().Add(-time.Minute)
	for _, agent := range []model.Agent{
		{ID: "edge-real", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-spoof", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		if err := st.UpsertAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	sched := model.ScheduledJob{
		ID:        "sched-id-label",
		Revision:  1,
		Enabled:   true,
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		NextRunAt: now,
		CreatedAt: now,
		UpdatedAt: now,
		ScheduleTargets: []model.ScheduleTarget{
			{ID: "target-real", Label: model.AgentIDLabel("edge-real"), IntervalSeconds: 30, NextRunAt: now},
		},
	}
	if err := st.CreateScheduledJob(ctx, sched); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())

	hub.runDueSchedules(ctx)

	jobs, err := st.ListActiveJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].AgentID != "edge-real" {
		t.Fatalf("scheduled id label jobs = %#v", jobs)
	}
}

func TestScheduledTargetsSkipAgentsOutsideAllowedScope(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	now := time.Now().UTC().Add(-time.Minute)
	for _, agent := range []model.Agent{
		{ID: "edge-allowed", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-denied", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		upsertHubTestAgent(t, ctx, st, agent)
	}
	upsertHubTestAgentConfigLabels(t, ctx, st, "edge-allowed", []string{"blue"})
	upsertHubTestAgentConfigLabels(t, ctx, st, "edge-denied", []string{"blue"})
	sched := model.ScheduledJob{
		ID:        "sched-allowed-scope",
		Revision:  1,
		Enabled:   true,
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		NextRunAt: now,
		CreatedAt: now,
		UpdatedAt: now,
		ScheduleTargets: []model.ScheduleTarget{
			{ID: "target-blue", Label: "blue", AllowedAgentIDs: []string{"edge-allowed"}, IntervalSeconds: 30, NextRunAt: now},
		},
	}
	if err := st.CreateScheduledJob(ctx, sched); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())

	hub.runDueSchedules(ctx)

	jobs, err := st.ListActiveJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].AgentID != "edge-allowed" {
		t.Fatalf("scheduled scoped label jobs = %#v", jobs)
	}
}

func TestScheduledTargetsDoNotDriftToNewMatchingAgents(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	now := time.Now().UTC().Add(-time.Minute)
	for _, agent := range []model.Agent{
		{ID: "edge-frozen", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-new", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		upsertHubTestAgent(t, ctx, st, agent)
	}
	upsertHubTestAgentConfigLabels(t, ctx, st, "edge-frozen", []string{"blue"})
	upsertHubTestAgentConfigLabels(t, ctx, st, "edge-new", []string{"blue"})
	sched := model.ScheduledJob{
		ID:        "sched-frozen-scope",
		Revision:  1,
		Enabled:   true,
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		NextRunAt: now,
		CreatedAt: now,
		UpdatedAt: now,
		ScheduleTargets: []model.ScheduleTarget{
			{ID: "target-blue", Label: "blue", AllowedAgentIDs: []string{"edge-frozen"}, IntervalSeconds: 30, NextRunAt: now},
		},
	}
	if err := st.CreateScheduledJob(ctx, sched); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())

	hub.runDueSchedules(ctx)

	jobs, err := st.ListActiveJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].AgentID != "edge-frozen" {
		t.Fatalf("scheduled scope drifted to new agent: %#v", jobs)
	}
}

func TestHandleResultRejectsUnassignedAgent(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())
	now := time.Now().UTC()
	job := model.Job{
		ID:        "job-owned",
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		AgentID:   "edge-owner",
		Status:    model.JobRunning,
		CreatedAt: now,
		UpdatedAt: now,
		StartedAt: &now,
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	hub.markInflight("edge-owner", job.ID)

	hub.handleResult(ctx, "edge-other", grpcwire.ResultEvent{
		JobID: job.ID,
		Event: map[string]any{"type": "summary", "exit_code": 0},
	})

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobRunning {
		t.Fatalf("unassigned agent should not complete job: %#v", got)
	}
	events, err := st.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("unassigned agent should not write events: %#v", events)
	}
}

func TestHandleResultRequiresInflightOwnership(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())
	now := time.Now().UTC()
	job := model.Job{
		ID:        "job-not-inflight",
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		AgentID:   "edge-owner",
		Status:    model.JobRunning,
		CreatedAt: now,
		UpdatedAt: now,
		StartedAt: &now,
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	hub.handleResult(ctx, "edge-owner", grpcwire.ResultEvent{
		JobID: job.ID,
		Event: map[string]any{"type": "summary", "exit_code": 0},
	})

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobRunning {
		t.Fatalf("non-inflight job should not complete: %#v", got)
	}
}

func TestHandleResultAcceptsAssignedInflightAgent(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())
	now := time.Now().UTC()
	job := model.Job{
		ID:        "job-inflight",
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		AgentID:   "edge-owner",
		Status:    model.JobRunning,
		CreatedAt: now,
		UpdatedAt: now,
		StartedAt: &now,
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	hub.markInflight("edge-owner", job.ID)

	hub.handleResult(ctx, "edge-owner", grpcwire.ResultEvent{
		JobID: job.ID,
		Event: map[string]any{"type": "summary", "exit_code": 0},
	})

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobSucceeded {
		t.Fatalf("assigned inflight job should complete: %#v", got)
	}
	events, err := st.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("assigned inflight job should write summary and progress events: %#v", events)
	}
}

func TestFailTimedOutJobsDefersFanoutParentToChildren(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	runtime := config.DefaultRuntime()
	runtime.Count = 1
	runtime.ProbeStepTimeoutSec = 1
	policies := policy.PoliciesWithRuntime(runtime)
	hub := NewHub(st, policies, time.Minute, time.Millisecond, 4, slog.Default())

	old := time.Now().UTC().Add(-10 * time.Second)
	now := time.Now().UTC()
	parent := model.Job{
		ID:        "parent-timeout",
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		Status:    model.JobRunning,
		CreatedAt: old,
		UpdatedAt: old,
		StartedAt: &old,
	}
	child := model.Job{
		ID:        "child-active",
		ParentID:  parent.ID,
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		AgentID:   "edge-1",
		Status:    model.JobQueued,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := st.CreateJobs(ctx, []model.Job{parent, child}); err != nil {
		t.Fatal(err)
	}

	hub.failTimedOutJobs(ctx)

	gotParent, err := st.GetJob(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotParent.Status != model.JobRunning || gotParent.Error != "" {
		t.Fatalf("fanout parent should wait for child completion: %#v", gotParent)
	}
	gotChild, err := st.GetJob(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotChild.Status != model.JobQueued || gotChild.Error != "" {
		t.Fatalf("active child should be unchanged: %#v", gotChild)
	}
	events, err := st.ListJobEvents(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("unexpected parent events: %#v", events)
	}
}

func TestFailTimedOutJobsFailsFanoutParentAfterOverallLimit(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	runtime := config.DefaultRuntime()
	runtime.Count = 1
	runtime.ProbeStepTimeoutSec = 1
	runtime.MaxToolTimeoutSec = 3
	policies := policy.PoliciesWithRuntime(runtime)
	hub := NewHub(st, policies, time.Minute, time.Millisecond, 4, slog.Default())

	old := time.Now().UTC().Add(-5 * time.Second)
	now := time.Now().UTC()
	parent := model.Job{
		ID:        "parent-overall-timeout",
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		Status:    model.JobRunning,
		CreatedAt: old,
		UpdatedAt: old,
		StartedAt: &old,
	}
	child := model.Job{
		ID:        "child-still-active",
		ParentID:  parent.ID,
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		AgentID:   "edge-1",
		Status:    model.JobRunning,
		CreatedAt: now,
		UpdatedAt: now,
		StartedAt: &now,
	}
	if err := st.CreateJobs(ctx, []model.Job{parent, child}); err != nil {
		t.Fatal(err)
	}
	childCtx, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	grpcCancelCh := make(chan string, 1)
	hub.mu.Lock()
	hub.grpcCancels[child.AgentID] = grpcCancelCh
	hub.mu.Unlock()
	hub.markInflight(child.AgentID, child.ID)
	hub.setInflightCancel(child.ID, cancelChild)

	hub.failTimedOutJobs(ctx)

	gotParent, err := st.GetJob(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotParent.Status != model.JobFailed || gotParent.Error != model.JobErrorTimeout {
		t.Fatalf("fanout parent should fail at overall limit: %#v", gotParent)
	}
	gotChild, err := st.GetJob(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotChild.Status != model.JobFailed || gotChild.Error != model.JobErrorTimeout {
		t.Fatalf("fanout child should be killed at parent overall limit: %#v", gotChild)
	}
	if gotChild.CompletedAt == nil {
		t.Fatalf("fanout child should have completion time: %#v", gotChild)
	}
	if hub.inflightCount(child.AgentID) != 0 {
		t.Fatalf("child inflight should be cleared")
	}
	select {
	case <-childCtx.Done():
	default:
		t.Fatal("child context should be canceled")
	}
	select {
	case got := <-grpcCancelCh:
		if got != child.ID {
			t.Fatalf("grpc cancel job id = %q, want %q", got, child.ID)
		}
	default:
		t.Fatal("grpc cancel should be sent")
	}
	events, err := st.ListJobEvents(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("stored parent events = %#v", events)
	}
	if events[0].Event == nil || events[0].Event.Message != model.JobErrorTimeout {
		t.Fatalf("failure event = %#v", events[0])
	}
	if events[1].Event == nil || events[1].Event.Message != "failed" {
		t.Fatalf("terminal event = %#v", events[1])
	}
}

func TestFailTimedOutJobsSucceedsFanoutParentWhenSomeChildrenSucceeded(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	runtime := config.DefaultRuntime()
	runtime.Count = 1
	runtime.ProbeStepTimeoutSec = 1
	runtime.MaxToolTimeoutSec = 3
	policies := policy.PoliciesWithRuntime(runtime)
	hub := NewHub(st, policies, time.Minute, time.Millisecond, 4, slog.Default())

	old := time.Now().UTC().Add(-5 * time.Second)
	done := time.Now().UTC().Add(-1 * time.Second)
	parent := model.Job{
		ID:        "parent-partial-timeout",
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		Status:    model.JobRunning,
		CreatedAt: old,
		UpdatedAt: old,
		StartedAt: &old,
	}
	succeededChild := model.Job{
		ID:          "child-succeeded",
		ParentID:    parent.ID,
		Tool:        model.ToolPing,
		Target:      "1.1.1.1",
		AgentID:     "edge-ok",
		Status:      model.JobSucceeded,
		CreatedAt:   old,
		UpdatedAt:   done,
		StartedAt:   &old,
		CompletedAt: &done,
	}
	activeChild := model.Job{
		ID:        "child-still-active",
		ParentID:  parent.ID,
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		AgentID:   "edge-timeout",
		Status:    model.JobRunning,
		CreatedAt: old,
		UpdatedAt: old,
		StartedAt: &old,
	}
	if err := st.CreateJobs(ctx, []model.Job{parent, succeededChild, activeChild}); err != nil {
		t.Fatal(err)
	}

	hub.failTimedOutJobs(ctx)

	gotParent, err := st.GetJob(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotParent.Status != model.JobSucceeded || gotParent.Error != "" {
		t.Fatalf("fanout parent with partial success should succeed: %#v", gotParent)
	}
	gotChild, err := st.GetJob(ctx, activeChild.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotChild.Status != model.JobFailed || gotChild.Error != model.JobErrorTimeout {
		t.Fatalf("active fanout child should still be timed out: %#v", gotChild)
	}
	events, err := st.ListJobEvents(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 && len(events) != 2 {
		t.Fatalf("stored parent events = %#v", events)
	}
	if len(events) == 2 && (events[0].AgentID != activeChild.AgentID || events[0].Event == nil || events[0].Event.Message != model.JobErrorTimeout) {
		t.Fatalf("child timeout event = %#v", events[0])
	}
	terminal := events[len(events)-1]
	if terminal.Event == nil || terminal.Event.Message != "completed" {
		t.Fatalf("terminal event = %#v", terminal)
	}
}

func TestCompleteParentIfDoneSucceedsWhenSomeChildrenFailed(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())

	now := time.Now().UTC()
	parent := model.Job{
		ID:        "parent-partial-failed",
		Tool:      model.ToolHTTP,
		Target:    "https://example.com",
		Status:    model.JobRunning,
		CreatedAt: now,
		UpdatedAt: now,
		StartedAt: &now,
	}
	succeededChild := model.Job{
		ID:          "child-ok",
		ParentID:    parent.ID,
		Tool:        parent.Tool,
		Target:      parent.Target,
		AgentID:     "edge-ok",
		Status:      model.JobSucceeded,
		CreatedAt:   now,
		UpdatedAt:   now,
		StartedAt:   &now,
		CompletedAt: &now,
	}
	failedChild := model.Job{
		ID:          "child-failed",
		ParentID:    parent.ID,
		Tool:        parent.Tool,
		Target:      parent.Target,
		AgentID:     "edge-failed",
		Status:      model.JobFailed,
		Error:       model.JobErrorToolFailed,
		CreatedAt:   now,
		UpdatedAt:   now,
		StartedAt:   &now,
		CompletedAt: &now,
	}
	if err := st.CreateJobs(ctx, []model.Job{parent, succeededChild, failedChild}); err != nil {
		t.Fatal(err)
	}

	hub.completeParentIfDone(ctx, parent.ID)

	gotParent, err := st.GetJob(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotParent.Status != model.JobSucceeded || gotParent.Error != "" {
		t.Fatalf("fanout parent with partial child failure should succeed: %#v", gotParent)
	}
	events, err := st.ListJobEvents(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event == nil || events[0].Event.Message != "completed" {
		t.Fatalf("stored parent events = %#v", events)
	}
}

func TestHubInflightLimit(t *testing.T) {
	hub := NewHub(store.NewMemory(), policy.DefaultPolicies(), time.Minute, time.Millisecond, 9, slog.Default())

	if got := hub.inflightLimit(); got != 9 {
		t.Fatalf("default inflight = %d, want 9", got)
	}

	hub.SetInflightLimit(6)
	if got := hub.inflightLimit(); got != 6 {
		t.Fatalf("configured inflight = %d, want 6", got)
	}

	hub.SetInflightLimit(0)
	if got := hub.inflightLimit(); got != 4 {
		t.Fatalf("fallback inflight = %d, want 4", got)
	}
}

func waitHubTestEvent(t *testing.T, events <-chan model.JobEvent) model.JobEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for hub event")
	}
	return model.JobEvent{}
}
