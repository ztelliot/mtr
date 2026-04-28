import { extractHops, getHop, getMetric, latestToolResult, toolResultFromEvent } from "./events";
import type { Agent, HopResult, Job, JobEvent, Tool } from "./types";

export interface NodeResultRow {
  agentId: string;
  country?: string;
  region: string;
  provider: string;
  isp?: string;
  protocols?: number;
  target: string;
  status: string;
  ip?: string;
  sent?: number;
  lastMS?: number;
  avgMS?: number;
  bestMS?: number;
  worstMS?: number;
  stdevMS?: number;
  lossPct?: number;
  rttSamples?: Array<number | null>;
  connectMS?: number;
  records?: string[];
  httpCode?: number;
  totalMS?: number;
  dnsMS?: number;
  tlsMS?: number;
  firstByteMS?: number;
  downloadMS?: number;
  downloadSpeed?: number;
  bytesDownloaded?: number;
}

export interface MtrResultRow extends Omit<NodeResultRow, "agentId" | "target" | "records" | "httpCode" | "totalMS" | "dnsMS" | "tlsMS" | "firstByteMS" | "downloadMS" | "downloadSpeed" | "bytesDownloaded" | "connectMS"> {
  hop: number;
}

export const fanoutTools: Tool[] = ["ping", "dns", "port", "http"];

export function isFanoutTool(tool: Tool): boolean {
  return fanoutTools.includes(tool);
}

export function capableAgents(agents: Agent[] | null | undefined, tool: Tool): Agent[] {
  return safeAgents(agents).filter(
    (agent) =>
      agent.status === "online" &&
      Array.isArray(agent.capabilities) &&
      agent.capabilities.includes(tool)
  );
}

export function pingCapableAgents(agents: Agent[] | null | undefined): Agent[] {
  return capableAgents(agents, "ping");
}

export function buildNodeRows(
  tool: Tool,
  agents: Agent[] | null | undefined,
  jobs: Job[],
  eventsByJobId: Record<string, JobEvent[]>
): NodeResultRow[] {
  const safe = safeAgents(agents);
  const agentByID = new Map(safe.map((agent) => [agent.id, agent]));

  return jobs
    .flatMap((job) => {
      const events = eventsByJobId[job.id] ?? [];
      const blockedAgentIDs = targetBlockedAgentIDs(events);
      return agentIDsForJob(job, events, blockedAgentIDs).map((agentID) =>
        nodeRowForJob(tool, agentByID, job, eventsForAgent(events, agentID, blockedAgentIDs), agentID)
      );
    })
    .sort(compareNodeRows);
}

