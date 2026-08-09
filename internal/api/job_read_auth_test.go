package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/abuse"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
)

func TestAuthorizeJobReadEnforcesToolAndCurrentAgentScope(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	upsertTestAgent(t, ctx, st, "edge-blue", []string{"blue"})
	upsertTestAgent(t, ctx, st, "edge-red", []string{"red"})
	server := &Server{store: st}
	blueJob := model.Job{ID: "job-blue", Tool: model.ToolPing, AgentID: "edge-blue"}
	redJob := model.Job{ID: "job-red", Tool: model.ToolPing, AgentID: "edge-red"}

	tests := []struct {
		name    string
		scope   TokenScope
		job     model.Job
		allowed bool
	}{
		{
			name: "matching tag",
			scope: TokenScope{
				AgentTags: []string{"blue"},
				Tools:     map[model.Tool]ToolScope{model.ToolPing: {}},
			},
			job:     blueJob,
			allowed: true,
		},
		{
			name: "different tag",
			scope: TokenScope{
				AgentTags: []string{"blue"},
				Tools:     map[model.Tool]ToolScope{model.ToolPing: {}},
			},
			job: redJob,
		},
		{
			name: "tool denied",
			scope: TokenScope{
				Agents: []string{"edge-blue"},
				Tools:  map[model.Tool]ToolScope{model.ToolHTTP: {}},
			},
			job: blueJob,
		},
		{
			name: "agent id denied",
			scope: TokenScope{
				Agents:       []string{"*"},
				DeniedAgents: []string{"edge-blue"},
				Tools:        map[model.Tool]ToolScope{model.ToolPing: {}},
			},
			job: blueJob,
		},
		{
			name: "agent tag denied",
			scope: TokenScope{
				Agents:     []string{"*"},
				DeniedTags: []string{"blue"},
				Tools:      map[model.Tool]ToolScope{model.ToolPing: {}},
			},
			job: blueJob,
		},
		{
			name:    "all scope can read historical missing agent",
			scope:   TokenScope{All: true},
			job:     model.Job{ID: "historical", Tool: model.ToolPing, AgentID: "deleted-agent"},
			allowed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := server.authorizeJobRead(withPrincipal(ctx, principal{scope: tt.scope}), tt.job)
			if tt.allowed && err != nil {
				t.Fatalf("authorized read rejected: %v", err)
			}
			if !tt.allowed && !errors.Is(err, errJobReadForbidden) {
				t.Fatalf("read error = %v, want %v", err, errJobReadForbidden)
			}
		})
	}
}

func TestAuthorizeJobReadRequiresEveryFanoutChildToBeVisible(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	upsertTestAgent(t, ctx, st, "edge-blue", []string{"blue"})
	upsertTestAgent(t, ctx, st, "edge-red", []string{"red"})
	now := time.Now().UTC()
	parent := model.Job{ID: "fanout", Tool: model.ToolPing, Status: model.JobRunning, CreatedAt: now, UpdatedAt: now}
	children := []model.Job{
		{ID: "fanout-blue", ParentID: parent.ID, Tool: parent.Tool, AgentID: "edge-blue", Status: model.JobQueued, CreatedAt: now, UpdatedAt: now},
		{ID: "fanout-red", ParentID: parent.ID, Tool: parent.Tool, AgentID: "edge-red", Status: model.JobQueued, CreatedAt: now, UpdatedAt: now},
	}
	if err := st.CreateJobs(ctx, append([]model.Job{parent}, children...)); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: st}
	blueScope := TokenScope{
		AgentTags: []string{"blue"},
		Tools:     map[model.Tool]ToolScope{model.ToolPing: {}},
	}
	if err := server.authorizeJobRead(withPrincipal(ctx, principal{scope: blueScope}), parent); !errors.Is(err, errJobReadForbidden) {
		t.Fatalf("mixed-visibility fanout error = %v, want %v", err, errJobReadForbidden)
	}
	allAgentsScope := TokenScope{
		Agents: []string{"*"},
		Tools:  map[model.Tool]ToolScope{model.ToolPing: {}},
	}
	if err := server.authorizeJobRead(withPrincipal(ctx, principal{scope: allAgentsScope}), parent); err != nil {
		t.Fatalf("fully visible fanout rejected: %v", err)
	}
	if err := server.authorizeJobRead(withPrincipal(ctx, principal{scope: allAgentsScope}), model.Job{ID: "empty", Tool: model.ToolPing}); !errors.Is(err, errJobReadForbidden) {
		t.Fatalf("childless parent error = %v, want %v", err, errJobReadForbidden)
	}
}

func TestJobReadEndpointsRejectOutOfScopeToken(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	upsertTestAgent(t, ctx, st, "edge-blue", []string{"blue"})
	upsertTestAgent(t, ctx, st, "edge-red", []string{"red"})
	now := time.Now().UTC()
	job := model.Job{
		ID:        "job-red",
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		AgentID:   "edge-red",
		Status:    model.JobSucceeded,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.AddJobEvent(ctx, model.JobEvent{
		ID: "event-red", JobID: job.ID, AgentID: job.AgentID,
		Event: &model.StreamEvent{Type: "summary"}, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	handler := New(
		st,
		policy.DefaultPolicies(),
		abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
			Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
			IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		}),
		nil,
		"",
		slog.Default(),
		TokenConfig{Token: "blue-token", Scope: TokenScope{
			AgentTags: []string{"blue"},
			Tools:     map[model.Tool]ToolScope{model.ToolPing: {}},
		}},
	)

	for _, path := range []string{
		"/v1/jobs/job-red",
		"/v1/jobs/job-red/events",
		"/v1/jobs/job-red/stream",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer blue-token")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}
