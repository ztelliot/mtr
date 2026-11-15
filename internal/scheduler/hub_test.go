package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/grpcwire"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
	"google.golang.org/grpc/metadata"
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

func registerHubTestGRPCSession(t *testing.T, hub *Hub, agentID string, jobIDs ...string) (grpcAgentSession, chan string) {
	t.Helper()
	capacity := len(jobIDs)
	if capacity < 1 {
		capacity = 1
	}
	cancelCh := make(chan string, capacity*2)
	unlockAgent := hub.lockAgentLifecycles(agentID)
	session, superseded := hub.registerGRPCSession(agentID, &grpcwire.AgentHello{
		ID:                agentID,
		ProtocolVersion:   grpcwire.AgentProtocolVersion,
		Capabilities:      []model.Tool{model.ToolPing, model.ToolTraceroute, model.ToolMTR, model.ToolHTTP},
		Protocols:         model.ProtocolAll,
		MaxConcurrentJobs: capacity,
	}, time.Now().UTC(), cancelCh)
	unlockAgent()
	if len(superseded) != 0 {
		t.Fatalf("test gRPC session unexpectedly superseded jobs: %#v", superseded)
	}
	for _, jobID := range jobIDs {
		if !hub.markInflightForGRPCSession(agentID, jobID, session.generation) {
			t.Fatalf("mark test gRPC job %q inflight", jobID)
		}
	}
	return session, cancelCh
}

