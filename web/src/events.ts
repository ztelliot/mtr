import type { HopResult, Job, JobEvent, ProbeResult, ToolResult } from "./types";

export interface DisplayEvent {
  id: string;
  stream: string;
  time: string;
  title: string;
  detail: string;
}

export interface ResultRow {
  label: string;
  value: string;
  meta?: string;
  status?: string;
}

export function getEventType(event: JobEvent): string {
  return event.event?.type || event.parsed?.type || event.stream || "message";
}

export function getMetric(event: JobEvent): Record<string, unknown> | undefined {
  return event.event?.metric;
}

export function getHop(event: JobEvent): HopResult | undefined {
  return event.event?.hop;
}

export function toolResultFromEvent(event: JobEvent, fallback?: Pick<Job, "tool" | "target" | "ip_version">): ToolResult | undefined {
  if (event.parsed) {
    return event.parsed;
  }
  const payload = event.event;
  if (!payload) {
    return undefined;
  }
  const looksLikeResult =
    payload.type === "summary" ||
    payload.exit_code !== undefined ||
    Array.isArray(payload.records) ||
    Array.isArray(payload.hops);
  if (!looksLikeResult) {
    return undefined;
  }
  const summary = payload.type === "summary" ? metricObject(payload.metric) : {};
  return {
    type: payload.type || "summary",
    tool: fallback?.tool,
    target: fallback?.target,
    ip_version: fallback?.ip_version,
    exit_code: numberValue(payload.exit_code) ?? event.exit_code ?? 0,
    summary,
    records: Array.isArray(payload.records) ? payload.records : undefined,
    hops: Array.isArray(payload.hops) ? payload.hops : undefined
  };
}

function metricObject(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return {};
  }
  return value as Record<string, unknown>;
}

export function normalizeDisplayEvent(event: JobEvent): DisplayEvent {
  const title = getEventType(event);
  return {
    id: event.id,
    stream: title,
    time: event.created_at,
    title,
    detail: eventDetail(event)
  };
}

export function eventDetail(event: JobEvent): string {
  if (event.event?.message) {
    return event.event.message;
  }
  if (getHop(event)) {
    return hopLabel(getHop(event)!);
  }
  if (getMetric(event)) {
    return compactJSON(getMetric(event));
  }
  const result = toolResultFromEvent(event);
  if (result) {
    return resultLabel(result);
  }
  if (event.exit_code !== undefined) {
    return `exit code ${event.exit_code}`;
  }
  return "event received";
}

export function extractHops(events: JobEvent[]): HopResult[] {
  const byIndex = new Map<number, HopResult>();
  let authoritativeByIndex: Map<number, HopResult> | null = null;
  let latestHopCount: number | undefined;
  for (const event of events) {
    const result = toolResultFromEvent(event);
    latestHopCount = summaryHopCount(result) ?? latestHopCount;
    if (isSummaryResult(result) && result?.hops?.length) {
      authoritativeByIndex = new Map<number, HopResult>();
      for (const hop of result.hops) {
        authoritativeByIndex.set(hop.index, normalizeAuthoritativeHop(hop));
      }
      continue;
    }
    if (!isSummaryResult(result)) {
      for (const hop of result?.hops ?? []) {
        byIndex.set(hop.index, mergeHop(byIndex.get(hop.index), hop));
      }
    }
    const hop = getHop(event);
    if (hop) {
      byIndex.set(
        hop.index,
        event.event?.type === "hop_summary" ? normalizeHopSummary(hop) : mergeHop(byIndex.get(hop.index), hop)
      );
    }
  }
  const source = authoritativeByIndex ?? byIndex;
  return [...source.values()]
    .filter((hop) => latestHopCount === undefined || hop.index <= latestHopCount)
    .sort((a, b) => a.index - b.index);
}

export function latestParsed(events: JobEvent[], fallback?: Pick<Job, "tool" | "target" | "ip_version">): ToolResult | undefined {
  for (let i = events.length - 1; i >= 0; i -= 1) {
    const result = toolResultFromEvent(events[i], fallback);
    if (result && !isSummaryResult(result)) {
      return result;
    }
  }
  return undefined;
}

