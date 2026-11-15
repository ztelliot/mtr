package scheduler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
)

func TestScheduledAutoPingIncludesRestrictedNodesAndConstrainsDispatch(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	policies := policy.DefaultPolicies()
	hub := NewHub(st, policies, time.Minute, time.Millisecond, 4, slog.Default())
	now := time.Now().UTC().Add(-time.Minute)
	settings := config.ManagedSettings{LabelConfigs: map[string]config.LabelConfig{}}
	for label, protocols := range map[string]string{"tcp": "tcp", "noicmp": "udp,tcp"} {
		upsertHubTestAgent(t, ctx, st, model.Agent{
			ID: "edge-" + label, Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll,
			Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now,
		})
		upsertHubTestAgentConfigLabels(t, ctx, st, "edge-"+label, []string{"agent:" + label, "probe"})
		settings.LabelConfigs["agent:"+label] = config.LabelConfig{ToolPolicies: map[string]config.Policy{
			"ping": {Enabled: true, AllowedArgs: map[string]string{"protocol": protocols, "port": policy.DefaultPortRange}},
		}}
	}
	hub.ApplySettings(settings)
	if err := st.CreateScheduledJob(ctx, model.ScheduledJob{
		ID: "auto-ping", Revision: 1, Enabled: true, Tool: model.ToolPing, Target: "1.1.1.1",
		Args: map[string]string{"protocol": "auto"}, ResolveOnAgent: true,
		CronExpression: "* * * * *", NextRunAt: now, CreatedAt: now, UpdatedAt: now,
		ScheduleTargets: []model.ScheduleTarget{{Label: "probe", AgentScope: model.ScheduleAgentScope{All: true}}},
	}); err != nil {
		t.Fatal(err)
	}
	hub.runDueSchedules(ctx)
	jobs, err := st.ListActiveJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected both restricted nodes to receive scheduled jobs: %#v", jobs)
	}
	for _, job := range jobs {
		want := "tcp"
		if job.AgentID == "edge-noicmp" {
			want = "udp,tcp"
		}
		if got := job.Args[policy.AutoProtocolsArg]; got != want {
			t.Fatalf("%s candidates = %q, want %q", job.AgentID, got, want)
		}
		// A policy change between queueing and dispatch must narrow the wire job.
		current := policies.WithIntersection(map[string]config.Policy{
			"ping": {Enabled: true, AllowedArgs: map[string]string{"protocol": "tcp", "port": policy.DefaultPortRange}},
		}, config.DefaultRuntime())
		p, failure, err := compatibleQueuedJobPolicy(current, []model.Tool{model.ToolPing}, model.ProtocolAll, job)
		if err != nil || failure != "" {
			t.Fatalf("queued auto job rejected: failure=%q err=%v", failure, err)
		}
		if got := toJobSpecWithPolicies(job, p, current).Args[policy.AutoProtocolsArg]; got != "tcp" {
			t.Fatalf("wire candidates = %q, want tcp", got)
		}
	}
}
