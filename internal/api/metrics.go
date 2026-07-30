package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/store"
)

const (
	watchMetricsCollectTimeout = 10 * time.Second
	watchMetricsHistoryLimit   = 2000
	watchMetricsMaxTextRunes   = 2048
	watchMetricsMaxDNSRecords  = 128
	watchMetricsMaxRouteHops   = 64
)

var watchResultLabelNames = []string{"schedule_id", "agent_id", "ip_version"}

type watchResultLabels struct {
	scheduleID string
	agentID    string
	ipVersion  string
}

func (l watchResultLabels) values(extra ...string) []string {
	values := make([]string, 0, len(watchResultLabelNames)+len(extra))
	values = append(values, l.scheduleID, l.agentID, l.ipVersion)
	return append(values, extra...)
}

type watchMetricsCollector struct {
	rootCtx context.Context
	store   store.Store

	scheduleInfo       *prometheus.Desc
	scheduleEnabled    *prometheus.Desc
	scheduleCron       *prometheus.Desc
	scheduleTargetInfo *prometheus.Desc
	agentInfo          *prometheus.Desc
	agentOnline        *prometheus.Desc
	agentEnabled       *prometheus.Desc
	agentLastSeen      *prometheus.Desc

	resultSuccess       *prometheus.Desc
	resultExitCode      *prometheus.Desc
	resultTimestamp     *prometheus.Desc
	resultDuration      *prometheus.Desc
	resultStatusInfo    *prometheus.Desc
	resultTargetInfo    *prometheus.Desc
	resultTextDropped   *prometheus.Desc
	pingPackets         *prometheus.Desc
	pingPacketLoss      *prometheus.Desc
	pingRTT             *prometheus.Desc
	httpDuration        *prometheus.Desc
	httpStatusCode      *prometheus.Desc
	httpDownloadBytes   *prometheus.Desc
	httpDownloadRate    *prometheus.Desc
	dnsRecordCount      *prometheus.Desc
	dnsRecordInfo       *prometheus.Desc
	portOpen            *prometheus.Desc
	portConnectDuration *prometheus.Desc
	portInfo            *prometheus.Desc
	routeHopCount       *prometheus.Desc
	routePathInfo       *prometheus.Desc
	routeHopInfo        *prometheus.Desc
	routeHopLoss        *prometheus.Desc
	routeHopRTT         *prometheus.Desc
	routeHopProbes      *prometheus.Desc

	collectSuccess   *prometheus.Desc
	collectDuration  *prometheus.Desc
	collectedResults *prometheus.Desc
}

func newWatchMetricsHandler(rootCtx context.Context, st store.Store, accessMetrics *httpAccessMetrics) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		newWatchMetricsCollector(rootCtx, st),
	)
	if accessMetrics != nil {
		registry.MustRegister(accessMetrics.collectors()...)
	}
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