export function latestToolResult(events: JobEvent[], fallback?: Pick<Job, "tool" | "target" | "ip_version">): ToolResult | undefined {
  for (let i = events.length - 1; i >= 0; i -= 1) {
    const result = toolResultFromEvent(events[i], fallback);
    if (result) {
      return result;
    }
  }
  return undefined;
}

export function resultRows(result: ToolResult | undefined, hops: HopResult[] = []): ResultRow[] {
  if (!result) {
    return hops.map((hop) => ({
      label: `#${hop.index}`,
      value: hop.host || hop.ip || "*",
      meta: [
        hop.ip && hop.host ? hop.ip : undefined,
        hop.avg_ms !== undefined ? `${hop.avg_ms.toFixed(1)} ms avg` : undefined,
        hop.probes?.length ? `${hop.probes.length} probes` : undefined
      ]
        .filter(Boolean)
        .join(" · "),
      status: hop.loss_pct !== undefined ? `${hop.loss_pct.toFixed(1)}% loss` : undefined
    }));
  }

  if (result.records?.length) {
    return result.records.map((record) => ({
      label: record.type || "DNS",
      value: record.value
    }));
  }

  if (result.hops?.length) {
    return result.hops.map(normalizeHop).map((hop) => ({
      label: `#${hop.index}`,
      value: hop.host || hop.ip || "*",
      meta: [
        hop.ip && hop.host ? hop.ip : undefined,
        hop.probes?.length ? `${hop.probes.length} probes` : undefined
      ]
        .filter(Boolean)
        .join(" · "),
      status: hop.avg_ms !== undefined ? `${hop.avg_ms.toFixed(1)} ms` : undefined
    }));
  }

  if (result.summary) {
    return Object.entries(result.summary).map(([key, value]) => ({
      label: key,
      value: formatValue(value),
      status: key === "status" ? formatValue(value) : undefined
    }));
  }

  return [
    {
      label: result.tool || "result",
      value: result.target || "",
      status: `exit ${result.exit_code}`
    }
  ];
}

function resultLabel(result: ToolResult): string {
  const parts = [result.tool ? `${result.tool} completed` : "completed", `exit ${result.exit_code}`];
  if (result.summary) {
    parts.push(compactJSON(result.summary));
  }
  if (result.records?.length) {
    parts.push(`${result.records.length} DNS records`);
  }
  return parts.join(" · ");
}

function formatValue(value: unknown): string {
  if (value === null || value === undefined) {
    return "-";
  }
  if (typeof value === "object") {
    return compactJSON(value);
  }
  return String(value);
}

function hopLabel(hop: HopResult): string {
  const normalized = normalizeHop(hop);
  const target = normalized.host || normalized.ip || "*";
  const timing = normalized.avg_ms !== undefined ? `${normalized.avg_ms.toFixed(1)} ms avg` : "pending";
  return `hop ${normalized.index}: ${target}, ${timing}`;
}

function compactJSON(value: unknown): string {
  return JSON.stringify(value, null, 0);
}

function isSummaryResult(result: ToolResult | undefined): boolean {
  return result?.type === "summary";
}

function normalizeHop(hop: HopResult): HopResult {
  const cleaned: HopResult = {
    ...hop,
    ip: cleanPeer(hop.ip),
    host: cleanPeer(hop.host)
  };
  const probes = cleaned.probes?.length ? cleaned.probes : probeFromFlatHop(cleaned);
  if (!probes.length) {
    return cleaned;
  }
  const answered = probes.filter((probe) => probe.rtt_ms !== undefined && !probe.timeout);
  const samples = answered.flatMap((probe) => (probe.rtt_ms === undefined ? [] : [probe.rtt_ms]));
  const firstPeer = answered.find((probe) => cleanPeer(probe.ip) || cleanPeer(probe.host));
  const normalized: HopResult = {
    ...cleaned,
    ip: cleaned.ip || cleanPeer(firstPeer?.ip),
    host: cleaned.host || cleanPeer(firstPeer?.host),
    probes,
    sent: cleaned.sent ?? probes.length
  };

  if (normalized.loss_pct === undefined && probes.length > 0) {
    normalized.loss_pct = ((probes.length - answered.length) / probes.length) * 100;
  }
  if (samples.length === 0) {
    return normalized;
  }

  const sum = samples.reduce((total, value) => total + value, 0);
  normalized.avg_ms = hop.avg_ms ?? sum / samples.length;
  normalized.best_ms = hop.best_ms ?? Math.min(...samples);
  normalized.worst_ms = hop.worst_ms ?? Math.max(...samples);
  normalized.last_ms = hop.last_ms ?? samples[samples.length - 1];
  normalized.stdev_ms = hop.stdev_ms ?? standardDeviation(samples);
  return normalized;
}

