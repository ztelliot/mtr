package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/abuse"
	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/scheduler"
	"github.com/ztelliot/mtr/internal/store"
)

func TestAutoPingDispatchIncludesRestrictedNodes(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	settings, err := st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.LabelConfigs = map[string]config.LabelConfig{}
	for label, protocol := range map[string]string{"tcp": "tcp", "udp": "udp", "icmp": "icmp", "noicmp": "udp,tcp", "blocked": "tcp"} {
		port := policy.DefaultPortRange
		if label == "blocked" {
			port = "443"
		}
		settings.LabelConfigs["agent:"+label] = config.LabelConfig{ToolPolicies: map[string]config.Policy{
			"ping": {Enabled: true, AllowedArgs: map[string]string{"protocol": protocol, "port": port}},
		}}
		upsertTestAgent(t, ctx, st, "edge-"+label, []string{label, "agent:" + label})
	}
	if settings, err = st.UpdateManagedSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	policies := policy.DefaultPolicies()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := scheduler.NewHub(st, policies, time.Minute, time.Millisecond, 4, log)
	hub.ApplySettings(settings)
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", log, TokenConfig{Token: "admin", Scope: allTokenScope})

	for _, tt := range []struct {
		name, path, body string
		status           int
	}{
		{"fanout", "/v1/jobs", `{"tool":"ping","target":"1.1.1.1","args":{"protocol":"auto"}}`, http.StatusAccepted},
		{"pinned", "/v1/jobs", `{"tool":"ping","target":"1.1.1.1","agent_id":"edge-tcp","args":{"protocol":"auto"}}`, http.StatusAccepted},
		{"schedule", "/v1/schedules", `{"tool":"ping","target":"1.1.1.1","args":{"protocol":"auto"},"cron_expression":"* * * * *","schedule_targets":[{"label":"tcp"}]}`, http.StatusCreated},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer admin")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if tt.name == "fanout" {
				var parent model.Job
				if err := json.Unmarshal(rec.Body.Bytes(), &parent); err != nil {
					t.Fatal(err)
				}
				children, err := st.ListChildJobs(ctx, parent.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(children) != 4 {
					t.Fatalf("expected all four nodes with concrete protocols: %#v", children)
				}
				for _, child := range children {
					if child.AgentID == "edge-blocked" {
						t.Fatal("node without an allowed default probe port was included")
					}
				}
			}
		})
	}
}

func TestAutoRetainsTokenCandidateRestriction(t *testing.T) {
	server := &Server{policies: policy.DefaultPolicies()}
	for _, tt := range []struct {
		tool                                         model.Tool
		target, key, candidateKey, permitted, denied string
	}{
		{model.ToolPing, "1.1.1.1", "protocol", policy.AutoProtocolsArg, "udp", "tcp"},
		{model.ToolHTTP, "https://example.com", "http_version", policy.AutoHTTPVersionsArg, "2", "1.1"},
	} {
		t.Run(string(tt.tool), func(t *testing.T) {
			allowed := map[string]string{tt.key: tt.permitted, "port": policy.DefaultPortRange, "method": "GET"}
			ctx := withPrincipal(context.Background(), principal{scope: TokenScope{Tools: map[model.Tool]ToolScope{tt.tool: {AllowedArgs: allowed}}}})
			req := model.CreateJobRequest{Tool: tt.tool, Target: tt.target, Args: map[string]string{tt.key: "auto"}}
			req.Args = server.jobArgsForScope(ctx, req, server.policies)
			if err := server.authorizeJob(ctx, req); err != nil {
				t.Fatal(err)
			}
			if got := req.Args[tt.candidateKey]; got != tt.permitted {
				t.Fatalf("token candidates = %q, want %q", got, tt.permitted)
			}
			allowed[tt.key] = tt.denied
			if err := policy.ValidateAllowedArgs(req.Tool, req.Args, allowed); err == nil {
				t.Fatal("disjoint node policy must not widen the token restriction")
			}
		})
	}
}
