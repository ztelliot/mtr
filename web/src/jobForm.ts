import type { CreateJobRequest, Job, JobFormState, Tool } from "./types";

export const tools: Tool[] = ["ping", "traceroute", "mtr", "http", "dns", "port"];
export const navTools: Tool[] = ["ping", "dns", "port", "http", "traceroute", "mtr"];

export const defaultFormState: JobFormState = {
  tool: "ping",
  target: "",
  ipVersion: 0,
  agentId: "",
  count: "4",
  protocol: "icmp",
  port: "443",
  method: "HEAD",
  dnsType: "A",
  resolveOnAgent: true
};

const allTools = new Set<Tool>(tools);

export function requiresAgent(tool: Tool): boolean {
  return tool === "mtr" || tool === "traceroute";
}

export function formStateFromLocation(
  location: Pick<Location, "pathname" | "search">,
  base: JobFormState = defaultFormState
): JobFormState {
  const params = new URLSearchParams(location.search);
  const segments = location.pathname
    .split("/")
    .filter(Boolean)
    .map((segment) => decodeURIComponent(segment));
  const toolIndex = segments.findIndex((segment) => allTools.has(segment as Tool));
  const pathTool = toolIndex >= 0 ? (segments[toolIndex] as Tool) : undefined;
  const queryTool = normalizeTool(params.get("tool"));
  const tool = pathTool ?? queryTool ?? base.tool;
  const pathTarget = toolIndex >= 0 ? segments.slice(toolIndex + 1).join("/") : "";
  let target = params.get("target") ?? params.get("host") ?? pathTarget ?? base.target;
  const queryPort = params.get("port");
  if (tool === "port" && queryPort && !parseHostPort(target)) {
    target = `${formatHostForPort(target)}:${queryPort}`;
  }
  return {
    ...base,
    tool,
    target: normalizeTargetForTool(target, tool),
    ipVersion: parseIPVersion(params.get("ip_version") ?? params.get("ipVersion")) ?? base.ipVersion,
    agentId: params.get("agent_id") ?? params.get("agentId") ?? base.agentId,
    protocol: parseProtocol(params.get("protocol")) ?? base.protocol,
    dnsType: parseDNSType(params.get("type") ?? params.get("dns_type") ?? params.get("dnsType")) ?? base.dnsType,
    method: parseHTTPMethod(params.get("method")) ?? base.method,
    resolveOnAgent: parseBoolean(params.get("resolve_on_agent") ?? params.get("remote_dns") ?? params.get("remoteDns")) ?? base.resolveOnAgent
  };
}

export function locationHasExplicitTool(location: Pick<Location, "pathname" | "search">): boolean {
  const params = new URLSearchParams(location.search);
  if (normalizeTool(params.get("tool"))) {
    return true;
  }
  return location.pathname
    .split("/")
    .filter(Boolean)
    .map((segment) => decodeURIComponent(segment))
    .some((segment) => allTools.has(segment as Tool));
}

export function locationHasExplicitTarget(location: Pick<Location, "pathname" | "search">): boolean {
  const params = new URLSearchParams(location.search);
  if (params.has("target") || params.has("host")) {
    return true;
  }
  const segments = location.pathname
    .split("/")
    .filter(Boolean)
    .map((segment) => decodeURIComponent(segment));
  const toolIndex = segments.findIndex((segment) => allTools.has(segment as Tool));
  return toolIndex >= 0 && segments.slice(toolIndex + 1).join("/").trim() !== "";
}

export function formStatePath(state: JobFormState): string {
  const params = new URLSearchParams();
  if (state.target.trim()) {
    params.set("target", state.target.trim());
  }
  if (state.ipVersion !== 0 && state.tool !== "dns") {
    params.set("ip_version", String(state.ipVersion));
  }
  if (state.agentId) {
    params.set("agent_id", state.agentId);
  }
  if (state.tool === "dns") {
    params.set("type", state.dnsType);
  }
  if (state.tool === "http") {
    params.set("method", state.method);
  }
  if (state.tool === "ping" || state.tool === "mtr" || state.tool === "traceroute") {
    params.set("protocol", state.protocol);
  }
  if (state.tool !== "dns") {
    params.set("resolve_on_agent", state.resolveOnAgent ? "1" : "0");
  }
  return `/${state.tool}?${params.toString()}`;
}

export function jobResultPath(state: JobFormState, jobID: string): string {
  const [path, query = ""] = formStatePath(state).split("?");
  const params = new URLSearchParams(query);
  params.set("job_id", jobID);
  const nextQuery = params.toString();
  return nextQuery ? `${path}?${nextQuery}` : path;
}

export function formStateFromJob(job: Job, base: JobFormState = defaultFormState): JobFormState {
  const args = job.args ?? {};
  const target =
    job.tool === "port" && args.port && !parseHostPort(job.target)
      ? `${formatHostForPort(job.target)}:${args.port}`
      : job.target;
  return {
    ...base,
    tool: job.tool,
    target: normalizeTargetForTool(target, job.tool),
    ipVersion: job.ip_version ?? base.ipVersion,
    agentId: job.agent_id ?? "",
    protocol: parseProtocol(args.protocol) ?? base.protocol,
    dnsType: parseDNSType(args.type) ?? base.dnsType,
    method: parseHTTPMethod(args.method) ?? base.method,
    resolveOnAgent: job.resolve_on_agent ?? base.resolveOnAgent
  };
}