func newWatchMetricsCollector(rootCtx context.Context, st store.Store) *watchMetricsCollector {
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	resultLabels := append([]string(nil), watchResultLabelNames...)
	withResultLabels := func(extra ...string) []string {
		labels := append([]string(nil), resultLabels...)
		return append(labels, extra...)
	}
	return &watchMetricsCollector{
		rootCtx: rootCtx,
		store:   st,

		scheduleInfo:       prometheus.NewDesc("mtr_watch_schedule_info", "Static information about a configured watch schedule.", []string{"schedule_id", "name", "tool", "target"}, nil),
		scheduleEnabled:    prometheus.NewDesc("mtr_watch_schedule_enabled", "Whether a watch schedule is enabled (1) or disabled (0).", []string{"schedule_id"}, nil),
		scheduleCron:       prometheus.NewDesc("mtr_watch_schedule_cron_info", "Configured UTC crontab expression for a watch schedule.", []string{"schedule_id", "cron_expression"}, nil),
		scheduleTargetInfo: prometheus.NewDesc("mtr_watch_schedule_target_info", "Configured node label for a watch schedule.", []string{"schedule_id", "node_label"}, nil),
		agentInfo:          prometheus.NewDesc("mtr_agent_info", "Static information about an agent.", []string{"agent_id", "country", "region", "provider", "isp", "version"}, nil),
		agentOnline:        prometheus.NewDesc("mtr_agent_online", "Whether an agent is online (1) or offline (0).", []string{"agent_id"}, nil),
		agentEnabled:       prometheus.NewDesc("mtr_agent_enabled", "Whether an agent is enabled (1) or manually disabled (0).", []string{"agent_id"}, nil),
		agentLastSeen:      prometheus.NewDesc("mtr_agent_last_seen_timestamp_seconds", "Unix timestamp when an agent was last seen.", []string{"agent_id"}, nil),

		resultSuccess:       prometheus.NewDesc("mtr_watch_result_success", "Whether the latest completed watch result succeeded (1) or failed (0).", resultLabels, nil),
		resultExitCode:      prometheus.NewDesc("mtr_watch_result_exit_code", "Exit code of the latest completed watch result.", resultLabels, nil),
		resultTimestamp:     prometheus.NewDesc("mtr_watch_result_timestamp_seconds", "Unix timestamp of the latest completed watch result.", resultLabels, nil),
		resultDuration:      prometheus.NewDesc("mtr_watch_result_duration_seconds", "Execution duration of the latest completed watch result.", resultLabels, nil),
		resultStatusInfo:    prometheus.NewDesc("mtr_watch_result_status_info", "Status text for the latest completed watch result.", withResultLabels("status"), nil),
		resultTargetInfo:    prometheus.NewDesc("mtr_watch_result_target_info", "Resolved target information for the latest completed watch result.", withResultLabels("resolved_target"), nil),
		resultTextDropped:   prometheus.NewDesc("mtr_watch_result_text_items_dropped", "Number of text items omitted from an info metric because of exporter limits.", withResultLabels("kind"), nil),
		pingPackets:         prometheus.NewDesc("mtr_watch_ping_packets", "Packets in the latest ping result.", withResultLabels("direction"), nil),
		pingPacketLoss:      prometheus.NewDesc("mtr_watch_ping_packet_loss_ratio", "Packet loss ratio in the latest ping result, from 0 to 1.", resultLabels, nil),
		pingRTT:             prometheus.NewDesc("mtr_watch_ping_rtt_seconds", "Round-trip time in the latest ping result.", withResultLabels("stat"), nil),
		httpDuration:        prometheus.NewDesc("mtr_watch_http_duration_seconds", "HTTP timing phase duration in the latest result.", withResultLabels("phase"), nil),
		httpStatusCode:      prometheus.NewDesc("mtr_watch_http_status_code", "HTTP response status code in the latest result.", resultLabels, nil),
		httpDownloadBytes:   prometheus.NewDesc("mtr_watch_http_download_bytes", "Response body bytes downloaded in the latest HTTP result.", resultLabels, nil),
		httpDownloadRate:    prometheus.NewDesc("mtr_watch_http_download_bytes_per_second", "Response body download rate in the latest HTTP result.", resultLabels, nil),
		dnsRecordCount:      prometheus.NewDesc("mtr_watch_dns_record_count", "DNS record count in the latest result.", resultLabels, nil),
		dnsRecordInfo:       prometheus.NewDesc("mtr_watch_dns_record_info", "DNS record text from the latest result. The metric value is always 1.", withResultLabels("record_type", "record", "record_hash", "truncated"), nil),
		portOpen:            prometheus.NewDesc("mtr_watch_port_open", "Whether the probed TCP port is open (1) or not (0).", resultLabels, nil),
		portConnectDuration: prometheus.NewDesc("mtr_watch_port_connect_duration_seconds", "TCP connect duration in the latest port result.", resultLabels, nil),
		portInfo:            prometheus.NewDesc("mtr_watch_port_info", "TCP port and peer text from the latest result. The metric value is always 1.", withResultLabels("port", "protocol", "peer"), nil),
		routeHopCount:       prometheus.NewDesc("mtr_watch_route_hop_count", "Hop count in the latest traceroute or MTR result.", resultLabels, nil),
		routePathInfo:       prometheus.NewDesc("mtr_watch_route_path_info", "Complete route path JSON from the latest traceroute or MTR result. The metric value is always 1.", withResultLabels("path", "path_hash", "truncated"), nil),
		routeHopInfo:        prometheus.NewDesc("mtr_watch_route_hop_info", "Hop text from the latest traceroute or MTR result. The metric value is always 1.", withResultLabels("hop", "host", "ip"), nil),
		routeHopLoss:        prometheus.NewDesc("mtr_watch_route_hop_loss_ratio", "Hop packet loss ratio in the latest MTR result, from 0 to 1.", withResultLabels("hop"), nil),
		routeHopRTT:         prometheus.NewDesc("mtr_watch_route_hop_rtt_seconds", "Hop round-trip time in the latest traceroute or MTR result.", withResultLabels("hop", "stat"), nil),
		routeHopProbes:      prometheus.NewDesc("mtr_watch_route_hop_probes_sent", "Probe count sent to a hop in the latest MTR result.", withResultLabels("hop"), nil),

		collectSuccess:   prometheus.NewDesc("mtr_watch_metrics_collect_success", "Whether the last watch metrics collection completed without store errors (1) or not (0).", nil, nil),
		collectDuration:  prometheus.NewDesc("mtr_watch_metrics_collect_duration_seconds", "Duration of the last watch metrics collection.", nil, nil),
		collectedResults: prometheus.NewDesc("mtr_watch_metrics_collected_results", "Number of latest completed per-agent watch results exported.", nil, nil),
	}
}