func TestDeleteManagedAgentRevokesCurrentGRPCSession(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	agent := model.Agent{
		ID: "edge-delete", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll,
		Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now,
	}
	upsertHubTestAgent(t, ctx, st, agent)
	job := model.Job{
		ID: "delete-running", AgentID: agent.ID, Tool: model.ToolPing, Target: "1.1.1.1",
		Status: model.JobRunning, CreatedAt: now, UpdatedAt: now, StartedAt: &now,
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	session, _ := registerHubTestGRPCSession(t, hub, agent.ID, job.ID)
	if err := hub.DeleteManagedAgent(ctx, agent.ID, func() error {
		return st.DeleteAgent(ctx, agent.ID)
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.done:
	default:
		t.Fatal("deleted gRPC session remained active")
	}
	if hub.isCurrentGRPCSession(agent.ID, session.generation) {
		t.Fatal("deleted gRPC generation remained registered")
	}
	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobFailed || got.Error != model.JobErrorAgentDisconnected {
		t.Fatalf("running job after agent deletion = %#v", got)
	}
}

func TestDeleteManagedAgentWaitsForInProgressGRPCDispatch(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	agent := model.Agent{
		ID: "edge-delete-send", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll,
		Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now,
	}
	upsertHubTestAgent(t, ctx, st, agent)
	job := model.Job{
		ID: "delete-send-job", AgentID: agent.ID, Tool: model.ToolPing, Target: "1.1.1.1",
		Args: map[string]string{"protocol": "icmp"}, Status: model.JobQueued, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	session, _ := registerHubTestGRPCSession(t, hub, agent.ID)
	stream := newBlockingHubSendStream(ctx)
	dispatchDone := make(chan error, 1)
	errCh := make(chan error, 1)
	go func() {
		_, _, err := hub.dispatchGRPCJobsOnce(stream, agent.ID, session, errCh)
		dispatchDone <- err
	}()
	select {
	case <-stream.entered:
	case <-time.After(time.Second):
		t.Fatal("gRPC dispatch did not reach stream.Send")
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- hub.DeleteManagedAgent(ctx, agent.ID, func() error {
			return st.DeleteAgent(ctx, agent.ID)
		})
	}()
	select {
	case err := <-deleteDone:
		t.Fatalf("delete returned before in-progress send completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(stream.release)
	if err := <-dispatchDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-stream.sent:
		if message.Job == nil || message.Job.ID != job.ID {
			t.Fatalf("dispatched message = %#v", message)
		}
	default:
		t.Fatal("stream.Send did not complete before delete returned")
	}
	if _, current, err := hub.dispatchGRPCJobsOnce(stream, agent.ID, session, errCh); err != nil || current {
		t.Fatalf("dispatch after delete: current=%v err=%v", current, err)
	}
	select {
	case message := <-stream.sent:
		t.Fatalf("deleted generation received another message: %#v", message)
	default:
	}
}

func TestBlockedGRPCSendTimesOutAndReleasesManagedAgentDeletion(t *testing.T) {
	ctx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	agent := model.Agent{
		ID: "edge-stalled-send", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll,
		Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now,
	}
	upsertHubTestAgent(t, ctx, st, agent)
	job := model.Job{
		ID: "stalled-send-job", AgentID: agent.ID, Tool: model.ToolPing, Target: "1.1.1.1",
		Args: map[string]string{"protocol": "icmp"}, Status: model.JobQueued, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hub.grpcSendTimeout = 20 * time.Millisecond
	session, _ := registerHubTestGRPCSession(t, hub, agent.ID)
	stream := newBlockingHubSendStream(ctx)
	errCh := make(chan error, 2)
	dispatchDone := make(chan error, 1)
	go func() {
		_, _, err := hub.dispatchGRPCJobsOnce(stream, agent.ID, session, errCh)
		dispatchDone <- err
	}()
	select {
	case <-stream.entered:
	case <-time.After(time.Second):
		t.Fatal("gRPC dispatch did not reach stream.Send")
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- hub.DeleteManagedAgent(context.Background(), agent.ID, func() error {
			return st.DeleteAgent(context.Background(), agent.ID)
		})
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, errGRPCSendTimeout) {
			t.Fatalf("send watchdog error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked gRPC send did not time out")
	}
	// This mirrors gRPC canceling the stream context as soon as Connect returns
	// the timeout reported above.
	cancelStream()
	select {
	case err := <-dispatchDone:
		if !errors.Is(err, errGRPCSendTimeout) {
			t.Fatalf("dispatch error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed-out dispatch did not release lifecycle ownership")
	}
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("managed Agent deletion remained blocked after send timeout")
	}
	select {
	case message := <-stream.sent:
		t.Fatalf("timed-out stream delivered a message: %#v", message)
	default:
	}
}

func TestScheduledFanoutRejectsTargetsBeyondHardLimit(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	for index := 0; index <= model.MaxFanoutTargets; index++ {
		upsertHubTestAgent(t, ctx, st, model.Agent{
			ID: fmt.Sprintf("edge-%02d", index), Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll,
			Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now,
		})
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := hub.scheduledRunAgents(ctx, model.ScheduledJob{
		Tool: model.ToolPing, Target: "1.1.1.1", ResolveOnAgent: true,
	}, model.ScheduleTarget{
		Label:      model.AgentAllLabel,
		AgentScope: model.ScheduleAgentScope{All: true},
	}, model.IPv4)
	if err == nil || !strings.Contains(err.Error(), "fanout target limit exceeded") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunDueSchedulesRejectsAggregateFanoutAcrossLabels(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC().Add(-time.Minute)
	for index := 0; index <= model.MaxFanoutTargets; index++ {
		agentID := fmt.Sprintf("aggregate-%02d", index)
		upsertHubTestAgent(t, ctx, st, model.Agent{
			ID: agentID, Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll,
			Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now,
		})
		label := "blue"
		if index >= model.MaxFanoutTargets/2 {
			label = "green"
		}
		upsertHubTestAgentConfigLabels(t, ctx, st, agentID, []string{label})
	}
	sched := model.ScheduledJob{
		ID: "sched-aggregate-limit", Revision: 1, Enabled: true, Tool: model.ToolPing, Target: "1.1.1.1",
		CronExpression: "* * * * *", NextRunAt: now, CreatedAt: now, UpdatedAt: now,
		ScheduleTargets: []model.ScheduleTarget{
			{Label: "blue", AgentScope: model.ScheduleAgentScope{All: true}},
			{Label: "green", AgentScope: model.ScheduleAgentScope{All: true}},
		},
	}
	if err := st.CreateScheduledJob(ctx, sched); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hub.runDueSchedules(ctx)
	jobs, err := st.ListActiveJobs(ctx, model.MaxFanoutTargets+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("aggregate fanout created partial jobs: %#v", jobs)
	}
	loaded, err := st.GetScheduledJob(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.NextRunAt.After(sched.NextRunAt) {
		t.Fatalf("over-limit schedule was not advanced: before=%s after=%s", sched.NextRunAt, loaded.NextRunAt)
	}
}

func hubTestGRPCSessionOwnsJob(hub *Hub, agentID string, jobID string, generation uint64) bool {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	_, running := hub.running[agentID][jobID]
	meta, current := hub.meta[agentID]
	return running && current && meta.grpcGeneration == generation && hub.grpcJobGenerations[jobID] == generation
}

func setHubTestInflightCancel(hub *Hub, jobID string, cancel context.CancelFunc) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.inflightCancels[jobID] = cancel
}

func createHubTestRunningGRPCJob(t *testing.T, id string) (context.Context, store.Store, *Hub, model.Job, grpcAgentSession, chan string) {
	t.Helper()
	ctx := context.Background()
	st := store.NewTestMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()
	job := model.Job{
		ID: id, AgentID: "edge-" + id, Tool: model.ToolPing, Target: "1.1.1.1",
		Status: model.JobRunning, CreatedAt: now, UpdatedAt: now, StartedAt: &now,
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	session, cancelCh := registerHubTestGRPCSession(t, hub, job.AgentID, job.ID)
	return ctx, st, hub, job, session, cancelCh
}

func countHubTestStreamEvents(t *testing.T, ctx context.Context, st store.Store, jobID string, stream string) int {
	t.Helper()
	events, err := st.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Stream == stream {
			count++
		}
	}
	return count
}

func hubTestEventWithJSONSize(t *testing.T, size int) map[string]any {
	t.Helper()
	payload := map[string]any{"type": "stdout", "message": ""}
	base, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if size < len(base) {
		t.Fatalf("event JSON size %d is below minimum %d", size, len(base))
	}
	payload["message"] = strings.Repeat("x", size-len(base))
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != size {
		t.Fatalf("event JSON size = %d, want %d", len(encoded), size)
	}
	return payload
}

func TestAgentLifecycleLocksAreReleasedAfterUse(t *testing.T) {
	hub := NewHub(store.NewTestMemory(), policy.DefaultPolicies(), time.Second, time.Second, 1, slog.Default())
	unlock := hub.lockAgentLifecycles("edge-b", "edge-a", "edge-a")
	hub.agentLifecycleMu.Lock()
	if len(hub.agentLifecycles) != 2 {
		t.Fatalf("lifecycle lock count while held = %d", len(hub.agentLifecycles))
	}
	hub.agentLifecycleMu.Unlock()

	unlock()
	hub.agentLifecycleMu.Lock()
	defer hub.agentLifecycleMu.Unlock()
	if len(hub.agentLifecycles) != 0 {
		t.Fatalf("lifecycle locks retained after release: %d", len(hub.agentLifecycles))
	}
}

func TestFractionalSummaryExitCodeCannotCompleteJob(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.Default())
	job := model.Job{
		ID: "fractional-exit", AgentID: "edge-1", Tool: model.ToolPing,
		Target: "1.1.1.1", Status: model.JobRunning, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	session, _ := registerHubTestGRPCSession(t, hub, job.AgentID, job.ID)
	hub.handleResultForSession(ctx, job.AgentID, session.generation, grpcwire.ResultEvent{
		JobID: job.ID,
		Event: map[string]any{"type": "summary", "exit_code": 0.5},
	})

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobRunning {
		t.Fatalf("fractional exit code changed job status to %s", got.Status)
	}
	events, err := st.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("fractional exit code stored events: %#v", events)
	}
}

func TestGRPCResultEventLimitAllows1024NonSummaryEventsAndSummary(t *testing.T) {
	ctx, st, hub, job, session, _ := createHubTestRunningGRPCJob(t, "event-limit-allowed")
	for i := 0; i < grpcAgentResultEventLimit; i++ {
		hub.handleResultForSession(ctx, job.AgentID, session.generation, grpcwire.ResultEvent{
			JobID: job.ID,
			Event: map[string]any{"type": "stdout", "message": "ok"},
		})
	}
	hub.handleResultForSession(ctx, job.AgentID, session.generation, grpcwire.ResultEvent{
		JobID: job.ID,
		Event: map[string]any{"type": "summary", "exit_code": 0},
	})

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobSucceeded {
		t.Fatalf("job status = %s, want %s", got.Status, model.JobSucceeded)
	}
	if got := countHubTestStreamEvents(t, ctx, st, job.ID, "stdout"); got != grpcAgentResultEventLimit {
		t.Fatalf("stored stdout events = %d, want %d", got, grpcAgentResultEventLimit)
	}
	hub.mu.Lock()
	_, budgetExists := hub.grpcResultBudgets[job.ID]
	hub.mu.Unlock()
	if budgetExists {
		t.Fatal("completed job retained its gRPC result budget")
	}
}

func TestGRPCResultEventLimitRejects1025AndDropsLaterEvents(t *testing.T) {
	ctx, st, hub, job, session, cancelCh := createHubTestRunningGRPCJob(t, "event-limit-rejected")
	for i := 0; i < grpcAgentResultEventLimit; i++ {
		hub.handleResultForSession(ctx, job.AgentID, session.generation, grpcwire.ResultEvent{
			JobID: job.ID,
			Event: map[string]any{"type": "stdout", "message": "accepted"},
		})
	}
	hub.handleResultForSession(ctx, job.AgentID, session.generation, grpcwire.ResultEvent{
		JobID: job.ID,
		Event: map[string]any{"type": "stdout", "message": "over-limit"},
	})
	hub.handleResultForSession(ctx, job.AgentID, session.generation, grpcwire.ResultEvent{
		JobID: job.ID,
		Event: map[string]any{"type": "stdout", "message": "late"},
	})

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobFailed || got.Error != grpcAgentResultLimitExceededError {
		t.Fatalf("over-limit job = %#v", got)
	}
	if got := countHubTestStreamEvents(t, ctx, st, job.ID, "stdout"); got != grpcAgentResultEventLimit {
		t.Fatalf("stored stdout events = %d, want %d", got, grpcAgentResultEventLimit)
	}
	select {
	case canceledJobID := <-cancelCh:
		if canceledJobID != job.ID {
			t.Fatalf("canceled job = %q, want %q", canceledJobID, job.ID)
		}
	default:
		t.Fatal("over-limit job was not canceled on the gRPC agent")
	}
	hub.mu.Lock()
	_, running := hub.running[job.AgentID][job.ID]
	_, budgetExists := hub.grpcResultBudgets[job.ID]
	hub.mu.Unlock()
	if running || budgetExists {
		t.Fatalf("over-limit cleanup retained running=%t budget=%t", running, budgetExists)
	}
}

func TestGRPCResultLimitFailureRestoresExceededTombstoneAfterStoreError(t *testing.T) {
	ctx := context.Background()
	base := store.NewTestMemory()
	failing := &failActiveJobOnceStore{Store: base, err: errors.New("temporary failure"), fail: true}
	hub := NewHub(failing, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()
	job := model.Job{ID: "result-limit-store-error", AgentID: "edge-result-limit-store-error", Tool: model.ToolPing, Target: "1.1.1.1", Status: model.JobRunning, CreatedAt: now, UpdatedAt: now, StartedAt: &now}
	if err := base.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	session, _ := registerHubTestGRPCSession(t, hub, job.AgentID, job.ID)
	hub.mu.Lock()
	budget := hub.grpcResultBudgets[job.ID]
	budget.nonSummaryEvents = grpcAgentResultEventLimit
	hub.grpcResultBudgets[job.ID] = budget
	hub.mu.Unlock()

	overLimit := grpcwire.ResultEvent{JobID: job.ID, Event: map[string]any{"type": "stdout", "message": "must never persist"}}
	hub.handleResultForSession(ctx, job.AgentID, session.generation, overLimit)
	got, err := base.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	hub.mu.Lock()
	restoredBudget, budgetExists := hub.grpcResultBudgets[job.ID]
	hub.mu.Unlock()
	if got.Status != model.JobRunning || !budgetExists || !restoredBudget.exceeded {
		t.Fatalf("failed limit transaction status=%s budget=%#v exists=%t", got.Status, restoredBudget, budgetExists)
	}
	if got := countHubTestStreamEvents(t, ctx, base, job.ID, "stdout"); got != 0 {
		t.Fatalf("triggering over-limit event persisted %d times", got)
	}

	hub.handleResultForSession(ctx, job.AgentID, session.generation, overLimit)
	got, err = base.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobFailed || got.Error != grpcAgentResultLimitExceededError {
		t.Fatalf("retried result-limit failure = %#v", got)
	}
	if got := countHubTestStreamEvents(t, ctx, base, job.ID, "stdout"); got != 0 {
		t.Fatalf("event after initial limit violation persisted %d times", got)
	}
}

func TestGRPCResultByteLimitCountsSummaryAndAllowsExactBoundary(t *testing.T) {
	summary := map[string]any{"type": "summary", "exit_code": 0}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}

	sendProgressBytes := func(t *testing.T, hub *Hub, ctx context.Context, job model.Job, generation uint64, total int) {
		t.Helper()
		const chunks = 5
		for chunk := 0; chunk < chunks; chunk++ {
			size := total / chunks
			if chunk < total%chunks {
				size++
			}
			hub.handleResultForSession(ctx, job.AgentID, generation, grpcwire.ResultEvent{
				JobID: job.ID,
				Event: hubTestEventWithJSONSize(t, size),
			})
		}
	}

	t.Run("exact 4MiB including summary", func(t *testing.T) {
		ctx, st, hub, job, session, _ := createHubTestRunningGRPCJob(t, "byte-limit-exact")
		sendProgressBytes(t, hub, ctx, job, session.generation, grpcAgentResultBytesLimit-len(summaryJSON))
		hub.handleResultForSession(ctx, job.AgentID, session.generation, grpcwire.ResultEvent{JobID: job.ID, Event: summary})
		got, err := st.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != model.JobSucceeded {
			t.Fatalf("job status at exact byte limit = %s", got.Status)
		}
	})

	t.Run("summary exceeds 4MiB", func(t *testing.T) {
		ctx, st, hub, job, session, cancelCh := createHubTestRunningGRPCJob(t, "byte-limit-exceeded")
		sendProgressBytes(t, hub, ctx, job, session.generation, grpcAgentResultBytesLimit-len(summaryJSON)+1)
		hub.handleResultForSession(ctx, job.AgentID, session.generation, grpcwire.ResultEvent{JobID: job.ID, Event: summary})
		got, err := st.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != model.JobFailed || got.Error != grpcAgentResultLimitExceededError {
			t.Fatalf("job beyond byte limit = %#v", got)
		}
		select {
		case <-cancelCh:
		default:
			t.Fatal("byte-limit failure did not cancel the gRPC job")
		}
	})
}

func TestStaleGRPCGenerationCannotConsumeReplacementResultBudget(t *testing.T) {
	ctx, st, hub, job, oldSession, _ := createHubTestRunningGRPCJob(t, "stale-budget")
	newSession, superseded := hub.registerGRPCSession(job.AgentID, &grpcwire.AgentHello{
		ID:                job.AgentID,
		ProtocolVersion:   grpcwire.AgentProtocolVersion,
		Capabilities:      []model.Tool{model.ToolPing},
		Protocols:         model.ProtocolAll,
		MaxConcurrentJobs: 1,
	}, time.Now().UTC(), make(chan string, 2))
	if len(superseded) != 1 || superseded[0] != job.ID {
		t.Fatalf("superseded jobs = %#v", superseded)
	}
	if !hub.markInflightForGRPCSession(job.AgentID, job.ID, newSession.generation) {
		t.Fatal("replacement session did not claim job")
	}

	hub.handleResultForSession(ctx, job.AgentID, oldSession.generation, grpcwire.ResultEvent{
		JobID: job.ID,
		Event: map[string]any{"type": "stdout", "message": strings.Repeat("x", 1024)},
	})
	hub.mu.Lock()
	budget := hub.grpcResultBudgets[job.ID]
	hub.mu.Unlock()
	if budget.generation != newSession.generation || budget.nonSummaryEvents != 0 || budget.totalBytes != 0 || budget.exceeded {
		t.Fatalf("stale generation changed replacement budget: %#v", budget)
	}
	if got := countHubTestStreamEvents(t, ctx, st, job.ID, "stdout"); got != 0 {
		t.Fatalf("stale generation stored %d events", got)
	}
}

func TestStaleGRPCSessionCannotClearReplacementMetadataOrJobs(t *testing.T) {
	hub := NewHub(store.NewTestMemory(), policy.DefaultPolicies(), time.Minute, time.Second, 1, slog.Default())
	now := time.Now().UTC()
	oldCancelCh := make(chan string, 1)
	oldSession, superseded := hub.registerGRPCSession("edge-reconnect", &grpcwire.AgentHello{
		ID:           "edge-reconnect",
		Capabilities: []model.Tool{model.ToolPing},
		Protocols:    model.ProtocolAll,
	}, now, oldCancelCh)
	if len(superseded) != 0 || !hub.markInflightForGRPCSession("edge-reconnect", "old-job", oldSession.generation) {
		t.Fatalf("old session registration failed: superseded=%#v", superseded)
	}

	newCancelCh := make(chan string, 1)
	newSession, superseded := hub.registerGRPCSession("edge-reconnect", &grpcwire.AgentHello{
		ID:                "edge-reconnect",
		Capabilities:      []model.Tool{model.ToolTraceroute},
		Protocols:         model.ProtocolAll,
		MaxConcurrentJobs: 1,
	}, now.Add(time.Second), newCancelCh)
	if len(superseded) != 1 || superseded[0] != "old-job" {
		t.Fatalf("superseded jobs = %#v", superseded)
	}
	select {
	case <-oldSession.done:
	default:
		t.Fatal("replacement did not stop the stale send loop")
	}
	if !hub.markInflightForGRPCSession("edge-reconnect", "new-job", newSession.generation) {
		t.Fatal("new session did not own its job")
	}

	if jobs, current := hub.unregisterGRPCSession("edge-reconnect", oldSession.generation); current || len(jobs) != 0 {
		t.Fatalf("stale cleanup current=%t jobs=%#v", current, jobs)
	}
	hub.mu.Lock()
	gotCancelCh := hub.grpcCancels["edge-reconnect"]
	_, newRunning := hub.running["edge-reconnect"]["new-job"]
	newGeneration := hub.grpcJobGenerations["new-job"]
	_, oldBudgetExists := hub.grpcResultBudgets["old-job"]
	newBudget := hub.grpcResultBudgets["new-job"]
	hub.mu.Unlock()
	if gotCancelCh != newCancelCh || !newRunning || newGeneration != newSession.generation || oldBudgetExists || newBudget.generation != newSession.generation {
		t.Fatalf("stale cleanup changed replacement state: cancel=%p running=%t generation=%d old_budget=%t new_budget=%#v", gotCancelCh, newRunning, newGeneration, oldBudgetExists, newBudget)
	}
}

func TestDisconnectedGRPCCleanupStopsAtDeadline(t *testing.T) {
	blocking := &blockingMarkOfflineStore{Store: store.NewTestMemory(), called: make(chan struct{})}
	hub := NewHub(blocking, policy.DefaultPolicies(), time.Minute, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		hub.cleanupDisconnectedGRPCSession(ctx, "edge-slow-store", []string{"running-job"})
		close(done)
	}()
	<-blocking.called
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gRPC disconnect cleanup remained blocked after its deadline")
	}
}

type blockingMarkOfflineStore struct {
	store.Store
	called chan struct{}
}

func (s *blockingMarkOfflineStore) MarkAgentOffline(ctx context.Context, _ string) error {
	close(s.called)
	<-ctx.Done()
	return ctx.Err()
}

func TestCompatibleQueuedJobPolicyUsesCurrentAgentProtocol(t *testing.T) {
	capabilities := []model.Tool{model.ToolPing, model.ToolTraceroute, model.ToolMTR, model.ToolHTTP}
	policies := policy.DefaultPolicies()

	tests := []struct {
		name        string
		policies    policy.Set
		job         model.Job
		wantFailure string
	}{
		{name: "traceroute udp", policies: policies, job: model.Job{ID: "route", AgentID: "agent", Tool: model.ToolTraceroute, Target: "1.1.1.1", Args: map[string]string{"protocol": "udp"}}},
		{name: "http auto", policies: policies, job: model.Job{ID: "http", AgentID: "agent", Tool: model.ToolHTTP, Target: "https://example.com", Args: map[string]string{"http_version": "auto"}}},
		{name: "missing capability", policies: policies, job: model.Job{ID: "dns", AgentID: "agent", Tool: model.ToolDNS, Target: "example.com"}, wantFailure: model.JobErrorUnsupportedTool},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, failure, err := compatibleQueuedJobPolicy(tt.policies, capabilities, model.ProtocolAll, tt.job)
			if tt.wantFailure == "" {
				if err != nil || failure != "" {
					t.Fatalf("unexpected rejection failure=%q err=%v", failure, err)
				}
				return
			}
			if err == nil || failure != tt.wantFailure {
				t.Fatalf("failure=%q err=%v, want %q", failure, err, tt.wantFailure)
			}
		})
	}
}

func TestEffectiveAgentInflightLimitUsesAdvertisedCapacity(t *testing.T) {
	for _, tt := range []struct {
		configured int
		advertised int
		want       int
	}{
		{configured: 8, advertised: 4, want: 4},
		{configured: 2, advertised: 4, want: 2},
		{configured: 8, advertised: 0, want: 0},
	} {
		if got := effectiveAgentInflightLimit(tt.configured, tt.advertised); got != tt.want {
			t.Fatalf("effectiveAgentInflightLimit(%d, %d)=%d, want %d", tt.configured, tt.advertised, got, tt.want)
		}
	}
}

func TestValidateAgentWireMetadataRequiresCurrentCompleteMetadata(t *testing.T) {
	valid := func() (int, []model.Tool, model.ProtocolMask, int) {
		return grpcwire.AgentProtocolVersion, []model.Tool{model.ToolPing}, model.ProtocolAll, 4
	}
	version, capabilities, protocols, capacity := valid()
	if err := validateAgentWireMetadata(version, capabilities, protocols, capacity); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	for name, mutate := range map[string]func() (int, []model.Tool, model.ProtocolMask, int){
		"missing version": func() (int, []model.Tool, model.ProtocolMask, int) { return 0, capabilities, protocols, capacity },
		"no capabilities": func() (int, []model.Tool, model.ProtocolMask, int) { return version, nil, protocols, capacity },
		"unknown capability": func() (int, []model.Tool, model.ProtocolMask, int) {
			return version, []model.Tool{"unknown"}, protocols, capacity
		},
		"no protocols": func() (int, []model.Tool, model.ProtocolMask, int) { return version, capabilities, 0, capacity },
		"no capacity":  func() (int, []model.Tool, model.ProtocolMask, int) { return version, capabilities, protocols, 0 },
	} {
		t.Run(name, func(t *testing.T) {
			v, c, p, maxJobs := mutate()
			if err := validateAgentWireMetadata(v, c, p, maxJobs); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

func TestValidateAgentMessageShapeRejectsInvalidUnions(t *testing.T) {
	validResult := &grpcwire.AgentResult{JobID: "job-1", Event: map[string]any{"type": "stdout"}}
	tests := []struct {
		name    string
		message *grpcwire.AgentMessage
		valid   bool
	}{
		{name: "hello", message: &grpcwire.AgentMessage{Type: "hello", Agent: &grpcwire.AgentHello{}}, valid: true},
		{name: "heartbeat", message: &grpcwire.AgentMessage{Type: "heartbeat", Heartbeat: &grpcwire.Heartbeat{}}, valid: true},
		{name: "result", message: &grpcwire.AgentMessage{Type: "result", Result: validResult}, valid: true},
		{name: "nil message"},
		{name: "unknown", message: &grpcwire.AgentMessage{Type: "wat"}},
		{name: "heartbeat missing payload", message: &grpcwire.AgentMessage{Type: "heartbeat"}},
		{name: "heartbeat with extra payload", message: &grpcwire.AgentMessage{Type: "heartbeat", Heartbeat: &grpcwire.Heartbeat{}, Result: validResult}},
		{name: "result missing payload", message: &grpcwire.AgentMessage{Type: "result"}},
		{name: "result empty type", message: &grpcwire.AgentMessage{Type: "result", Result: &grpcwire.AgentResult{JobID: "job-1", Event: map[string]any{"type": ""}}}},
		{name: "result wrong type", message: &grpcwire.AgentMessage{Type: "result", Result: &grpcwire.AgentResult{JobID: "job-1", Event: map[string]any{"type": 1}}}},
		{
			name: "result missing job id",
			message: &grpcwire.AgentMessage{
				Type:   "result",
				Result: &grpcwire.AgentResult{Event: map[string]any{"type": "stdout"}},
			},
		},
		{name: "result nil event", message: &grpcwire.AgentMessage{Type: "result", Result: &grpcwire.AgentResult{JobID: "job-1"}}},
		{
			name: "hello with extra payload",
			message: &grpcwire.AgentMessage{
				Type:      "hello",
				Agent:     &grpcwire.AgentHello{},
				Heartbeat: &grpcwire.Heartbeat{},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAgentMessageShape(tt.message)
			if tt.valid && err != nil {
				t.Fatalf("valid message rejected: %v", err)
			}
			if !tt.valid && err == nil {
				t.Fatal("invalid message accepted")
			}
		})
	}
}

func TestGRPCRecvLoopRejectsHeartbeatFlood(t *testing.T) {
	hub := NewHub(store.NewTestMemory(), policy.DefaultPolicies(), time.Minute, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	session, _ := registerHubTestGRPCSession(t, hub, "edge-heartbeat")
	stream := &hubRecvTestStream{
		ctx: context.Background(),
		messages: []*grpcwire.AgentMessage{
			{Type: "heartbeat", Heartbeat: &grpcwire.Heartbeat{}},
			{Type: "heartbeat", Heartbeat: &grpcwire.Heartbeat{}},
		},
	}
	errCh := make(chan error, 1)
	hub.recvLoop(stream, "edge-heartbeat", session.generation, errCh)
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "heartbeat interval") {
		t.Fatalf("heartbeat flood error = %v", err)
	}
}

func TestGRPCRecvLoopDropsUnownedResultsBeforeStoreLookup(t *testing.T) {
	counting := &getJobCountingStore{Store: store.NewTestMemory()}
	hub := NewHub(counting, policy.DefaultPolicies(), time.Minute, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	session, _ := registerHubTestGRPCSession(t, hub, "edge-unowned")
	stream := &hubRecvTestStream{
		ctx: context.Background(),
		messages: []*grpcwire.AgentMessage{{
			Type:   "result",
			Result: &grpcwire.AgentResult{JobID: "random-job-id", Event: map[string]any{"type": "stdout", "message": "noise"}},
		}},
	}
	errCh := make(chan error, 1)
	hub.recvLoop(stream, "edge-unowned", session.generation, errCh)
	if err := <-errCh; err != nil {
		t.Fatalf("recv loop returned %v", err)
	}
	counting.mu.Lock()
	getJobCalls := counting.getJobCalls
	counting.mu.Unlock()
	if getJobCalls != 0 {
		t.Fatalf("unowned result caused %d GetJob calls", getJobCalls)
	}
}

func TestGRPCRecvLoopRejectsUnknownOrNilPayload(t *testing.T) {
	for name, message := range map[string]*grpcwire.AgentMessage{
		"unknown":           {Type: "unknown"},
		"nil heartbeat":     {Type: "heartbeat"},
		"nil result":        {Type: "result"},
		"result nil event":  {Type: "result", Result: &grpcwire.AgentResult{JobID: "job-1"}},
		"result untyped":    {Type: "result", Result: &grpcwire.AgentResult{JobID: "job-1", Event: map[string]any{"message": "missing type"}}},
		"result wrong type": {Type: "result", Result: &grpcwire.AgentResult{JobID: "job-1", Event: map[string]any{"type": false}}},
	} {
		t.Run(name, func(t *testing.T) {
			hub := NewHub(store.NewTestMemory(), policy.DefaultPolicies(), time.Minute, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			session, _ := registerHubTestGRPCSession(t, hub, "edge-invalid-"+strings.ReplaceAll(name, " ", "-"))
			stream := &hubRecvTestStream{ctx: context.Background(), messages: []*grpcwire.AgentMessage{message}}
			errCh := make(chan error, 1)
			hub.recvLoop(stream, "edge-invalid-"+strings.ReplaceAll(name, " ", "-"), session.generation, errCh)
			if err := <-errCh; err == nil {
				t.Fatal("invalid message did not terminate recv loop")
			}
		})
	}
}

func TestGRPCSendLoopDispatchesCurrentProtocolJobs(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	capabilities := []model.Tool{model.ToolTraceroute}
	for _, agent := range []model.Agent{
		{ID: "old-grpc", Capabilities: capabilities, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "new-grpc", Capabilities: capabilities, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		if err := st.UpsertAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
		job := model.Job{ID: agent.ID + "-job", AgentID: agent.ID, Tool: model.ToolTraceroute, Target: "1.1.1.1", Args: map[string]string{"protocol": "udp"}, Status: model.JobQueued, CreatedAt: now, UpdatedAt: now}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Second, 1, slog.Default())
	oldCancelCh := make(chan string)
	oldSession, _ := hub.registerGRPCSession("old-grpc", &grpcwire.AgentHello{ID: "old-grpc", Capabilities: capabilities, Protocols: model.ProtocolAll, MaxConcurrentJobs: 1}, now, oldCancelCh)
	newCancelCh := make(chan string)
	newSession, _ := hub.registerGRPCSession("new-grpc", &grpcwire.AgentHello{ID: "new-grpc", Capabilities: capabilities, Protocols: model.ProtocolAll, MaxConcurrentJobs: 1}, now, newCancelCh)

	oldCtx, cancelOld := context.WithCancel(ctx)
	defer cancelOld()
	newCtx, cancelNew := context.WithCancel(ctx)
	defer cancelNew()
	oldStream := &hubTestConnectServer{ctx: oldCtx, sent: make(chan *grpcwire.ServerMessage, 1)}
	newStream := &hubTestConnectServer{ctx: newCtx, sent: make(chan *grpcwire.ServerMessage, 1)}
	go hub.sendLoop(oldStream, "old-grpc", oldSession, oldCancelCh, make(chan error, 1))
	go hub.sendLoop(newStream, "new-grpc", newSession, newCancelCh, make(chan error, 1))

	select {
	case message := <-newStream.sent:
		if message.Job == nil || message.Job.ID != "new-grpc-job" || message.Job.Args["protocol"] != "udp" {
			t.Fatalf("new agent dispatch = %#v", message)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("new gRPC agent did not receive current-protocol job")
	}
	select {
	case message := <-oldStream.sent:
		if message.Job == nil || message.Job.ID != "old-grpc-job" || message.Job.Args["protocol"] != "udp" {
			t.Fatalf("peer dispatch = %#v", message)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("peer gRPC agent did not receive current-protocol job")
	}
}

type hubTestConnectServer struct {
	ctx  context.Context
	sent chan *grpcwire.ServerMessage
}

type blockingHubSendStream struct {
	ctx     context.Context
	entered chan struct{}
	release chan struct{}
	sent    chan *grpcwire.ServerMessage
	once    sync.Once
}

func newBlockingHubSendStream(ctx context.Context) *blockingHubSendStream {
	return &blockingHubSendStream{
		ctx:     ctx,
		entered: make(chan struct{}),
		release: make(chan struct{}),
		sent:    make(chan *grpcwire.ServerMessage, 2),
	}
}

func (s *blockingHubSendStream) Send(message *grpcwire.ServerMessage) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
	s.sent <- message
	return nil
}

func (s *blockingHubSendStream) Recv() (*grpcwire.AgentMessage, error) { return nil, io.EOF }
func (s *blockingHubSendStream) SetHeader(metadata.MD) error           { return nil }
func (s *blockingHubSendStream) SendHeader(metadata.MD) error          { return nil }
func (s *blockingHubSendStream) SetTrailer(metadata.MD)                {}
func (s *blockingHubSendStream) Context() context.Context              { return s.ctx }
func (s *blockingHubSendStream) SendMsg(any) error                     { return nil }
func (s *blockingHubSendStream) RecvMsg(any) error                     { return io.EOF }

func (s *hubTestConnectServer) Send(message *grpcwire.ServerMessage) error {
	s.sent <- message
	return nil
}

func (s *hubTestConnectServer) Recv() (*grpcwire.AgentMessage, error) {
	return nil, io.EOF
}

func (s *hubTestConnectServer) SetHeader(metadata.MD) error  { return nil }
func (s *hubTestConnectServer) SendHeader(metadata.MD) error { return nil }
func (s *hubTestConnectServer) SetTrailer(metadata.MD)       {}
func (s *hubTestConnectServer) Context() context.Context     { return s.ctx }
func (s *hubTestConnectServer) SendMsg(any) error            { return nil }
func (s *hubTestConnectServer) RecvMsg(any) error            { return io.EOF }

type hubRecvTestStream struct {
	ctx      context.Context
	messages []*grpcwire.AgentMessage
	next     int
}

func (s *hubRecvTestStream) Send(*grpcwire.ServerMessage) error { return nil }

func (s *hubRecvTestStream) Recv() (*grpcwire.AgentMessage, error) {
	if s.next >= len(s.messages) {
		return nil, io.EOF
	}
	message := s.messages[s.next]
	s.next++
	return message, nil
}

func (s *hubRecvTestStream) SetHeader(metadata.MD) error  { return nil }
func (s *hubRecvTestStream) SendHeader(metadata.MD) error { return nil }
func (s *hubRecvTestStream) SetTrailer(metadata.MD)       {}
func (s *hubRecvTestStream) Context() context.Context     { return s.ctx }
func (s *hubRecvTestStream) SendMsg(any) error            { return nil }
func (s *hubRecvTestStream) RecvMsg(any) error            { return io.EOF }

type getJobCountingStore struct {
	store.Store
	mu          sync.Mutex
	getJobCalls int
}

func (s *getJobCountingStore) GetJob(ctx context.Context, id string) (model.Job, error) {
	s.mu.Lock()
	s.getJobCalls++
	s.mu.Unlock()
	return s.Store.GetJob(ctx, id)
}

func TestFailTimedOutQueuedJobPublishesFailure(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
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

func TestRunDueSchedulesRunsAllLabelTargetsOnOneCronTick(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
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
		CronExpression: "* * * * *",
		NextRunAt:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
		ScheduleTargets: []model.ScheduleTarget{
			{Label: "blue", AgentScope: model.ScheduleAgentScope{All: true}},
			{Label: "red", AgentScope: model.ScheduleAgentScope{All: true}},
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
	if loaded.LastRunAt == nil || !loaded.NextRunAt.After(*loaded.LastRunAt) || len(loaded.ScheduleTargets) != 2 {
		t.Fatalf("schedule cron state not advanced: %#v", loaded)
	}
	if loaded.NextRunAt.Location() != time.UTC || loaded.NextRunAt.Second() != 0 || loaded.NextRunAt.Nanosecond() != 0 {
		t.Fatalf("next cron tick is not aligned to a UTC minute: %s", loaded.NextRunAt)
	}
}

func TestRunDueSchedulesUsesCurrentProtocolForAllOnlineAgents(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC().Add(-time.Minute)
	capabilities := []model.Tool{model.ToolTraceroute}
	for _, id := range []string{"old-route", "new-route"} {
		upsertHubTestAgent(t, ctx, st, model.Agent{ID: id, Capabilities: capabilities, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now})
	}
	sched := model.ScheduledJob{
		ID:             "sched-udp-route",
		Revision:       1,
		Enabled:        true,
		Tool:           model.ToolTraceroute,
		Target:         "1.1.1.1",
		Args:           map[string]string{"protocol": "udp"},
		ResolveOnAgent: true,
		CronExpression: "* * * * *",
		NextRunAt:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
		ScheduleTargets: []model.ScheduleTarget{
			{Label: model.AgentAllLabel, AgentScope: model.ScheduleAgentScope{All: true}},
		},
	}
	if err := st.CreateScheduledJob(ctx, sched); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())
	hub.mu.Lock()
	hub.meta["old-route"] = agentMeta{capabilities: capabilities, protocols: model.ProtocolAll, lastSeen: time.Now()}
	hub.meta["new-route"] = agentMeta{capabilities: capabilities, protocols: model.ProtocolAll, lastSeen: time.Now()}
	hub.mu.Unlock()

	hub.runDueSchedules(ctx)

	jobs, err := st.ListActiveJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].Args["protocol"] != "udp" || jobs[1].Args["protocol"] != "udp" {
		t.Fatalf("scheduled jobs = %#v", jobs)
	}
}

func TestRunDueSchedulesDisablesInvalidCronWithoutCreatingJobs(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC().Add(-time.Minute)
	sched := model.ScheduledJob{
		ID: "sched-invalid-cron", Revision: 1, Enabled: true, Tool: model.ToolPing, Target: "1.1.1.1",
		CronExpression: "61 * * * *", NextRunAt: now, CreatedAt: now, UpdatedAt: now,
		ScheduleTargets: []model.ScheduleTarget{{Label: "agent", AgentScope: model.ScheduleAgentScope{All: true}}},
	}
	if err := st.CreateScheduledJob(ctx, sched); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())

	hub.runDueSchedules(ctx)

	loaded, err := st.GetScheduledJob(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Enabled {
		t.Fatalf("invalid cron schedule remained enabled: %#v", loaded)
	}
	jobs, err := st.ListActiveJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("invalid cron schedule created jobs: %#v", jobs)
	}
}

func TestScheduledIDLabelsMatchOnlyRealAgentID(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
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
		ID:             "sched-id-label",
		Revision:       1,
		Enabled:        true,
		Tool:           model.ToolPing,
		Target:         "1.1.1.1",
		CronExpression: "* * * * *",
		NextRunAt:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
		ScheduleTargets: []model.ScheduleTarget{
			{Label: model.AgentIDLabel("edge-real"), AgentScope: model.ScheduleAgentScope{All: true}},
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
	st := store.NewTestMemory()
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
		ID:             "sched-allowed-scope",
		Revision:       1,
		Enabled:        true,
		Tool:           model.ToolPing,
		Target:         "1.1.1.1",
		CronExpression: "* * * * *",
		NextRunAt:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
		ScheduleTargets: []model.ScheduleTarget{
			{Label: "blue", AgentScope: model.ScheduleAgentScope{AllowedAgentIDs: []string{"edge-allowed"}}},
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

func TestScheduledTargetsIncludeAgentsAddedAfterScheduleCreation(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC().Add(-time.Minute)
	upsertHubTestAgent(t, ctx, st, model.Agent{ID: "edge-existing", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now})
	upsertHubTestAgentConfigLabels(t, ctx, st, "edge-existing", []string{"blue"})
	sched := model.ScheduledJob{
		ID:             "sched-dynamic-scope",
		Revision:       1,
		Enabled:        true,
		Tool:           model.ToolPing,
		Target:         "1.1.1.1",
		CronExpression: "* * * * *",
		NextRunAt:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
		ScheduleTargets: []model.ScheduleTarget{
			{Label: "blue", AgentScope: model.ScheduleAgentScope{All: true}},
		},
	}
	if err := st.CreateScheduledJob(ctx, sched); err != nil {
		t.Fatal(err)
	}
	upsertHubTestAgent(t, ctx, st, model.Agent{ID: "edge-new", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now})
	upsertHubTestAgentConfigLabels(t, ctx, st, "edge-new", []string{"blue"})
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
	if len(jobs) != 2 || !gotAgents["edge-existing"] || !gotAgents["edge-new"] {
		t.Fatalf("scheduled dynamic scope jobs = %#v", jobs)
	}
}

func TestScheduledTargetsExcludeAgentsThatNoLongerMatchAtRunTime(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC().Add(-time.Minute)
	agents := []model.Agent{
		{ID: "edge-keep", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-relabeled", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-incompatible", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-disabled", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	}
	for _, agent := range agents {
		upsertHubTestAgent(t, ctx, st, agent)
		upsertHubTestAgentConfigLabels(t, ctx, st, agent.ID, []string{"blue"})
	}
	sched := model.ScheduledJob{
		ID:             "sched-current-agent-state",
		Revision:       1,
		Enabled:        true,
		Tool:           model.ToolPing,
		Target:         "1.1.1.1",
		IPVersion:      model.IPv4,
		CronExpression: "* * * * *",
		NextRunAt:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
		ScheduleTargets: []model.ScheduleTarget{
			{Label: "blue", AgentScope: model.ScheduleAgentScope{All: true}},
		},
	}
	if err := st.CreateScheduledJob(ctx, sched); err != nil {
		t.Fatal(err)
	}

	upsertHubTestAgentConfigLabels(t, ctx, st, "edge-relabeled", []string{"green"})
	agents[2].Protocols = model.ProtocolIPv6
	upsertHubTestAgent(t, ctx, st, agents[2])
	disabledConfig, err := st.GetAgentConfig(ctx, "edge-disabled")
	if err != nil {
		t.Fatal(err)
	}
	disabledConfig.Disabled = true
	if err := st.UpsertAgentConfig(ctx, disabledConfig); err != nil {
		t.Fatal(err)
	}

	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())
	hub.runDueSchedules(ctx)

	jobs, err := st.ListActiveJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].AgentID != "edge-keep" {
		t.Fatalf("scheduled jobs should use current agent state: %#v", jobs)
	}
}

func TestHandleResultRejectsUnassignedAgent(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
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
	registerHubTestGRPCSession(t, hub, "edge-owner", job.ID)
	otherSession, _ := registerHubTestGRPCSession(t, hub, "edge-other")

	hub.handleResultForSession(ctx, "edge-other", otherSession.generation, grpcwire.ResultEvent{
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
	st := store.NewTestMemory()
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
	session, _ := registerHubTestGRPCSession(t, hub, "edge-owner")

	hub.handleResultForSession(ctx, "edge-owner", session.generation, grpcwire.ResultEvent{
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
	st := store.NewTestMemory()
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
	session, _ := registerHubTestGRPCSession(t, hub, "edge-owner", job.ID)

	hub.handleResultForSession(ctx, "edge-owner", session.generation, grpcwire.ResultEvent{
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

func TestNonterminalResultLosesAtomicRaceWithCancellation(t *testing.T) {
	ctx := context.Background()
	base := store.NewTestMemory()
	barrier := &blockingRunningEventStore{Store: base, entered: make(chan struct{}), release: make(chan struct{})}
	hub := NewHub(barrier, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()
	job := model.Job{ID: "job-cancel-event-race", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-owner", Status: model.JobRunning, CreatedAt: now, UpdatedAt: now, StartedAt: &now}
	if err := base.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	session, _ := registerHubTestGRPCSession(t, hub, job.AgentID, job.ID)

	done := make(chan struct{})
	go func() {
		hub.handleResultForSession(ctx, job.AgentID, session.generation, grpcwire.ResultEvent{JobID: job.ID, Event: map[string]any{"type": "stdout", "message": "late"}})
		close(done)
	}()
	<-barrier.entered
	if err := base.UpdateJobStatus(ctx, job.ID, model.JobCanceled, "canceled"); err != nil {
		t.Fatal(err)
	}
	close(barrier.release)
	<-done

	events, err := base.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("late event was stored after cancellation: %#v", events)
	}
}

type blockingRunningEventStore struct {
	store.Store
	entered chan struct{}
	release chan struct{}
}

func (s *blockingRunningEventStore) AddRunningJobEvent(ctx context.Context, sourceJobID string, agentID string, event model.JobEvent) (bool, error) {
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return s.Store.AddRunningJobEvent(ctx, sourceJobID, agentID, event)
}

func TestStaleResultGenerationAndAttemptCannotAppendEvent(t *testing.T) {
	t.Run("grpc generation", func(t *testing.T) {
		ctx := context.Background()
		st := store.NewTestMemory()
		hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.New(slog.NewTextHandler(io.Discard, nil)))
		now := time.Now().UTC()
		job := model.Job{ID: "job-stale-grpc-event", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-grpc", Status: model.JobRunning, CreatedAt: now, UpdatedAt: now, StartedAt: &now}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		oldSession, _ := hub.registerGRPCSession(job.AgentID, &grpcwire.AgentHello{ID: job.AgentID, Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, MaxConcurrentJobs: 1}, now, make(chan string, 1))
		if !hub.markInflightForGRPCSession(job.AgentID, job.ID, oldSession.generation) {
			t.Fatal("mark old gRPC generation")
		}
		newSession, _ := hub.registerGRPCSession(job.AgentID, &grpcwire.AgentHello{ID: job.AgentID, Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, MaxConcurrentJobs: 1}, now.Add(time.Second), make(chan string, 1))
		if !hub.markInflightForGRPCSession(job.AgentID, job.ID, newSession.generation) {
			t.Fatal("mark replacement gRPC generation")
		}
		hub.handleResultForSession(ctx, job.AgentID, oldSession.generation, grpcwire.ResultEvent{JobID: job.ID, Event: map[string]any{"type": "stdout", "message": "stale"}})
		hub.handleResultForSession(ctx, job.AgentID, newSession.generation, grpcwire.ResultEvent{JobID: job.ID, Event: map[string]any{"type": "stdout", "message": "current"}})
		assertSingleCurrentStreamEvent(t, ctx, st, job.ID, "current")
	})

	t.Run("outbound attempt", func(t *testing.T) {
		ctx := context.Background()
		st := store.NewTestMemory()
		hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.New(slog.NewTextHandler(io.Discard, nil)))
		now := time.Now().UTC()
		job := model.Job{ID: "job-stale-outbound-event", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-http", Status: model.JobRunning, CreatedAt: now, UpdatedAt: now, StartedAt: &now}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		hub.mu.Lock()
		_, generation := hub.registerOutboundAgentLoopLocked(ctx, job.AgentID)
		hub.mu.Unlock()
		oldAttempt, ok := hub.markInflightForOutboundAgent(job.AgentID, job.ID, generation)
		if !ok {
			t.Fatal("mark old outbound attempt")
		}
		newAttempt, ok := hub.markInflightForOutboundAgent(job.AgentID, job.ID, generation)
		if !ok || newAttempt == oldAttempt {
			t.Fatalf("mark replacement outbound attempt=%d ok=%t", newAttempt, ok)
		}
		hub.mu.Lock()
		_, grpcBudgetExists := hub.grpcResultBudgets[job.ID]
		hub.mu.Unlock()
		if grpcBudgetExists {
			t.Fatal("outbound attempt inherited a gRPC result budget")
		}
		hub.handleOutboundAgentResult(ctx, job.AgentID, generation, oldAttempt, grpcwire.ResultEvent{JobID: job.ID, Event: map[string]any{"type": "stdout", "message": "stale"}})
		hub.handleOutboundAgentResult(ctx, job.AgentID, generation, newAttempt, grpcwire.ResultEvent{JobID: job.ID, Event: map[string]any{"type": "stdout", "message": "current"}})
		assertSingleCurrentStreamEvent(t, ctx, st, job.ID, "current")
	})
}

func assertSingleCurrentStreamEvent(t *testing.T, ctx context.Context, st store.Store, jobID string, message string) {
	t.Helper()
	events, err := st.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event == nil || events[0].Event.Message != message {
		t.Fatalf("stored events = %#v, want only %q", events, message)
	}
}

func TestTerminalStoreFailureRestoresExactInflightOwnership(t *testing.T) {
	ctx := context.Background()
	base := store.NewTestMemory()
	failing := &failTerminalEventOnceStore{Store: base, err: errors.New("terminal transaction failed"), fail: true}
	hub := NewHub(failing, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()
	job := model.Job{ID: "job-terminal-store-failure", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-owner", Status: model.JobRunning, CreatedAt: now, UpdatedAt: now, StartedAt: &now}
	if err := base.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	session, _ := registerHubTestGRPCSession(t, hub, job.AgentID, job.ID)
	summary := grpcwire.ResultEvent{JobID: job.ID, Event: map[string]any{"type": "summary", "exit_code": 0}}

	hub.handleResultForSession(ctx, job.AgentID, session.generation, summary)
	got, err := base.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobRunning || !hubTestGRPCSessionOwnsJob(hub, job.AgentID, job.ID, session.generation) {
		t.Fatalf("failed transaction lost running ownership: job=%#v owns=%t", got, hubTestGRPCSessionOwnsJob(hub, job.AgentID, job.ID, session.generation))
	}
	hub.mu.Lock()
	restoredBudget, budgetExists := hub.grpcResultBudgets[job.ID]
	hub.mu.Unlock()
	if !budgetExists || restoredBudget.generation != session.generation || restoredBudget.totalBytes == 0 {
		t.Fatalf("failed transaction did not restore consumed result budget: %#v exists=%t", restoredBudget, budgetExists)
	}
	events, err := base.ListJobEvents(ctx, job.ID)
	if err != nil || len(events) != 0 {
		t.Fatalf("failed transaction stored events=%#v err=%v", events, err)
	}

	hub.handleResultForSession(ctx, job.AgentID, session.generation, summary)
	got, err = base.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobSucceeded {
		t.Fatalf("retry status=%s", got.Status)
	}
	hub.mu.Lock()
	_, budgetExists = hub.grpcResultBudgets[job.ID]
	hub.mu.Unlock()
	if budgetExists {
		t.Fatal("completed retry retained result budget")
	}
	events, err = base.ListJobEvents(ctx, job.ID)
	if err != nil || len(events) != 2 {
		t.Fatalf("retry events=%#v err=%v", events, err)
	}
}

type failTerminalEventOnceStore struct {
	store.Store
	mu   sync.Mutex
	err  error
	fail bool
}

type failActiveJobOnceStore struct {
	store.Store
	mu   sync.Mutex
	err  error
	fail bool
}

func (s *failActiveJobOnceStore) FailActiveJob(ctx context.Context, jobID string, message string) (bool, error) {
	s.mu.Lock()
	if s.fail {
		s.fail = false
		err := s.err
		s.mu.Unlock()
		return false, err
	}
	s.mu.Unlock()
	return s.Store.FailActiveJob(ctx, jobID, message)
}

func (s *failTerminalEventOnceStore) FinishRunningJobWithEvent(ctx context.Context, sourceJobID string, agentID string, status model.JobStatus, msg string, event model.JobEvent) (bool, error) {
	s.mu.Lock()
	if s.fail {
		s.fail = false
		err := s.err
		s.mu.Unlock()
		return false, err
	}
	s.mu.Unlock()
	return s.Store.FinishRunningJobWithEvent(ctx, sourceJobID, agentID, status, msg, event)
}

func TestHandleResultUsesParsedFailureType(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())
	now := time.Now().UTC()
	job := model.Job{
		ID:        "job-unsupported-protocol",
		Tool:      model.ToolPing,
		Target:    "2606:4700:4700::1111",
		AgentID:   "edge-v4",
		IPVersion: model.IPv6,
		Status:    model.JobRunning,
		CreatedAt: now,
		UpdatedAt: now,
		StartedAt: &now,
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	session, _ := registerHubTestGRPCSession(t, hub, "edge-v4", job.ID)

	hub.handleResultForSession(ctx, "edge-v4", session.generation, grpcwire.ResultEvent{
		JobID: job.ID,
		Event: map[string]any{
			"type":      "summary",
			"exit_code": -1,
			"metric":    map[string]any{"status": model.JobErrorUnsupportedProtocol},
		},
	})

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobFailed || got.Error != model.JobErrorUnsupportedProtocol {
		t.Fatalf("unsupported protocol failure should be preserved: %#v", got)
	}
}

func TestFailTimedOutJobsDefersFanoutParentToChildren(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
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
	st := store.NewTestMemory()
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
	_, grpcCancelCh := registerHubTestGRPCSession(t, hub, child.AgentID, child.ID)
	setHubTestInflightCancel(hub, child.ID, cancelChild)

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
	st := store.NewTestMemory()
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
	st := store.NewTestMemory()
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
	hub := NewHub(store.NewTestMemory(), policy.DefaultPolicies(), time.Minute, time.Millisecond, 9, slog.Default())

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

func TestAgentSettingsUsePolicyLabelsAndIgnoreGroupExecutionSettings(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	upsertHubTestAgent(t, ctx, st, model.Agent{ID: "edge-1", Status: model.AgentOffline})
	upsertHubTestAgentConfigLabels(t, ctx, st, "edge-1", []string{"blue", "agent:custom"})
	hub := NewHub(st, policy.DefaultPolicies(), time.Minute, time.Millisecond, 4, slog.Default())
	groupRuntime := config.DefaultRuntime()
	groupRuntime.Count = 1
	groupScheduler := config.Scheduler{MaxInflightPerAgent: 1}
	typeRuntime := config.DefaultRuntime()
	typeRuntime.Count = 4
	idRuntime := config.DefaultRuntime()
	idRuntime.Count = 2
	hub.ApplySettings(config.ManagedSettings{LabelConfigs: map[string]config.LabelConfig{
		"blue":         {Runtime: &groupRuntime, Scheduler: &groupScheduler, ToolPolicies: map[string]config.Policy{"ping": {Enabled: false}}},
		"agent:custom": {Runtime: &typeRuntime},
		"id:edge-1": {Runtime: &idRuntime, Scheduler: &config.Scheduler{MaxInflightPerAgent: 2}, ToolPolicies: map[string]config.Policy{
			"ping": {Enabled: true, AllowedArgs: map[string]string{"protocol": "tcp", "port": "443"}},
		}},
	}})
	scheduler, runtime, policies, err := hub.settingsForAgent(ctx, "edge-1")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Count != 2 || scheduler.MaxInflightPerAgent != 2 || !policies["ping"].Enabled || policies["ping"].AllowedArgs["protocol"] != "tcp" {
		t.Fatalf("policy label settings were not applied: runtime=%#v scheduler=%#v policies=%#v", runtime, scheduler, policies)
	}
	// Dropping the per-node label configuration leaves the custom agent policy
	// active; the ordinary group's stricter settings must still have no effect.
	hub.ApplySettings(config.ManagedSettings{LabelConfigs: map[string]config.LabelConfig{
		"blue":         {Runtime: &groupRuntime, Scheduler: &groupScheduler, ToolPolicies: map[string]config.Policy{"ping": {Enabled: false}}},
		"agent:custom": {Runtime: &typeRuntime},
	}})
	scheduler, runtime, policies, err = hub.settingsForAgent(ctx, "edge-1")
	if err != nil || runtime.Count != 4 || scheduler.MaxInflightPerAgent != 4 || len(policies) != 0 {
		t.Fatalf("group settings affected execution: runtime=%#v scheduler=%#v policies=%#v err=%v", runtime, scheduler, policies, err)
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
