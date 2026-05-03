package model

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

type Tool string

const (
	ToolPing       Tool = "ping"
	ToolTraceroute Tool = "traceroute"
	ToolMTR        Tool = "mtr"
	ToolHTTP       Tool = "http"
	ToolDNS        Tool = "dns"
	ToolPort       Tool = "port"
)

type IPVersion int

const (
	IPAny IPVersion = 0
	IPv4  IPVersion = 4
	IPv6  IPVersion = 6
)

type ProtocolMask uint8

const (
	ProtocolIPv4 ProtocolMask = 1 << iota
	ProtocolIPv6
)

const ProtocolAll = ProtocolIPv4 | ProtocolIPv6

type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCanceled  JobStatus = "canceled"
)

type AgentStatus string

const (
	AgentOnline  AgentStatus = "online"
	AgentOffline AgentStatus = "offline"
)

type Job struct {
	ID                string            `json:"id"`
	ParentID          string            `json:"parent_id,omitempty"`
	ScheduledID       string            `json:"scheduled_id,omitempty"`
	ScheduledRevision int               `json:"scheduled_revision,omitempty"`
	Tool              Tool              `json:"tool"`
	Target            string            `json:"target"`
	ResolvedTarget    string            `json:"resolved_target,omitempty"`
	Args              map[string]string `json:"args,omitempty"`
	IPVersion         IPVersion         `json:"ip_version,omitempty"`
	AgentID           string            `json:"agent_id,omitempty"`
	ResolveOnAgent    bool              `json:"resolve_on_agent,omitempty"`
	Status            JobStatus         `json:"status"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
	StartedAt         *time.Time        `json:"started_at,omitempty"`
	CompletedAt       *time.Time        `json:"completed_at,omitempty"`
	Error             string            `json:"-"`
}

const (
	JobErrorTargetBlocked       = "target_blocked"
	JobErrorUnsupportedTool     = "unsupported_tool"
	JobErrorUnsupportedProtocol = "unsupported_protocol"
	JobErrorAgentDisconnected   = "agent_disconnected"
	JobErrorTimeout             = "job_timeout"
	JobErrorToolFailed          = "tool_failed"
	JobErrorFanoutFailed        = "fanout_failed"
	JobErrorFailed              = "job_failed"
)

func (j Job) MarshalJSON() ([]byte, error) {
	type jobAlias Job
	type wireJob struct {
		jobAlias
		ErrorType string `json:"error_type,omitempty"`
	}
	errorType := ""
	if j.Status == JobFailed || j.Status == JobCanceled {
		errorType = PublicJobErrorType(j.Error)
	}
	return json.Marshal(wireJob{
		jobAlias:  jobAlias(j),
		ErrorType: errorType,
	})
}

func PublicJobErrorType(message string) string {
	switch strings.TrimSpace(strings.ToLower(message)) {
	case "":
		return ""
	case JobErrorTargetBlocked:
		return JobErrorTargetBlocked
	case JobErrorUnsupportedTool:
		return JobErrorUnsupportedTool
	case JobErrorUnsupportedProtocol:
		return JobErrorUnsupportedProtocol
	case JobErrorAgentDisconnected, "agent disconnected":
		return JobErrorAgentDisconnected
	case JobErrorTimeout, "job timed out", "timeout":
		return JobErrorTimeout
	case JobErrorToolFailed, "agent tool failed":
		return JobErrorToolFailed
	case JobErrorFanoutFailed, "one or more fanout jobs failed":
		return JobErrorFanoutFailed
	default:
		return JobErrorFailed
	}
}

type JobEvent struct {
	ID        string       `json:"id"`
	JobID     string       `json:"job_id"`
	AgentID   string       `json:"agent_id,omitempty"`
	Stream    string       `json:"stream"`
	Event     *StreamEvent `json:"event,omitempty"`
	ExitCode  *int         `json:"exit_code,omitempty"`
	Parsed    *ToolResult  `json:"parsed,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
}

func (e JobEvent) MarshalJSON() ([]byte, error) {
	type wireEvent struct {
		ID        string    `json:"id"`
		JobID     string    `json:"job_id"`
		AgentID   string    `json:"agent_id,omitempty"`
		Event     any       `json:"event,omitempty"`
		CreatedAt time.Time `json:"created_at"`
	}
	return json.Marshal(wireEvent{
		ID:        e.ID,
		JobID:     e.JobID,
		AgentID:   e.AgentID,
		Event:     e.Payload(),
		CreatedAt: e.CreatedAt,
	})
}

func (e JobEvent) Payload() any {
	if e.Parsed != nil {
		return e.Parsed.EventPayload()
	}
	if e.Event != nil {
		return e.Event.EventPayload()
	}
	return nil
}

func (e JobEvent) EventType() string {
	if e.Parsed != nil {
		if e.Parsed.Type != "" {
			return e.Parsed.Type
		}
		return "summary"
	}
	if e.Event != nil && e.Event.Type != "" {
		return e.Event.Type
	}
	if e.Stream != "" {
		return e.Stream
	}
	return "message"
}