func (c *watchMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		c.scheduleInfo, c.scheduleEnabled, c.scheduleCron, c.scheduleTargetInfo,
		c.agentInfo, c.agentOnline, c.agentEnabled, c.agentLastSeen,
		c.resultSuccess, c.resultExitCode, c.resultTimestamp, c.resultDuration,
		c.resultStatusInfo, c.resultTargetInfo, c.resultTextDropped,
		c.pingPackets, c.pingPacketLoss, c.pingRTT,
		c.httpDuration, c.httpStatusCode, c.httpDownloadBytes, c.httpDownloadRate,
		c.dnsRecordCount, c.dnsRecordInfo,
		c.portOpen, c.portConnectDuration, c.portInfo,
		c.routeHopCount, c.routePathInfo, c.routeHopInfo, c.routeHopLoss, c.routeHopRTT, c.routeHopProbes,
		c.collectSuccess, c.collectDuration, c.collectedResults,
	} {
		ch <- desc
	}
}

func (c *watchMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(c.rootCtx, watchMetricsCollectTimeout)
	defer cancel()
	success := true
	results := 0

	agents, err := c.store.ListAgents(ctx)
	if err != nil {
		success = false
	} else if !c.collectAgents(ctx, ch, agents) {
		success = false
	}

	schedules, err := c.store.ListScheduledJobs(ctx)
	if err != nil {
		success = false
	} else {
		for _, schedule := range schedules {
			c.collectSchedule(ch, schedule)
			jobs, err := c.store.ListScheduledJobHistory(ctx, schedule.ID, store.ScheduledJobHistoryFilter{
				Limit:    watchMetricsHistoryLimit,
				Revision: schedule.Revision,
			})
			if err != nil {
				success = false
				continue
			}
			for _, job := range latestCompletedJobsByAgent(jobs) {
				events, err := c.store.ListJobEvents(ctx, job.ID)
				if err != nil {
					success = false
					continue
				}
				c.collectResult(ch, schedule, job, singleSummaryEvent(job, events), events)
				results++
			}
		}
	}

	ch <- prometheus.MustNewConstMetric(c.collectSuccess, prometheus.GaugeValue, boolFloat(success))
	ch <- prometheus.MustNewConstMetric(c.collectDuration, prometheus.GaugeValue, time.Since(started).Seconds())
	ch <- prometheus.MustNewConstMetric(c.collectedResults, prometheus.GaugeValue, float64(results))
}

func (c *watchMetricsCollector) collectAgents(ctx context.Context, ch chan<- prometheus.Metric, agents []model.Agent) bool {
	configs, err := c.store.ListAgentConfigs(ctx)
	if err != nil {
		return false
	}
	httpAgents, err := c.store.ListHTTPAgents(ctx)
	if err != nil {
		return false
	}
	disabled := make(map[string]bool, len(configs))
	for _, config := range configs {
		disabled[config.ID] = config.Disabled
	}
	for _, agent := range httpAgents {
		disabled[agent.ID] = !agent.Enabled
	}
	for _, agent := range agents {
		ch <- prometheus.MustNewConstMetric(c.agentInfo, prometheus.GaugeValue, 1,
			agent.ID, agent.Country, agent.Region, agent.Provider, agent.ISP, agent.Version)
		ch <- prometheus.MustNewConstMetric(c.agentOnline, prometheus.GaugeValue, boolFloat(agent.Status == model.AgentOnline), agent.ID)
		ch <- prometheus.MustNewConstMetric(c.agentEnabled, prometheus.GaugeValue, boolFloat(!disabled[agent.ID]), agent.ID)
		if !agent.LastSeenAt.IsZero() {
			ch <- prometheus.MustNewConstMetric(c.agentLastSeen, prometheus.GaugeValue, float64(agent.LastSeenAt.Unix()), agent.ID)
		}
	}
	return true
}