function nodeRowForJob(
  tool: Tool,
  agentByID: Map<string, Agent>,
  job: Job,
  events: JobEvent[],
  agentID: string
): NodeResultRow {
  const agent = agentByID.get(agentID);
  const parsed = latestToolResult(events, job);
  const summary = parsed?.summary ?? {};
  const metrics = metricSamples(events);
  const base = {
    agentId: agentID,
    country: normalizeCountry(agent?.country),
    region: agent?.region || "-",
    provider: agent?.provider || agent?.name || agentID || "-",
    isp: agent?.isp,
    protocols: singleProtocolMask(agent?.protocols),
    target: parsed?.target || job.target,
    status: parsed ? statusFromParsed(parsed.exit_code, summary.status) : metrics.length > 0 ? "running" : job.status
  };

  if (tool === "ping") {
    const metricRTTSamples = metrics.map((metric) => metric.latency ?? null);
    const live = summarizeSamples(metricRTTSamples.flatMap((sample) => (sample === null ? [] : [sample])));
    const sent = firstNumber(summary.packets_transmitted) ?? maxSeq(metrics) ?? metrics.length;
    const received =
      firstNumber(summary.packets_received) ??
      metrics.filter((metric) => metric.latency !== undefined && !metric.timeout).length;
    const lossPct = firstNumber(summary.packet_loss_pct) ?? lossPercent(sent, received);
    const rttSamples = completeTimeoutSamples(metricRTTSamples, sent, lossPct);
    return {
      ...base,
      ip: targetResolvedIP(events) || job.resolved_target || "-",
      sent,
      lastMS: live.lastMS,
      bestMS: firstNumber(summary.rtt_min_ms) ?? live.bestMS,
      avgMS: firstNumber(summary.rtt_avg_ms) ?? live.avgMS,
      worstMS: firstNumber(summary.rtt_max_ms) ?? live.worstMS,
      stdevMS: firstNumber(summary.rtt_mdev_ms) ?? live.stdevMS,
      lossPct,
      rttSamples
    };
  }

  if (tool === "dns") {
    return {
      ...base,
      records: sortDNSRecordLines(parsed?.records?.map((record) => `${record.type || "DNS"} ${record.value}`) ?? [])
    };
  }

  if (tool === "port") {
    return {
      ...base,
      ip: stringSummary(summary.peer),
      connectMS: firstNumber(summary.connect_ms),
      status: stringSummary(summary.status) || base.status
    };
  }

  if (tool === "http") {
    const httpCode = firstNumber(summary.http_code);
    return {
      ...base,
      ip: remoteAddressIP(summary.remote_addr),
      httpCode,
      status: httpCode === undefined ? base.status : String(httpCode),
      totalMS: firstNumber(summary.time_total_ms),
      dnsMS: firstNumber(summary.time_dns_ms),
      connectMS: firstNumber(summary.time_connect_ms),
      tlsMS: firstNumber(summary.time_tls_ms),
      firstByteMS: firstNumber(summary.time_first_byte_ms),
      downloadMS: firstNumber(summary.time_download_ms),
      downloadSpeed: firstNumber(summary.download_bytes_per_sec),
      bytesDownloaded: firstNumber(summary.bytes_downloaded)
    };
  }

  return base;
}

function agentIDsForJob(job: Job, events: JobEvent[], blockedAgentIDs: Set<string>): string[] {
  if (job.agent_id) {
    return blockedAgentIDs.has(job.agent_id) ? [] : [job.agent_id];
  }
  const ids = new Set<string>();
  for (const event of events) {
    if (event.agent_id && !blockedAgentIDs.has(event.agent_id) && isDisplayableResultEvent(event)) {
      ids.add(event.agent_id);
    }
  }
  return [...ids];
}

function eventsForAgent(events: JobEvent[], agentID: string, blockedAgentIDs: Set<string>): JobEvent[] {
  if (blockedAgentIDs.has(agentID)) {
    return [];
  }
  return events.filter(
    (event) => !isTargetBlockedEvent(event) && (event.agent_id === agentID || (!event.agent_id && !agentID))
  );
}

function targetBlockedAgentIDs(events: JobEvent[]): Set<string> {
  const ids = new Set<string>();
  for (const event of events) {
    if (event.agent_id && isTargetBlockedEvent(event)) {
      ids.add(event.agent_id);
    }
  }
  return ids;
}

function isDisplayableResultEvent(event: JobEvent): boolean {
  if (isTargetBlockedEvent(event)) {
    return false;
  }
  if (event.parsed || event.exit_code !== undefined || event.event?.exit_code !== undefined) {
    return true;
  }
  const type = event.event?.type || event.stream;
  return type === "metric" || type === "summary";
}

function isTargetBlockedEvent(event: JobEvent): boolean {
  return event.event?.type === "target_blocked" || event.event?.message === "target_blocked" || event.stream === "target_blocked";
}