export function buildCreateJobRequest(state: JobFormState): CreateJobRequest {
  const args: Record<string, string> = {};
  if (state.tool === "ping" || state.tool === "traceroute" || state.tool === "mtr") {
    setIfPresent(args, "protocol", state.protocol);
  }
  if (state.tool === "http") {
    setIfPresent(args, "method", state.method);
  }
  if (state.tool === "dns") {
    setIfPresent(args, "type", state.dnsType);
  }
  if (state.tool === "port") {
    const parsed = parseHostPort(state.target);
    if (parsed) {
      setIfPresent(args, "port", parsed.port);
    }
  }

  return {
    tool: state.tool,
    target: state.tool === "port" ? parseHostPort(state.target)?.host ?? state.target.trim() : state.target.trim(),
    ...(Object.keys(args).length > 0 ? { args } : {}),
    ...(state.ipVersion === 0 || state.tool === "dns" ? {} : { ip_version: state.ipVersion }),
    ...(state.agentId ? { agent_id: state.agentId } : {}),
    resolve_on_agent: state.resolveOnAgent
  };
}

export function validateForm(state: JobFormState): string | null {
  if (!state.target.trim()) {
    return "Target is required.";
  }
  if (requiresAgent(state.tool) && !state.agentId) {
    return `${state.tool} requires an agent.`;
  }
  if (state.tool === "port" && !parseHostPort(state.target)) {
    return "port requires host:port.";
  }
  return null;
}

export function normalizeTargetForTool(value: string, tool: Tool): string {
  if (!value.trim()) {
    return "";
  }
  const target = targetParts(value);
  if (tool === "http") {
    return /^https?:\/\//i.test(value.trim())
      ? value.trim()
      : `https://${formatHostForUrl(target.host || "example.com")}${target.port ? `:${target.port}` : ""}`;
  }
  if (tool === "port") {
    return `${formatHostForPort(target.host || "1.1.1.1")}:${target.port || "443"}`;
  }
  return formatHostForPlain(target.host) || value.trim();
}

export function parseHostPort(value: string): { host: string; port: string } | null {
  const trimmed = value.trim();
  const bracketMatch = trimmed.match(/^\[([^\]]+)\]:(\d{1,5})$/);
  if (bracketMatch) {
    return validPort(bracketMatch[2]) ? { host: bracketMatch[1], port: bracketMatch[2] } : null;
  }
  const index = trimmed.lastIndexOf(":");
  if (index <= 0 || index === trimmed.length - 1) {
    return null;
  }
  const host = trimmed.slice(0, index).trim();
  const port = trimmed.slice(index + 1).trim();
  if (!host || host.includes(":") || !validPort(port)) {
    return null;
  }
  return { host, port };
}

function targetParts(value: string): { host: string; port?: string } {
  const trimmed = value.trim();
  const url = parseHTTPURL(trimmed);
  if (url) {
    return {
      host: formatHostForPlain(url.hostname),
      port: url.port || undefined
    };
  }

  const withoutPath = stripPath(trimmed);
  const parsed = parseHostPort(withoutPath);
  if (parsed) {
    return parsed;
  }
  return { host: withoutPath };
}

function parseHTTPURL(value: string): URL | null {
  if (!/^https?:\/\//i.test(value)) {
    return null;
  }
  try {
    return new URL(value);
  } catch {
    return null;
  }
}

function stripPath(value: string): string {
  const index = ["/", "?", "#"]
    .map((char) => value.indexOf(char))
    .filter((item) => item >= 0)
    .sort((left, right) => left - right)[0];
  return index === undefined ? value : value.slice(0, index);
}

function formatHostForPlain(value: string): string {
  return value.startsWith("[") && value.endsWith("]") ? value.slice(1, -1) : value;
}

function formatHostForUrl(value: string): string {
  return value.includes(":") && !value.startsWith("[") ? `[${value}]` : value;
}

function formatHostForPort(value: string): string {
  return formatHostForUrl(formatHostForPlain(value));
}

function setIfPresent(args: Record<string, string>, key: string, value: string): void {
  const trimmed = value.trim();
  if (trimmed) {
    args[key] = trimmed;
  }
}

function validPort(value: string): boolean {
  const port = Number(value);
  return Number.isInteger(port) && port > 0 && port <= 65535;
}

function normalizeTool(value: string | null): Tool | undefined {
  return value && allTools.has(value as Tool) ? (value as Tool) : undefined;
}

function parseIPVersion(value: string | null): JobFormState["ipVersion"] | undefined {
  return value === "0" || value === "4" || value === "6" ? (Number(value) as JobFormState["ipVersion"]) : undefined;
}

function parseProtocol(value: string | null): JobFormState["protocol"] | undefined {
  return value === "icmp" || value === "tcp" ? value : undefined;
}

function parseDNSType(value: string | null): JobFormState["dnsType"] | undefined {
  return value === "A" || value === "AAAA" || value === "CNAME" || value === "MX" || value === "TXT" || value === "NS" ? value : undefined;
}

function parseHTTPMethod(value: string | null): JobFormState["method"] | undefined {
  const normalized = value?.trim().toUpperCase();
  return normalized === "HEAD" || normalized === "GET" ? normalized : undefined;
}

function parseBoolean(value: string | null): boolean | undefined {
  if (!value) {
    return undefined;
  }
  const normalized = value.trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(normalized)) {
    return true;
  }
  if (["0", "false", "no", "off"].includes(normalized)) {
    return false;
  }
  return undefined;
}
