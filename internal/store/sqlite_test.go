package store

import (
	"context"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/model"
)

func TestSQLiteRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := NewSQLite(ctx, "sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Second)
	agent := model.Agent{
		ID:           "agent-1",
		Country:      "CN",
		Region:       "local",
		Provider:     "kubernetes",
		ISP:          "test-isp",
		Version:      "v1.2.3",
		Capabilities: []model.Tool{model.ToolPing},
		Protocols:    model.ProtocolIPv4,
		Status:       model.AgentOnline,
		LastSeenAt:   now,
		CreatedAt:    now,
	}
	if err := st.UpsertAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	agents, err := st.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Country != "CN" || agents[0].Provider != "kubernetes" || agents[0].ISP != "test-isp" || agents[0].Version != "v1.2.3" {
		t.Fatalf("unexpected agents: %#v", agents)
	}

	job := model.Job{
		ID:        "job-1",
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		Args:      map[string]string{"count": "4"},
		IPVersion: model.IPv4,
		Status:    model.JobQueued,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	parent := model.Job{
		ID:          "job-parent",
		Tool:        model.ToolPing,
		Target:      "1.1.1.1",
		Status:      model.JobSucceeded,
		CreatedAt:   now,
		UpdatedAt:   now,
		StartedAt:   &now,
		CompletedAt: &now,
	}
	child := parent
	child.ID = "job-child"
	child.ParentID = parent.ID
	child.AgentID = "agent-1"
	child.Status = model.JobSucceeded
	child.StartedAt = nil
	child.CompletedAt = &now
	if err := st.CreateJobs(ctx, []model.Job{parent, child}); err != nil {
		t.Fatal(err)
	}
	children, err := st.ListChildJobs(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].ID != child.ID || children[0].ParentID != parent.ID {
		t.Fatalf("batched child jobs not persisted atomically: %#v", children)
	}
	jobs, err := st.ListQueuedJobs(ctx, "agent-1", []model.Tool{model.ToolPing}, model.ProtocolIPv4, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "job-1" {
		t.Fatalf("unexpected jobs: %#v", jobs)
	}
	jobs, err = st.ListQueuedJobs(ctx, "agent-1", []model.Tool{model.ToolPing}, model.ProtocolIPv6, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected IPv6-only agent mask to skip IPv4 job: %#v", jobs)
	}
	claimed, err := st.ClaimQueuedJob(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("expected queued job to be claimed")
	}
	claimed, err = st.ClaimQueuedJob(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("running job should not be claimed twice")
	}
	claimedJob, err := st.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if claimedJob.Status != model.JobRunning || claimedJob.StartedAt == nil {
		t.Fatalf("claim did not mark job running: %#v", claimedJob)
	}
	activeJobs, err := st.ListActiveJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(activeJobs) != 1 || activeJobs[0].ID != "job-1" {
		t.Fatalf("active jobs = %#v", activeJobs)
	}
	jobs, err = st.ListQueuedJobs(ctx, "agent-1", []model.Tool{model.ToolPing}, model.ProtocolIPv4, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("claimed job still queued: %#v", jobs)
	}

	exit := 0
	event := model.JobEvent{
		ID:       "event-1",
		JobID:    "job-1",
		AgentID:  "agent-1",
		Stream:   "parsed",
		ExitCode: &exit,
		Parsed: &model.ToolResult{
			Tool:     model.ToolPing,
			Target:   "1.1.1.1",
			ExitCode: 0,
			Summary:  map[string]any{"packet_loss_pct": float64(0)},
		},
		CreatedAt: now,
	}
	if err := st.AddJobEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Parsed == nil || events[0].Parsed.Tool != model.ToolPing {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestSQLiteScheduledJobHistory(t *testing.T) {
	ctx := context.Background()
	st, err := NewSQLite(ctx, "sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Second)
	sched := model.ScheduledJob{
		ID:              "sched-1",
		Revision:        1,
		Enabled:         true,
		Tool:            model.ToolPing,
		Target:          "1.1.1.1",
		Args:            map[string]string{"count": "1"},
		IntervalSeconds: 60,
		NextRunAt:       now.Add(-time.Second),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := st.CreateScheduledJob(ctx, sched); err != nil {
		t.Fatal(err)
	}
	due, err := st.ListDueScheduledJobs(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != "sched-1" {
		t.Fatalf("unexpected due schedules: %#v", due)
	}

	job := model.Job{
		ID:                "job-scheduled-1",
		ScheduledID:       "sched-1",
		ScheduledRevision: 1,
		Tool:              model.ToolPing,
		Target:            "1.1.1.1",
		Args:              map[string]string{"count": "1"},
		Status:            model.JobQueued,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	oldJob := job
	oldJob.ID = "job-scheduled-old"
	oldJob.CreatedAt = now.Add(-2 * time.Hour)
	oldJob.UpdatedAt = oldJob.CreatedAt
	if err := st.CreateJob(ctx, oldJob); err != nil {
		t.Fatal(err)
	}
	delayedStart := now.Add(-time.Minute)
	delayedJob := job
	delayedJob.ID = "job-scheduled-delayed"
	delayedJob.Status = model.JobSucceeded
	delayedJob.CreatedAt = now.Add(-2 * time.Hour)
	delayedJob.StartedAt = &delayedStart
	delayedJob.UpdatedAt = delayedStart
	if err := st.CreateJob(ctx, delayedJob); err != nil {
		t.Fatal(err)
	}
	next := now.Add(time.Minute)
	if err := st.UpdateScheduledJobRun(ctx, "sched-1", now, next); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.GetScheduledJob(ctx, "sched-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastRunAt == nil || !loaded.NextRunAt.Equal(next) {
		t.Fatalf("schedule run timestamps not saved: %#v", loaded)
	}
	history, err := st.ListScheduledJobHistory(ctx, "sched-1", ScheduledJobHistoryFilter{Limit: 10, Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[0].ID != "job-scheduled-1" || history[1].ID != "job-scheduled-delayed" || history[0].ScheduledID != "sched-1" {
		t.Fatalf("unexpected history: %#v", history)
	}
	recent, err := st.ListScheduledJobHistory(ctx, "sched-1", ScheduledJobHistoryFilter{Limit: 10, Revision: 1, From: now.Add(-time.Hour), HasFrom: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 || recent[0].ID != "job-scheduled-1" || recent[1].ID != "job-scheduled-delayed" {
		t.Fatalf("unexpected filtered recent history: %#v", recent)
	}
	older, err := st.ListScheduledJobHistory(ctx, "sched-1", ScheduledJobHistoryFilter{Limit: 10, Revision: 1, To: now.Add(-time.Hour), HasTo: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 1 || older[0].ID != "job-scheduled-old" {
		t.Fatalf("unexpected filtered older history: %#v", older)
	}
	bucketed, err := st.ListScheduledJobHistory(ctx, "sched-1", ScheduledJobHistoryFilter{Limit: 10, Revision: 1, BucketSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if len(bucketed) != 2 {
		t.Fatalf("expected bucketed history to keep distinct hourly buckets: %#v", bucketed)
	}
	wrongToolJob := job
	wrongToolJob.ID = "job-scheduled-wrong-tool"
	wrongToolJob.ScheduledRevision = 2
	wrongToolJob.Tool = model.ToolDNS
	wrongToolJob.Target = "example.com"
	wrongToolJob.Args = map[string]string{"type": "A"}
	wrongToolJob.CreatedAt = now.Add(time.Second)
	wrongToolJob.UpdatedAt = wrongToolJob.CreatedAt
	if err := st.CreateJob(ctx, wrongToolJob); err != nil {
		t.Fatal(err)
	}
	currentDefinition, err := st.ListScheduledJobHistory(ctx, "sched-1", ScheduledJobHistoryFilter{Limit: 10, Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(currentDefinition) != 3 {
		t.Fatalf("expected definition filter to exclude old schedule definitions: %#v", currentDefinition)
	}
}

func TestMemoryRejectsOrphanJobEvent(t *testing.T) {
	st := NewMemory()
	err := st.AddJobEvent(context.Background(), model.JobEvent{
		ID:    "event-1",
		JobID: "missing-job",
	})
	if err != ErrNotFound {
		t.Fatalf("err = %v, want %v", err, ErrNotFound)
	}
}