type StreamEvent struct {
	Type    string         `json:"type"`
	Message string         `json:"message,omitempty"`
	Hop     *HopResult     `json:"hop,omitempty"`
	Metric  map[string]any `json:"metric,omitempty"`
}

func (e StreamEvent) EventPayload() map[string]any {
	return e.payload(true)
}

func (e StreamEvent) WirePayload() map[string]any {
	return e.payload(false)
}

func (e StreamEvent) payload(public bool) map[string]any {
	out := map[string]any{}
	if e.Type != "" {
		out["type"] = e.Type
	}
	if e.Message != "" {
		out["message"] = e.Message
	}
	if e.Hop != nil {
		out["hop"] = e.Hop
	}
	if metric := metricPayload(e.Metric, public); len(metric) > 0 {
		out["metric"] = metric
	}
	return out
}

type ToolResult struct {
	Type      string         `json:"type,omitempty"`
	Tool      Tool           `json:"tool"`
	Target    string         `json:"target"`
	IPVersion IPVersion      `json:"ip_version,omitempty"`
	ExitCode  int            `json:"exit_code"`
	Summary   map[string]any `json:"summary,omitempty"`
	Hops      []HopResult    `json:"hops,omitempty"`
	Records   []DNSRecord    `json:"records,omitempty"`
}

func (r ToolResult) EventPayload() map[string]any {
	return r.eventPayload(true)
}

func (r ToolResult) WirePayload() map[string]any {
	return r.eventPayload(false)
}

func (r ToolResult) eventPayload(public bool) map[string]any {
	eventType := r.Type
	if eventType == "" {
		eventType = "summary"
	}
	out := map[string]any{
		"type": eventType,
	}
	out["exit_code"] = r.ExitCode
	if len(r.Summary) > 0 {
		if metric := metricPayload(r.Summary, public); len(metric) > 0 {
			out["metric"] = metric
		}
	}
	if len(r.Hops) > 0 {
		out["hops"] = r.Hops
	}
	if len(r.Records) > 0 {
		out["records"] = sortedDNSRecords(r.Records)
	}
	return out
}

func sortedDNSRecords(records []DNSRecord) []DNSRecord {
	out := append([]DNSRecord(nil), records...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type == out[j].Type {
			return out[i].Value < out[j].Value
		}
		return out[i].Type < out[j].Type
	})
	return out
}

func metricPayload(metric map[string]any, public bool) map[string]any {
	out := make(map[string]any, len(metric))
	hadError := false
	for key, value := range metric {
		if public && key == "error" {
			hadError = true
			continue
		}
		out[key] = value
	}
	if public && hadError && len(out) == 1 {
		if _, ok := out["status"]; ok {
			return map[string]any{}
		}
	}
	return out
}

type HopResult struct {
	Index   int           `json:"index"`
	Host    string        `json:"host,omitempty"`
	IP      string        `json:"ip,omitempty"`
	RTTMS   *float64      `json:"rtt_ms,omitempty"`
	Timeout bool          `json:"timeout,omitempty"`
	Probes  []ProbeResult `json:"probes,omitempty"`
	LossPct *float64      `json:"loss_pct,omitempty"`
	Sent    *int          `json:"sent,omitempty"`
	AvgMS   *float64      `json:"avg_ms,omitempty"`
	BestMS  *float64      `json:"best_ms,omitempty"`
	WorstMS *float64      `json:"worst_ms,omitempty"`
	LastMS  *float64      `json:"last_ms,omitempty"`
	StdevMS *float64      `json:"stdev_ms,omitempty"`
	TimesMS []float64     `json:"times_ms,omitempty"`
}

func (h HopResult) FlattenSingleProbe() HopResult {
	if len(h.Probes) == 0 {
		return h
	}
	probe := h.Probes[0]
	if h.Host == "" {
		h.Host = probe.Host
	}
	if h.IP == "" {
		h.IP = probe.IP
	}
	if h.RTTMS == nil {
		h.RTTMS = probe.RTTMS
	}
	h.Timeout = probe.Timeout
	h.Probes = nil
	return h
}

type ProbeResult struct {
	Host    string   `json:"host,omitempty"`
	IP      string   `json:"ip,omitempty"`
	RTTMS   *float64 `json:"rtt_ms,omitempty"`
	Timeout bool     `json:"timeout,omitempty"`
}

type DNSRecord struct {
	Type  string `json:"type,omitempty"`
	Value string `json:"value"`
}

type Agent struct {
	ID           string       `json:"id"`
	Country      string       `json:"country,omitempty"`
	Region       string       `json:"region,omitempty"`
	Provider     string       `json:"provider,omitempty"`
	ISP          string       `json:"isp,omitempty"`
	Version      string       `json:"version,omitempty"`
	Labels       []string     `json:"labels,omitempty"`
	TokenHash    string       `json:"-"`
	Capabilities []Tool       `json:"capabilities"`
	Protocols    ProtocolMask `json:"protocols"`
	Status       AgentStatus  `json:"status"`
	LastSeenAt   time.Time    `json:"last_seen_at"`
	CreatedAt    time.Time    `json:"created_at"`
}