func (c *watchMetricsCollector) collectSchedule(ch chan<- prometheus.Metric, schedule model.ScheduledJob) {
	ch <- prometheus.MustNewConstMetric(c.scheduleInfo, prometheus.GaugeValue, 1,
		schedule.ID, schedule.Name, string(schedule.Tool), schedule.Target)
	ch <- prometheus.MustNewConstMetric(c.scheduleEnabled, prometheus.GaugeValue, boolFloat(schedule.Enabled), schedule.ID)
	ch <- prometheus.MustNewConstMetric(c.scheduleCron, prometheus.GaugeValue, 1, schedule.ID, schedule.CronExpression)
	for _, target := range schedule.ScheduleTargets {
		ch <- prometheus.MustNewConstMetric(c.scheduleTargetInfo, prometheus.GaugeValue, 1, schedule.ID, target.Label)
	}
}

func latestCompletedJobsByAgent(jobs []model.Job) []model.Job {
	latest := make(map[string]model.Job)
	for _, job := range jobs {
		if job.Status != model.JobSucceeded && job.Status != model.JobFailed && job.Status != model.JobCanceled {
			continue
		}
		existing, ok := latest[job.AgentID]
		if !ok || scheduleHistoryEventTime(job).After(scheduleHistoryEventTime(existing)) {
			latest[job.AgentID] = job
		}
	}
	out := make([]model.Job, 0, len(latest))
	for _, job := range latest {
		out = append(out, job)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

func (c *watchMetricsCollector) collectResult(ch chan<- prometheus.Metric, schedule model.ScheduledJob, job model.Job, event model.JobEvent, events []model.JobEvent) {
	agentID := event.AgentID
	if agentID == "" {
		agentID = job.AgentID
	}
	result := event.Parsed
	if result == nil {
		result = &model.ToolResult{
			Tool:      job.Tool,
			Target:    job.Target,
			IPVersion: job.IPVersion,
			ExitCode:  scheduleSummaryExitCode(job),
			Summary:   map[string]any{},
		}
		if event.Event != nil {
			result.Summary = event.Event.Metric
		}
		if event.ExitCode != nil {
			result.ExitCode = *event.ExitCode
		}
	}
	result = resultWithRouteEventHops(job.Tool, result, events)
	ipVersion := job.IPVersion
	if result.IPVersion != model.IPAny {
		ipVersion = result.IPVersion
	}
	labels := watchResultLabels{scheduleID: schedule.ID, agentID: agentID, ipVersion: strconv.Itoa(int(ipVersion))}

	succeeded := job.Status == model.JobSucceeded && result.ExitCode == 0
	ch <- prometheus.MustNewConstMetric(c.resultSuccess, prometheus.GaugeValue, boolFloat(succeeded), labels.values()...)
	ch <- prometheus.MustNewConstMetric(c.resultExitCode, prometheus.GaugeValue, float64(result.ExitCode), labels.values()...)
	resultTime := event.CreatedAt
	if job.CompletedAt != nil && !job.CompletedAt.IsZero() {
		resultTime = *job.CompletedAt
	}
	if !resultTime.IsZero() {
		ch <- prometheus.MustNewConstMetric(c.resultTimestamp, prometheus.GaugeValue, float64(resultTime.Unix()), labels.values()...)
	}
	if job.StartedAt != nil && job.CompletedAt != nil {
		duration := job.CompletedAt.Sub(*job.StartedAt).Seconds()
		if duration >= 0 {
			ch <- prometheus.MustNewConstMetric(c.resultDuration, prometheus.GaugeValue, duration, labels.values()...)
		}
	}
	status := string(job.Status)
	if value, ok := textValue(result.Summary, "status"); ok {
		status = value
	}
	status, _, _ = limitedInfoText(status)
	ch <- prometheus.MustNewConstMetric(c.resultStatusInfo, prometheus.GaugeValue, 1, labels.values(status)...)
	resolvedTarget := firstTextValue(result.Summary, "target_ip", "remote_addr", "peer")
	if resolvedTarget == "" {
		resolvedTarget = job.ResolvedTarget
	}
	if resolvedTarget != "" {
		resolvedTarget, _, _ = limitedInfoText(resolvedTarget)
		ch <- prometheus.MustNewConstMetric(c.resultTargetInfo, prometheus.GaugeValue, 1, labels.values(resolvedTarget)...)
	}

	switch job.Tool {
	case model.ToolPing:
		c.collectPing(ch, labels, result.Summary)
	case model.ToolHTTP:
		c.collectHTTP(ch, labels, result.Summary)
	case model.ToolDNS:
		c.collectDNS(ch, labels, result)
	case model.ToolPort:
		c.collectPort(ch, labels, result.Summary)
	case model.ToolTraceroute, model.ToolMTR:
		c.collectRoute(ch, labels, result)
	}
}

func resultWithRouteEventHops(tool model.Tool, result *model.ToolResult, events []model.JobEvent) *model.ToolResult {
	if result == nil || (tool != model.ToolTraceroute && tool != model.ToolMTR) || len(result.Hops) > 0 {
		return result
	}
	hops := routeHopsFromEvents(events)
	if len(hops) == 0 {
		return result
	}
	cloned := *result
	cloned.Hops = hops
	return &cloned
}

type routeHopCandidate struct {
	hop      model.HopResult
	priority int
}

func routeHopsFromEvents(events []model.JobEvent) []model.HopResult {
	byIndex := map[int]routeHopCandidate{}
	setHop := func(hop model.HopResult, priority int) {
		if hop.Index <= 0 {
			return
		}
		hop = hop.FlattenSingleProbe()
		current, ok := byIndex[hop.Index]
		if !ok || priority >= current.priority {
			byIndex[hop.Index] = routeHopCandidate{hop: hop, priority: priority}
		}
	}
	for _, event := range events {
		if event.Parsed != nil {
			for _, hop := range event.Parsed.Hops {
				setHop(hop, 3)
			}
		}
		if event.Event == nil || event.Event.Hop == nil {
			continue
		}
		typ := event.Event.Type
		if typ == "" {
			typ = event.Stream
		}
		priority := 1
		if typ == "hop_summary" {
			priority = 2
		}
		setHop(*event.Event.Hop, priority)
	}
	hops := make([]model.HopResult, 0, len(byIndex))
	for _, candidate := range byIndex {
		hops = append(hops, candidate.hop)
	}
	sort.Slice(hops, func(i, j int) bool { return hops[i].Index < hops[j].Index })
	return hops
}

func (c *watchMetricsCollector) collectPing(ch chan<- prometheus.Metric, labels watchResultLabels, summary map[string]any) {
	for direction, key := range map[string]string{"transmitted": "packets_transmitted", "received": "packets_received"} {
		if value, ok := numberValue(summary, key); ok {
			ch <- prometheus.MustNewConstMetric(c.pingPackets, prometheus.GaugeValue, value, labels.values(direction)...)
		}
	}
	if value, ok := numberValue(summary, "packet_loss_pct"); ok {
		ch <- prometheus.MustNewConstMetric(c.pingPacketLoss, prometheus.GaugeValue, percentRatio(value), labels.values()...)
	}
	for stat, key := range map[string]string{"min": "rtt_min_ms", "avg": "rtt_avg_ms", "max": "rtt_max_ms", "mdev": "rtt_mdev_ms"} {
		if value, ok := numberValue(summary, key); ok {
			ch <- prometheus.MustNewConstMetric(c.pingRTT, prometheus.GaugeValue, millisecondsSeconds(value), labels.values(stat)...)
		}
	}
}

func (c *watchMetricsCollector) collectHTTP(ch chan<- prometheus.Metric, labels watchResultLabels, summary map[string]any) {
	for phase, key := range map[string]string{
		"total": "time_total_ms", "dns": "time_dns_ms", "connect": "time_connect_ms",
		"tls": "time_tls_ms", "first_byte": "time_first_byte_ms", "download": "time_download_ms",
	} {
		if value, ok := numberValue(summary, key); ok {
			ch <- prometheus.MustNewConstMetric(c.httpDuration, prometheus.GaugeValue, millisecondsSeconds(value), labels.values(phase)...)
		}
	}
	if value, ok := numberValue(summary, "http_code"); ok {
		ch <- prometheus.MustNewConstMetric(c.httpStatusCode, prometheus.GaugeValue, value, labels.values()...)
	}
	if value, ok := numberValue(summary, "bytes_downloaded"); ok {
		ch <- prometheus.MustNewConstMetric(c.httpDownloadBytes, prometheus.GaugeValue, value, labels.values()...)
	}
	if value, ok := numberValue(summary, "download_bytes_per_sec"); ok {
		ch <- prometheus.MustNewConstMetric(c.httpDownloadRate, prometheus.GaugeValue, value, labels.values()...)
	}
}

func (c *watchMetricsCollector) collectDNS(ch chan<- prometheus.Metric, labels watchResultLabels, result *model.ToolResult) {
	count := float64(len(result.Records))
	if value, ok := numberValue(result.Summary, "record_count"); ok {
		count = value
	}
	ch <- prometheus.MustNewConstMetric(c.dnsRecordCount, prometheus.GaugeValue, count, labels.values()...)
	records := append([]model.DNSRecord(nil), result.Records...)
	sort.Slice(records, func(i, j int) bool {
		if records[i].Type == records[j].Type {
			return records[i].Value < records[j].Value
		}
		return records[i].Type < records[j].Type
	})
	dropped := 0
	if len(records) > watchMetricsMaxDNSRecords {
		dropped = len(records) - watchMetricsMaxDNSRecords
		records = records[:watchMetricsMaxDNSRecords]
	}
	for _, record := range records {
		value, hash, truncated := limitedInfoText(record.Value)
		ch <- prometheus.MustNewConstMetric(c.dnsRecordInfo, prometheus.GaugeValue, 1,
			labels.values(record.Type, value, hash, strconv.FormatBool(truncated))...)
	}
	if dropped > 0 {
		ch <- prometheus.MustNewConstMetric(c.resultTextDropped, prometheus.GaugeValue, float64(dropped), labels.values("dns_record")...)
	}
}

func (c *watchMetricsCollector) collectPort(ch chan<- prometheus.Metric, labels watchResultLabels, summary map[string]any) {
	status, _ := textValue(summary, "status")
	ch <- prometheus.MustNewConstMetric(c.portOpen, prometheus.GaugeValue, boolFloat(status == "open"), labels.values()...)
	if value, ok := numberValue(summary, "connect_ms"); ok {
		ch <- prometheus.MustNewConstMetric(c.portConnectDuration, prometheus.GaugeValue, millisecondsSeconds(value), labels.values()...)
	}
	port, portOK := numberValue(summary, "port")
	protocol, _ := textValue(summary, "protocol")
	peer, _ := textValue(summary, "peer")
	if portOK || protocol != "" || peer != "" {
		portText := ""
		if portOK {
			portText = strconv.FormatInt(int64(port), 10)
		}
		peer, _, _ = limitedInfoText(peer)
		ch <- prometheus.MustNewConstMetric(c.portInfo, prometheus.GaugeValue, 1, labels.values(portText, protocol, peer)...)
	}
}

func (c *watchMetricsCollector) collectRoute(ch chan<- prometheus.Metric, labels watchResultLabels, result *model.ToolResult) {
	hops := append([]model.HopResult(nil), result.Hops...)
	sort.Slice(hops, func(i, j int) bool { return hops[i].Index < hops[j].Index })
	count := float64(len(hops))
	if value, ok := numberValue(result.Summary, "hop_count"); ok {
		count = value
	}
	ch <- prometheus.MustNewConstMetric(c.routeHopCount, prometheus.GaugeValue, count, labels.values()...)
	dropped := 0
	if len(hops) > watchMetricsMaxRouteHops {
		dropped = len(hops) - watchMetricsMaxRouteHops
		hops = hops[:watchMetricsMaxRouteHops]
	}
	if len(hops) > 0 {
		pathJSON, _ := json.Marshal(routePath(hops))
		path, hash, truncated := limitedInfoText(string(pathJSON))
		ch <- prometheus.MustNewConstMetric(c.routePathInfo, prometheus.GaugeValue, 1,
			labels.values(path, hash, strconv.FormatBool(truncated))...)
	}
	for _, hop := range hops {
		hopLabel := strconv.Itoa(hop.Index)
		host, _, _ := limitedInfoText(hop.Host)
		ip, _, _ := limitedInfoText(hop.IP)
		ch <- prometheus.MustNewConstMetric(c.routeHopInfo, prometheus.GaugeValue, 1, labels.values(hopLabel, host, ip)...)
		if hop.LossPct != nil {
			ch <- prometheus.MustNewConstMetric(c.routeHopLoss, prometheus.GaugeValue, percentRatio(*hop.LossPct), labels.values(hopLabel)...)
		}
		if hop.Sent != nil {
			ch <- prometheus.MustNewConstMetric(c.routeHopProbes, prometheus.GaugeValue, float64(*hop.Sent), labels.values(hopLabel)...)
		}
		for stat, value := range routeRTTValues(hop) {
			ch <- prometheus.MustNewConstMetric(c.routeHopRTT, prometheus.GaugeValue, millisecondsSeconds(value), labels.values(hopLabel, stat)...)
		}
	}
	if dropped > 0 {
		ch <- prometheus.MustNewConstMetric(c.resultTextDropped, prometheus.GaugeValue, float64(dropped), labels.values("route_hop")...)
	}
}

type routePathHop struct {
	Hop     int    `json:"hop"`
	Host    string `json:"host,omitempty"`
	IP      string `json:"ip,omitempty"`
	Timeout bool   `json:"timeout,omitempty"`
}

func routePath(hops []model.HopResult) []routePathHop {
	out := make([]routePathHop, 0, len(hops))
	for _, hop := range hops {
		out = append(out, routePathHop{Hop: hop.Index, Host: hop.Host, IP: hop.IP, Timeout: hop.Timeout})
	}
	return out
}

func routeRTTValues(hop model.HopResult) map[string]float64 {
	out := map[string]float64{}
	for name, value := range map[string]*float64{
		"rtt": hop.RTTMS, "avg": hop.AvgMS, "best": hop.BestMS,
		"worst": hop.WorstMS, "last": hop.LastMS, "stdev": hop.StdevMS,
	} {
		if value != nil && finite(*value) {
			out[name] = *value
		}
	}
	return out
}

func numberValue(values map[string]any, key string) (float64, bool) {
	if values == nil {
		return 0, false
	}
	var value float64
	switch raw := values[key].(type) {
	case float64:
		value = raw
	case float32:
		value = float64(raw)
	case int:
		value = float64(raw)
	case int8:
		value = float64(raw)
	case int16:
		value = float64(raw)
	case int32:
		value = float64(raw)
	case int64:
		value = float64(raw)
	case uint:
		value = float64(raw)
	case uint8:
		value = float64(raw)
	case uint16:
		value = float64(raw)
	case uint32:
		value = float64(raw)
	case uint64:
		value = float64(raw)
	case json.Number:
		parsed, err := raw.Float64()
		if err != nil {
			return 0, false
		}
		value = parsed
	default:
		return 0, false
	}
	return value, finite(value)
}

func textValue(values map[string]any, key string) (string, bool) {
	if values == nil {
		return "", false
	}
	value, ok := values[key].(string)
	return strings.TrimSpace(value), ok && strings.TrimSpace(value) != ""
}

func firstTextValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := textValue(values, key); ok {
			return value
		}
	}
	return ""
}

func limitedInfoText(value string) (string, string, bool) {
	hashBytes := sha256.Sum256([]byte(value))
	hash := hex.EncodeToString(hashBytes[:16])
	runes := []rune(value)
	if len(runes) <= watchMetricsMaxTextRunes {
		return value, hash, false
	}
	return string(runes[:watchMetricsMaxTextRunes]) + "…", hash, true
}

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func millisecondsSeconds(value float64) float64 {
	return value / 1000
}

func percentRatio(value float64) float64 {
	return value / 100
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func (s *Server) getMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.scheduleReadAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "schedule read access is required to read metrics")
		return
	}
	s.metrics.ServeHTTP(w, r)
}
