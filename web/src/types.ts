export type Tool = "ping" | "traceroute" | "mtr" | "http" | "dns" | "port";
export type IPVersion = 0 | 4 | 6;
export type JobStatus = "queued" | "running" | "succeeded" | "failed" | "canceled";
export type AgentStatus = "online" | "offline";

export interface RuntimeConfig {
  apiBaseUrl: string;
  apiToken: string;
}

export interface VersionInfo {
  version: string;
  commit?: string;
  built_at?: string;
  go?: string;
}

export interface PermissionTool {
  allowed_args?: Record<string, string>;
  resolve_on_agent?: boolean;
  ip_versions?: IPVersion[];
  requires_agent: boolean;
}

export interface Permissions {
  tools: Partial<Record<Tool, PermissionTool>>;
  agents: string[];
  schedule_access: "none" | "read" | "write";
}

export interface GeoIPInfo {
  reverse?: string;
  country?: string;
  region?: string;
  city?: string;
  isp?: string;
  asn?: number;
  org?: string;
}

export interface CreateJobRequest {
  tool: Tool;
  target: string;
  args?: Record<string, string>;
  ip_version?: IPVersion;
  agent_id?: string;
  resolve_on_agent?: boolean;
}

export interface Job {
  id: string;
  scheduled_id?: string;
  scheduled_revision?: number;
  tool: Tool;
  target: string;
  resolved_target?: string;
  args?: Record<string, string>;
  ip_version?: IPVersion;
  agent_id?: string;
  resolve_on_agent?: boolean;
  status: JobStatus;
  created_at: string;
  updated_at: string;
  started_at?: string;
  completed_at?: string;
  error_type?: string;
}

export interface ScheduledJob {
  id: string;
  revision: number;
  name?: string;
  enabled: boolean;
  tool: Tool;
  target: string;
  args?: Record<string, string>;
  ip_version?: IPVersion;
  agent_id?: string;
  resolve_on_agent?: boolean;
  interval_seconds: number;
  next_run_at: string;
  last_run_at?: string;
  created_at: string;
  updated_at: string;
}

export interface CreateScheduledJobRequest {
  name?: string;
  enabled?: boolean;
  tool: Tool;
  target: string;
  args?: Record<string, string>;
  ip_version?: IPVersion;
  agent_id?: string;
  resolve_on_agent?: boolean;
  interval_seconds: number;
}

export interface Agent {
  id: string;
  name?: string;
  country?: string;
  region?: string;
  provider?: string;
  isp?: string;
  version?: string;
  capabilities: Tool[];
  protocols: number;
  status: AgentStatus;
  last_seen_at: string;
  created_at: string;
}

export interface JobEvent {
  id: string;
  job_id: string;
  agent_id?: string;
  stream?: string;
  event?: StreamEvent;
  exit_code?: number;
  parsed?: ToolResult;
  created_at: string;
}

export interface StreamEvent {
  type: string;
  message?: string;
  exit_code?: number;
  ip_version?: IPVersion;
  hop?: HopResult;
  metric?: Record<string, unknown>;
  hops?: HopResult[];
  records?: DNSRecord[];
  [key: string]: unknown;
}

export interface ToolResult {
  type?: string;
  tool?: Tool;
  target?: string;
  ip_version?: IPVersion;
  exit_code: number;
  summary?: Record<string, unknown>;
  hops?: HopResult[];
  records?: DNSRecord[];
}

export interface HopResult {
  index: number;
  host?: string;
  ip?: string;
  rtt_ms?: number;
  timeout?: boolean;
  probes?: ProbeResult[];
  loss_pct?: number;
  sent?: number;
  avg_ms?: number;
  best_ms?: number;
  worst_ms?: number;
  last_ms?: number;
  stdev_ms?: number;
  times_ms?: number[];
}

export interface ProbeResult {
  host?: string;
  ip?: string;
  rtt_ms?: number;
  timeout?: boolean;
}

export interface DNSRecord {
  type?: string;
  value: string;
}

export interface JobFormState {
  tool: Tool;
  target: string;
  ipVersion: IPVersion;
  agentId: string;
  count: string;
  protocol: "icmp" | "tcp";
  port: string;
  method: "HEAD" | "GET";
  dnsType: "A" | "AAAA" | "CNAME" | "MX" | "TXT" | "NS";
  resolveOnAgent: boolean;
}