function normalizeHopSummary(hop: HopResult): HopResult {
  const summary: HopResult = {
    ...hop,
    ip: cleanPeer(hop.ip),
    host: cleanPeer(hop.host)
  };
  delete summary.probes;
  delete summary.rtt_ms;
  delete summary.timeout;
  delete summary.times_ms;
  return summary;
}

function normalizeAuthoritativeHop(hop: HopResult): HopResult {
  return hasAggregateHopFields(hop) ? normalizeHopSummary(hop) : normalizeHop(hop);
}

function hasAggregateHopFields(hop: HopResult): boolean {
  return (
    hop.sent !== undefined ||
    hop.loss_pct !== undefined ||
    hop.avg_ms !== undefined ||
    hop.best_ms !== undefined ||
    hop.worst_ms !== undefined ||
    hop.last_ms !== undefined ||
    hop.stdev_ms !== undefined ||
    hop.times_ms !== undefined
  );
}

function summaryHopCount(result: ToolResult | undefined): number | undefined {
  if (!isSummaryResult(result)) {
    return undefined;
  }
  const value = result?.summary?.hop_count;
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function probeFromFlatHop(hop: HopResult): ProbeResult[] {
  if (hop.rtt_ms === undefined && !hop.timeout) {
    return [];
  }
  const probe: ProbeResult = {};
  if (hop.host) {
    probe.host = hop.host;
  }
  if (hop.ip) {
    probe.ip = hop.ip;
  }
  if (hop.rtt_ms !== undefined) {
    probe.rtt_ms = hop.rtt_ms;
  }
  if (hop.timeout) {
    probe.timeout = true;
  }
  return [probe];
}

function mergeHop(existing: HopResult | undefined, incoming: HopResult): HopResult {
  const next = normalizeHop(incoming);
  if (!existing) {
    return next;
  }
  const probes = [...(existing.probes ?? []), ...(next.probes ?? [])];
  const combined: HopResult = {
    ...existing,
    ...next,
    host: next.host || existing.host,
    ip: next.ip || existing.ip,
    probes: probes.length > 0 ? probes : existing.probes
  };
  if (probes.length > 0) {
    delete combined.sent;
    delete combined.loss_pct;
    delete combined.avg_ms;
    delete combined.best_ms;
    delete combined.worst_ms;
    delete combined.last_ms;
    delete combined.stdev_ms;
    delete combined.rtt_ms;
    delete combined.timeout;
  }
  const merged = normalizeHop(combined);
  return {
    ...merged,
    host: merged.host || existing.host,
    ip: merged.ip || existing.ip
  };
}

function standardDeviation(samples: number[]): number {
  if (samples.length === 0) {
    return 0;
  }
  const avg = samples.reduce((total, value) => total + value, 0) / samples.length;
  const variance = samples.reduce((total, value) => total + Math.pow(value - avg, 2), 0) / samples.length;
  return Math.sqrt(variance);
}

function cleanPeer(value: string | undefined): string | undefined {
  const trimmed = value?.trim();
  if (!trimmed || trimmed === "*" || trimmed.toLowerCase() === "timeout") {
    return undefined;
  }
  return trimmed;
}

function numberValue(value: unknown): number | undefined {
  return typeof value === "number" ? value : undefined;
}
