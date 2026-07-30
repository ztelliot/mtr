package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
)

func TestWatchMetricsExposeNumericAndTextResults(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	if err := st.UpsertAgent(ctx, model.Agent{
		ID:         "edge-1",
		Country:    "CN",
		Provider:   "example",
		Status:     model.AgentOnline,
		Protocols:  model.ProtocolAll,
		LastSeenAt: now,
		CreatedAt:  now,
	}); err != nil {
		t.Fatal(err)
	}

	loss := 20.0
	sent := 10
	avg := 12.5
	for _, fixture := range []struct {
		schedule model.ScheduledJob
		job      model.Job
		result   model.ToolResult
	}{
		{
			schedule: watchMetricsSchedule("ping-watch", model.ToolPing, "1.1.1.1", now),
			job:      watchMetricsJob("ping-job", "ping-watch", model.ToolPing, now),
			result: model.ToolResult{Tool: model.ToolPing, ExitCode: 0, Summary: map[string]any{
				"packets_transmitted": 4, "packets_received": 3, "packet_loss_pct": 25,
				"rtt_min_ms": 5.0, "rtt_avg_ms": 10.0, "rtt_max_ms": 15.0,
			}},
		},
		{
			schedule: watchMetricsSchedule("http-watch", model.ToolHTTP, "https://example.com", now),
			job:      watchMetricsJob("http-job", "http-watch", model.ToolHTTP, now),
			result: model.ToolResult{Tool: model.ToolHTTP, ExitCode: 0, Summary: map[string]any{
				"time_total_ms": 125.0, "http_code": 200, "bytes_downloaded": 1024,
			}},
		},
		{
			schedule: watchMetricsSchedule("dns-watch", model.ToolDNS, "example.com", now),
			job:      watchMetricsJob("dns-job", "dns-watch", model.ToolDNS, now),
			result: model.ToolResult{Tool: model.ToolDNS, ExitCode: 0, Summary: map[string]any{"record_count": 1}, Records: []model.DNSRecord{
				{Type: "A", Value: "192.0.2.10"},
			}},
		},
		{
			schedule: watchMetricsSchedule("port-watch", model.ToolPort, "example.com", now),
			job:      watchMetricsJob("port-job", "port-watch", model.ToolPort, now),
			result: model.ToolResult{Tool: model.ToolPort, ExitCode: 0, Summary: map[string]any{
				"status": "open", "port": 443, "protocol": "tcp", "peer": "192.0.2.10", "connect_ms": 8.5,
			}},
		},
		{
			schedule: watchMetricsSchedule("route-watch", model.ToolMTR, "example.com", now),
			job:      watchMetricsJob("route-job", "route-watch", model.ToolMTR, now),
			result: model.ToolResult{Tool: model.ToolMTR, ExitCode: 0, Summary: map[string]any{"hop_count": 1}, Hops: []model.HopResult{
				{Index: 1, Host: "router.example", IP: "192.0.2.1", LossPct: &loss, Sent: &sent, AvgMS: &avg},
			}},
		},
	} {
		if err := st.CreateScheduledJob(ctx, fixture.schedule); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, fixture.job); err != nil {
			t.Fatal(err)
		}
		result := fixture.result
		result.Type = "summary"
		result.Target = fixture.schedule.Target
		result.IPVersion = model.IPv4
		exitCode := result.ExitCode
		if err := st.AddJobEvent(ctx, model.JobEvent{
			ID: fixture.job.ID + "-summary", JobID: fixture.job.ID, AgentID: fixture.job.AgentID,
			Stream: "summary", ExitCode: &exitCode, Parsed: &result, CreatedAt: now.Add(time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}

	handler := New(st, policy.DefaultPolicies(), nil, nil, "", slog.Default())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	assertPrometheusMetric(t, body, "mtr_watch_schedule_cron_info", map[string]string{"schedule_id": "ping-watch", "cron_expression": "*/5 * * * *"}, "1")
	assertPrometheusMetric(t, body, "mtr_watch_schedule_target_info", map[string]string{"schedule_id": "ping-watch", "node_label": "asia"}, "1")
	assertPrometheusMetric(t, body, "mtr_watch_ping_packet_loss_ratio", map[string]string{"schedule_id": "ping-watch"}, "0.25")
	assertPrometheusMetric(t, body, "mtr_watch_ping_rtt_seconds", map[string]string{"schedule_id": "ping-watch", "stat": "avg"}, "0.01")
	assertPrometheusMetric(t, body, "mtr_watch_http_duration_seconds", map[string]string{"schedule_id": "http-watch", "phase": "total"}, "0.125")
	assertPrometheusMetric(t, body, "mtr_watch_dns_record_info", map[string]string{"schedule_id": "dns-watch", "record_type": "A", "record": "192.0.2.10"}, "1")
	assertPrometheusMetric(t, body, "mtr_watch_port_open", map[string]string{"schedule_id": "port-watch"}, "1")
	assertPrometheusMetric(t, body, "mtr_watch_route_hop_loss_ratio", map[string]string{"schedule_id": "route-watch", "hop": "1"}, "0.2")
	assertPrometheusMetric(t, body, "mtr_watch_route_hop_rtt_seconds", map[string]string{"schedule_id": "route-watch", "hop": "1", "stat": "avg"}, "0.0125")
	assertPrometheusMetric(t, body, "mtr_watch_route_path_info", map[string]string{"schedule_id": "route-watch"}, "1")
	if !strings.Contains(body, `path_hash="`) || !strings.Contains(body, `router.example`) {
		t.Fatalf("route path text/hash missing:\n%s", body)
	}
	if strings.Contains(body, "ping-job") || strings.Contains(body, "dns-job") {
		t.Fatalf("per-run job IDs must not become Prometheus labels:\n%s", body)
	}
}

func TestWatchMetricsReconstructRouteHopsFromStreamEvents(t *testing.T) {
	for _, tool := range []model.Tool{model.ToolTraceroute, model.ToolMTR} {
		t.Run(string(tool), func(t *testing.T) {
			st := store.NewMemory()
			ctx := context.Background()
			now := time.Unix(1_700_000_000, 0).UTC()
			schedule := watchMetricsSchedule(string(tool)+"-watch", tool, "1.0.0.1", now)
			job := watchMetricsJob(string(tool)+"-job", schedule.ID, tool, now)
			if err := st.CreateScheduledJob(ctx, schedule); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}

			rawRTT := 99.0
			if err := st.AddJobEvent(ctx, model.JobEvent{
				ID: job.ID + "-hop", JobID: job.ID, AgentID: job.AgentID, Stream: "hop",
				Event:     &model.StreamEvent{Type: "hop", Hop: &model.HopResult{Index: 1, IP: "192.0.2.1", RTTMS: &rawRTT}},
				CreatedAt: now.Add(time.Second),
			}); err != nil {
				t.Fatal(err)
			}

			if tool == model.ToolMTR {
				finalRTT := 25.0
				avg := 20.0
				loss := 20.0
				sent := 10
				if err := st.AddJobEvent(ctx, model.JobEvent{
					ID: job.ID + "-hop-summary", JobID: job.ID, AgentID: job.AgentID, Stream: "hop_summary",
					Event: &model.StreamEvent{Type: "hop_summary", Hop: &model.HopResult{
						Index: 1, Host: "router.example", IP: "192.0.2.1", RTTMS: &finalRTT,
						LossPct: &loss, Sent: &sent, AvgMS: &avg,
					}},
					CreatedAt: now.Add(2 * time.Second),
				}); err != nil {
					t.Fatal(err)
				}
			}

			exitCode := 0
			if err := st.AddJobEvent(ctx, model.JobEvent{
				ID: job.ID + "-summary", JobID: job.ID, AgentID: job.AgentID, Stream: "summary", ExitCode: &exitCode,
				Parsed: &model.ToolResult{Type: "summary", Tool: tool, Target: schedule.Target, IPVersion: model.IPv4,
					ExitCode: 0, Summary: map[string]any{"hop_count": 1, "protocol": "icmp"}},
				CreatedAt: now.Add(3 * time.Second),
			}); err != nil {
				t.Fatal(err)
			}

			handler := New(st, policy.DefaultPolicies(), nil, nil, "", slog.Default())
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			assertPrometheusMetric(t, body, "mtr_watch_route_hop_info", map[string]string{
				"schedule_id": schedule.ID, "hop": "1", "ip": "192.0.2.1",
			}, "1")
			if tool == model.ToolMTR {
				assertPrometheusMetric(t, body, "mtr_watch_route_hop_rtt_seconds", map[string]string{
					"schedule_id": schedule.ID, "hop": "1", "stat": "avg",
				}, "0.02")
				assertPrometheusMetric(t, body, "mtr_watch_route_hop_loss_ratio", map[string]string{
					"schedule_id": schedule.ID, "hop": "1",
				}, "0.2")
			} else {
				assertPrometheusMetric(t, body, "mtr_watch_route_hop_rtt_seconds", map[string]string{
					"schedule_id": schedule.ID, "hop": "1", "stat": "rtt",
				}, "0.099")
			}
			assertPrometheusMetric(t, body, "mtr_watch_route_path_info", map[string]string{"schedule_id": schedule.ID}, "1")
		})
	}
}

func TestWatchMetricsKeepLatestCompletedResultWhileNextRunIsActive(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	schedule := watchMetricsSchedule("ping-watch", model.ToolPing, "1.1.1.1", now)
	if err := st.CreateScheduledJob(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	completed := watchMetricsJob("completed", schedule.ID, schedule.Tool, now)
	if err := st.CreateJob(ctx, completed); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if err := st.AddJobEvent(ctx, model.JobEvent{
		ID: "completed-summary", JobID: completed.ID, AgentID: completed.AgentID, Stream: "summary", ExitCode: &exitCode,
		Parsed: &model.ToolResult{Tool: model.ToolPing, ExitCode: 0, Summary: map[string]any{"packet_loss_pct": 10}}, CreatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	active := watchMetricsJob("active", schedule.ID, schedule.Tool, now.Add(time.Minute))
	active.Status = model.JobRunning
	active.CompletedAt = nil
	if err := st.CreateJob(ctx, active); err != nil {
		t.Fatal(err)
	}

	handler := New(st, policy.DefaultPolicies(), nil, nil, "", slog.Default())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assertPrometheusMetric(t, rec.Body.String(), "mtr_watch_ping_packet_loss_ratio", map[string]string{"schedule_id": schedule.ID}, "0.1")
}

func TestWatchMetricsRequireScheduleReadAccess(t *testing.T) {
	st := store.NewMemory()
	handler := New(st, policy.DefaultPolicies(), nil, nil, "", slog.Default(),
		TokenConfig{Token: "reader", Scope: TokenScope{ScheduleAccess: ScheduleAccessRead}},
		TokenConfig{Token: "manage-only", Scope: TokenScope{ManageAccess: ScheduleAccessRead}},
	)

	for _, test := range []struct {
		name   string
		token  string
		status int
	}{
		{name: "missing token", status: http.StatusUnauthorized},
		{name: "wrong scope", token: "manage-only", status: http.StatusForbidden},
		{name: "schedule reader", token: "reader", status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if test.token != "" {
				req.Header.Set("Authorization", "Bearer "+test.token)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.status {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, test.status, rec.Body.String())
			}
		})
	}
}

func watchMetricsSchedule(id string, tool model.Tool, target string, now time.Time) model.ScheduledJob {
	return model.ScheduledJob{
		ID: id, Revision: 1, Name: id, Enabled: true, Tool: tool, Target: target,
		IPVersion: model.IPv4, CronExpression: "*/5 * * * *", NextRunAt: now.Add(5 * time.Minute), CreatedAt: now, UpdatedAt: now,
		ScheduleTargets: []model.ScheduleTarget{{Label: "asia"}},
	}
}

func watchMetricsJob(id string, scheduleID string, tool model.Tool, now time.Time) model.Job {
	started := now
	completed := now.Add(time.Second)
	return model.Job{
		ID: id, ScheduledID: scheduleID, ScheduledRevision: 1, Tool: tool, Target: "target.example",
		IPVersion: model.IPv4, AgentID: "edge-1", Status: model.JobSucceeded,
		CreatedAt: now, UpdatedAt: completed, StartedAt: &started, CompletedAt: &completed,
	}
}

func assertPrometheusMetric(t *testing.T, body string, name string, labels map[string]string, wantValue string) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		matches := true
		for key, value := range labels {
			if !strings.Contains(line, key+"="+strconv.Quote(value)) {
				matches = false
				break
			}
		}
		if matches && strings.HasSuffix(line, " "+wantValue) {
			return
		}
	}
	t.Fatalf("metric %s with labels %#v and value %s not found:\n%s", name, labels, wantValue, body)
}