type AgentToolPermission struct {
	AllowedArgs    map[string]string `json:"allowed_args,omitempty"`
	ResolveOnAgent *bool             `json:"resolve_on_agent,omitempty"`
	IPVersions     []IPVersion       `json:"ip_versions,omitempty"`
	RequiresAgent  bool              `json:"requires_agent"`
}

type AgentView struct {
	ID         string                       `json:"id"`
	Country    string                       `json:"country,omitempty"`
	Region     string                       `json:"region,omitempty"`
	Provider   string                       `json:"provider,omitempty"`
	ISP        string                       `json:"isp,omitempty"`
	Version    string                       `json:"version,omitempty"`
	Labels     []string                     `json:"labels,omitempty"`
	Tools      map[Tool]AgentToolPermission `json:"tools"`
	Protocols  ProtocolMask                 `json:"protocols"`
	Status     AgentStatus                  `json:"status"`
	LastSeenAt time.Time                    `json:"last_seen_at"`
	CreatedAt  time.Time                    `json:"created_at"`
}

const (
	AgentAllLabel      = "agent"
	AgentGRPCLabel     = "agent:grpc"
	AgentHTTPLabel     = "agent:http"
	AgentIDLabelPrefix = "id:"
)

type AgentTransport string

const (
	AgentTransportGRPC AgentTransport = "grpc"
	AgentTransportHTTP AgentTransport = "http"
)

func AgentIDLabel(agentID string) string {
	return AgentIDLabelPrefix + strings.TrimSpace(agentID)
}

func NormalizeAgentLabels(agentID string, transport AgentTransport, labels []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(labels)+2)
	add := func(label string) {
		label = strings.TrimSpace(label)
		if label == "" {
			return
		}
		if _, ok := seen[label]; ok {
			return
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	add(AgentAllLabel)
	switch transport {
	case AgentTransportGRPC:
		add(AgentGRPCLabel)
	case AgentTransportHTTP:
		add(AgentHTTPLabel)
	}
	add(AgentIDLabel(agentID))
	for _, label := range labels {
		if IsReservedAgentLabel(label) {
			continue
		}
		add(label)
	}
	return out
}

func IsReservedAgentLabel(label string) bool {
	label = strings.TrimSpace(label)
	return label == AgentAllLabel || label == AgentGRPCLabel || label == AgentHTTPLabel || strings.HasPrefix(label, AgentIDLabelPrefix)
}

type CreateJobRequest struct {
	Tool           Tool              `json:"tool"`
	Target         string            `json:"target"`
	Args           map[string]string `json:"args,omitempty"`
	IPVersion      IPVersion         `json:"ip_version,omitempty"`
	AgentID        string            `json:"agent_id,omitempty"`
	ResolveOnAgent bool              `json:"resolve_on_agent,omitempty"`
}

type ScheduleTarget struct {
	ID              string     `json:"id,omitempty"`
	Label           string     `json:"label"`
	AllowedAgentIDs []string   `json:"allowed_agent_ids,omitempty"`
	IntervalSeconds int        `json:"interval_seconds"`
	NextRunAt       time.Time  `json:"next_run_at"`
	LastRunAt       *time.Time `json:"last_run_at,omitempty"`
}

type ScheduleTargetRequest struct {
	Label           string `json:"label"`
	IntervalSeconds int    `json:"interval_seconds"`
}

type ScheduledJob struct {
	ID              string            `json:"id"`
	Revision        int               `json:"revision"`
	Name            string            `json:"name,omitempty"`
	Enabled         bool              `json:"enabled"`
	Tool            Tool              `json:"tool"`
	Target          string            `json:"target"`
	Args            map[string]string `json:"args,omitempty"`
	IPVersion       IPVersion         `json:"ip_version,omitempty"`
	ResolveOnAgent  bool              `json:"resolve_on_agent,omitempty"`
	IntervalSeconds int               `json:"interval_seconds"`
	NextRunAt       time.Time         `json:"next_run_at"`
	LastRunAt       *time.Time        `json:"last_run_at,omitempty"`
	ScheduleTargets []ScheduleTarget  `json:"schedule_targets,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

type CreateScheduledJobRequest struct {
	Name            string                  `json:"name,omitempty"`
	Enabled         *bool                   `json:"enabled,omitempty"`
	Tool            Tool                    `json:"tool"`
	Target          string                  `json:"target"`
	Args            map[string]string       `json:"args,omitempty"`
	IPVersion       IPVersion               `json:"ip_version,omitempty"`
	ResolveOnAgent  bool                    `json:"resolve_on_agent,omitempty"`
	ScheduleTargets []ScheduleTargetRequest `json:"schedule_targets,omitempty"`
}

func (s ScheduledJob) EffectiveScheduleTargets() []ScheduleTarget {
	return append([]ScheduleTarget(nil), s.ScheduleTargets...)
}
