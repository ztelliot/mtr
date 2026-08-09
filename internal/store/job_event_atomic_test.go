package store

import (
	"context"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/model"
)

func TestAtomicRunningJobEvents(t *testing.T) {
	for _, backend := range []struct {
		name string
		new  func(*testing.T) Store
	}{
		{name: "memory", new: func(*testing.T) Store { return NewTestMemory() }},
		{name: "sqlite", new: func(t *testing.T) Store {
			st, err := newTestSQLite(t.Context(), "sqlite://"+t.TempDir()+"/atomic-events.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(st.Close)
			return st
		}},
	} {
		t.Run(backend.name, func(t *testing.T) {
			ctx := context.Background()
			st := backend.new(t)
			now := time.Now().UTC()
			agent := model.Agent{ID: "atomic-agent", Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now}
			if err := st.UpsertAgent(ctx, agent); err != nil {
				t.Fatal(err)
			}

			running := model.Job{ID: "running-event", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: agent.ID, Status: model.JobRunning, CreatedAt: now, UpdatedAt: now, StartedAt: &now}
			if err := st.CreateJob(ctx, running); err != nil {
				t.Fatal(err)
			}
			event := model.JobEvent{ID: "running-event-1", JobID: running.ID, AgentID: agent.ID, Stream: "stdout", CreatedAt: now}
			stored, err := st.AddRunningJobEvent(ctx, running.ID, agent.ID, event)
			if err != nil || !stored {
				t.Fatalf("add running event: stored=%t err=%v", stored, err)
			}
			wrongAgentEvent := event
			wrongAgentEvent.ID = "running-event-wrong-agent"
			stored, err = st.AddRunningJobEvent(ctx, running.ID, "other-agent", wrongAgentEvent)
			if err != nil || stored {
				t.Fatalf("wrong agent event: stored=%t err=%v", stored, err)
			}
			if err := st.UpdateJobStatus(ctx, running.ID, model.JobCanceled, "canceled"); err != nil {
				t.Fatal(err)
			}
			lateEvent := event
			lateEvent.ID = "running-event-late"
			stored, err = st.AddRunningJobEvent(ctx, running.ID, agent.ID, lateEvent)
			if err != nil || stored {
				t.Fatalf("late event: stored=%t err=%v", stored, err)
			}
			events, err := st.ListJobEvents(ctx, running.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].ID != event.ID {
				t.Fatalf("stored running events = %#v", events)
			}

			terminal := model.Job{ID: "terminal-event", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: agent.ID, Status: model.JobRunning, CreatedAt: now, UpdatedAt: now, StartedAt: &now}
			if err := st.CreateJob(ctx, terminal); err != nil {
				t.Fatal(err)
			}
			exitCode := 0
			summary := model.JobEvent{ID: "terminal-summary", JobID: terminal.ID, AgentID: agent.ID, Stream: "summary", ExitCode: &exitCode, CreatedAt: now}
			finished, err := st.FinishRunningJobWithEvent(ctx, terminal.ID, agent.ID, model.JobSucceeded, "", summary)
			if err != nil || !finished {
				t.Fatalf("finish with event: finished=%t err=%v", finished, err)
			}
			got, err := st.GetJob(ctx, terminal.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != model.JobSucceeded {
				t.Fatalf("terminal status = %s", got.Status)
			}
			events, err = st.ListJobEvents(ctx, terminal.ID)
			if err != nil || len(events) != 1 || events[0].ID != summary.ID {
				t.Fatalf("terminal events = %#v, err=%v", events, err)
			}
		})
	}
}

func TestFinishRunningJobWithEventRollsBackOnEventFailure(t *testing.T) {
	for _, backend := range []struct {
		name string
		new  func(*testing.T) Store
	}{
		{name: "memory", new: func(*testing.T) Store { return NewTestMemory() }},
		{name: "sqlite", new: func(t *testing.T) Store {
			st, err := newTestSQLite(t.Context(), "sqlite://"+t.TempDir()+"/atomic-rollback.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(st.Close)
			return st
		}},
	} {
		t.Run(backend.name, func(t *testing.T) {
			ctx := context.Background()
			st := backend.new(t)
			now := time.Now().UTC()
			agent := model.Agent{ID: "rollback-agent", Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now}
			if err := st.UpsertAgent(ctx, agent); err != nil {
				t.Fatal(err)
			}
			job := model.Job{ID: "rollback-job", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: agent.ID, Status: model.JobRunning, CreatedAt: now, UpdatedAt: now, StartedAt: &now}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			exitCode := 0
			badEvent := model.JobEvent{ID: "orphan-summary", JobID: "missing-event-job", AgentID: agent.ID, Stream: "summary", ExitCode: &exitCode, CreatedAt: now}
			if finished, err := st.FinishRunningJobWithEvent(ctx, job.ID, agent.ID, model.JobSucceeded, "", badEvent); err == nil || finished {
				t.Fatalf("finish with orphan event: finished=%t err=%v", finished, err)
			}
			got, err := st.GetJob(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != model.JobRunning || got.CompletedAt != nil {
				t.Fatalf("failed terminal transaction changed job: %#v", got)
			}
		})
	}
}