export function buildMtrRows(
  agents: Agent[] | null | undefined,
  agentID: string | undefined,
  events: JobEvent[]
): MtrResultRow[] {
  const agent = safeAgents(agents).find((item) => item.id === agentID);
  const historicalSamples = mtrHistoricalRTTSamples(events);
  return extractHops(events).map((hop) => {
    const samples = hopSamples(hop);
    const live = summarizeSamples(samples);
    const sent = hop.sent ?? hop.probes?.length ?? samples.length;
    const received = samples.length;
    const lossPct = hop.loss_pct ?? lossPercent(sent, received);
    const rttSamples = completeTimeoutSamples(historicalSamples.get(hop.index) ?? hopRTTSamples(hop), sent, lossPct);
    return {
      hop: hop.index,
      country: normalizeCountry(agent?.country),
      region: agent?.region || "-",
      provider: agent?.provider || agent?.name || agentID || "-",
      isp: agent?.isp,
      protocols: singleProtocolMask(agent?.protocols),
      status: lossPct !== undefined ? (lossPct >= 100 ? "timeout" : "running") : sent > 0 && received === 0 ? "timeout" : "running",
      ip: hop.ip || hop.host || "*",
      sent,
      lastMS: hop.last_ms ?? live.lastMS,
      avgMS: hop.avg_ms ?? live.avgMS,
      bestMS: hop.best_ms ?? live.bestMS,
      worstMS: hop.worst_ms ?? live.worstMS,
      stdevMS: hop.stdev_ms ?? live.stdevMS,
      lossPct,
      rttSamples
    };
  });
}

function singleProtocolMask(protocols: number | undefined): number | undefined {
  return protocols === 1 || protocols === 2 ? protocols : undefined;
}

export function buildPingRows(
  agents: Agent[] | null | undefined,
  jobs: Job[],
  eventsByJobId: Record<string, JobEvent[]>
): NodeResultRow[] {
  return buildNodeRows("ping", agents, jobs, eventsByJobId);
}

function safeAgents(agents: Agent[] | null | undefined): Agent[] {
  return Array.isArray(agents) ? agents : [];
}

function compareNodeRows(left: NodeResultRow, right: NodeResultRow): number {
  return (
    compareSortText(left.country, right.country) ||
    compareSortText(left.region, right.region) ||
    compareSortText(left.provider, right.provider) ||
    compareSortText(left.isp, right.isp)
  );
}

function compareSortText(left: string | undefined, right: string | undefined): number {
  return sortText(left).localeCompare(sortText(right), undefined, { numeric: true, sensitivity: "base" });
}

function sortText(value: string | undefined): string {
  const normalized = value?.trim();
  return normalized && normalized !== "-" ? normalized : "\uffff";
}

function sortDNSRecordLines(records: string[]): string[] {
  return [...records].sort((left, right) => left.localeCompare(right, undefined, { numeric: true, sensitivity: "base" }));
}

function normalizeCountry(country: string | undefined): string | undefined {
  const code = country?.trim().toUpperCase();
  return code && /^[A-Z]{2}$/.test(code) ? code : undefined;
}

function metricSamples(events: JobEvent[]): Array<{ latency?: number; seq?: number; timeout?: boolean }> {
  return events.flatMap((event) => {
    const metric = getMetric(event);
    const latency = metric?.latency_ms;
    const seq = firstNumber(metric?.seq);
    const timeout = metric?.timeout === true || metric?.status === "timeout";
    if (typeof latency !== "number" && seq === undefined && !timeout) {
      return [];
    }
    return [{ latency: typeof latency === "number" ? latency : undefined, seq, timeout }];
  });
}

function latestMetric(events: JobEvent[]): Record<string, unknown> | undefined {
  for (let index = events.length - 1; index >= 0; index -= 1) {
    const metric = getMetric(events[index]);
    if (metric) {
      return metric;
    }
  }
  return undefined;
}

function targetResolvedIP(events: JobEvent[]): string | undefined {
  for (let index = events.length - 1; index >= 0; index -= 1) {
    const event = events[index].event;
    if (event?.type !== "target_resolved") {
      continue;
    }
    const targetIP = event.metric?.target_ip;
    if (typeof targetIP === "string" && targetIP.trim()) {
      return targetIP;
    }
  }
  return undefined;
}

function hopSamples(hop: HopResult): number[] {
  if (hop.probes?.length) {
    return hop.probes.flatMap((probe) => (probe.rtt_ms === undefined || probe.timeout ? [] : [probe.rtt_ms]));
  }
  if (hop.rtt_ms !== undefined && !hop.timeout) {
    return [hop.rtt_ms];
  }
  return hop.times_ms ?? [];
}

function hopRTTSamples(hop: HopResult): Array<number | null> {
  if (hop.probes?.length) {
    return hop.probes.map((probe) => (probe.rtt_ms === undefined || probe.timeout ? null : probe.rtt_ms));
  }
  if (hop.rtt_ms !== undefined && !hop.timeout) {
    return [hop.rtt_ms];
  }
  if (hop.timeout) {
    return [null];
  }
  return hop.times_ms ?? [];
}

function completeTimeoutSamples(samples: Array<number | null>, sent: number | undefined, lossPct: number | undefined): Array<number | null> {
  if (samples.length > 0 || lossPct === undefined || lossPct < 100 || !sent || sent <= 0) {
    return samples;
  }
  return Array.from({ length: sent }, () => null);
}

function mtrHistoricalRTTSamples(events: JobEvent[]): Map<number, Array<number | null>> {
  const samplesByHop = new Map<number, Array<number | null>>();
  for (const event of events) {
    const eventType = event.event?.type || event.stream;
    const hop = getHop(event);
    if (hop && eventType !== "hop_summary") {
      appendHopRTTSamples(samplesByHop, hop);
    }

    const result = toolResultFromEvent(event);
    if (result?.type !== "summary") {
      for (const resultHop of result?.hops ?? []) {
        appendHopRTTSamples(samplesByHop, resultHop);
      }
    }
  }
  return samplesByHop;
}

function appendHopRTTSamples(samplesByHop: Map<number, Array<number | null>>, hop: HopResult): void {
  const samples = hopRTTSamples(hop);
  if (samples.length === 0) {
    return;
  }
  samplesByHop.set(hop.index, [...(samplesByHop.get(hop.index) ?? []), ...samples]);
}

function summarizeSamples(samples: number[]) {
  if (samples.length === 0) {
    return {};
  }
  const bestMS = Math.min(...samples);
  const worstMS = Math.max(...samples);
  const avgMS = samples.reduce((sum, sample) => sum + sample, 0) / samples.length;
  const variance =
    samples.reduce((sum, sample) => sum + Math.pow(sample - avgMS, 2), 0) / samples.length;
  return {
    lastMS: samples[samples.length - 1],
    bestMS,
    avgMS,
    worstMS,
    stdevMS: Math.sqrt(variance)
  };
}

function firstNumber(value: unknown): number | undefined {
  return typeof value === "number" ? value : undefined;
}

function stringSummary(value: unknown): string | undefined {
  if (typeof value === "number") {
    return String(value);
  }
  return typeof value === "string" ? value : undefined;
}

function remoteAddressIP(value: unknown): string | undefined {
  const address = stringSummary(value);
  if (!address) {
    return undefined;
  }
  if (address.startsWith("[")) {
    const end = address.indexOf("]");
    return end > 0 ? address.slice(1, end) : address;
  }
  const hostPort = address.match(/^([^:]+):\d+$/);
  if (hostPort) {
    return hostPort[1];
  }
  return address;
}

function maxSeq(metrics: Array<{ seq?: number }>): number | undefined {
  const seqs = metrics.flatMap((metric) => (metric.seq === undefined ? [] : [metric.seq]));
  return seqs.length > 0 ? Math.max(...seqs) : undefined;
}

function lossPercent(sent: number | undefined, received: number): number | undefined {
  if (!sent || sent <= 0) {
    return undefined;
  }
  return ((sent - received) / sent) * 100;
}

function statusFromParsed(exitCode: number, status: unknown): string {
  if (typeof status === "string" && status) {
    return status;
  }
  return exitCode === 0 ? "succeeded" : "failed";
}
