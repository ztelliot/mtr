export type Tool = "ping" | "traceroute" | "mtr" | "http" | "dns" | "port";
export type IPVersion = 0 | 4 | 6;
export type JobStatus = "queued" | "running" | "succeeded" | "failed" | "canceled";
export type AgentStatus = "online" | "offline";

export interface RuntimeConfig {
  apiBaseUrl: string;
  apiToken: string;
  brand?: string;
  brandUrl?: string | null;
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

export interface TokenToolScope {
  allowed_args?: Record<string, string>;
  resolve_on_agent?: boolean;
  ip_versions?: IPVersion[];
}

export interface Permissions {
  tools: Partial<Record<Tool, PermissionTool>>;
  schedule_access: "none" | "read" | "write";
  manage_access?: "none" | "read" | "write";
}

export interface LimitSpec {
  requests_per_minute: number;
  burst: number;
}

export interface CIDRSpec extends LimitSpec {
  ipv4_prefix: number;
  ipv6_prefix: number;
}

export interface RateLimitConfig {
  global: LimitSpec;
  ip: LimitSpec;
  cidr: CIDRSpec;
  geoip: LimitSpec;
  tools?: Record<string, { global: LimitSpec; cidr: LimitSpec; ip: LimitSpec }>;
  exempt_cidrs?: string[];
}

export interface ManagedRateLimit {
  revision?: number;
  updated_at?: string;
  rate_limit: RateLimitConfig;
}

export interface RuntimeSettings {
  count: number;
  max_hops: number;
  probe_step_timeout_sec: number;
  max_tool_timeout_sec: number;
  http_timeout_sec: number;
  dns_timeout_sec: number;
  resolve_timeout_sec: number;
  http_invoke_attempts: number;
  http_max_health_interval_sec: number;
}

export interface SchedulerSettings {
  agent_offline_after_sec: number;
  max_inflight_per_agent: number;
  poll_interval_sec: number;
}

export interface PolicySettings {
  enabled: boolean;
  allowed_args?: Record<string, string>;
}

export interface APITokenPermission {
  id?: string;
  name?: string;
  secret: string;
  rotate?: boolean;
  all?: boolean;
  schedule_access?: "none" | "read" | "write";
  manage_access?: "none" | "read" | "write";
  agents?: string[];
  denied_agents?: string[];
  agent_tags?: string[];
  denied_tags?: string[];
  tools?: Partial<Record<Tool, TokenToolScope>>;
}

export type ManagedTokenRequest = Omit<APITokenPermission, "id" | "secret" | "rotate">;

export interface ManagedTokenListResponse {
  revision?: number;
  updated_at?: string;
  tokens?: APITokenPermission[];
}

export interface ManagedTokenResponse {
  revision?: number;
  updated_at?: string;
  token: APITokenPermission;
}

export interface RegisterToken {
  id?: string;
  name?: string;
  token: string;
  rotate?: boolean;
}

export type ManagedRegisterTokenRequest = Pick<RegisterToken, "name">;

export interface ManagedRegisterTokenListResponse {
  revision?: number;
  updated_at?: string;
  tokens?: RegisterToken[];
}

export interface ManagedRegisterTokenResponse {
  revision?: number;
  updated_at?: string;
  token: RegisterToken;
}

export interface ManagedSettings {
  revision?: number;
  updated_at?: string;
  rate_limit: RateLimitConfig;
  label_configs?: Record<string, LabelConfigSettings>;
  api_tokens?: APITokenPermission[];
  register_tokens?: RegisterToken[];
}

export interface ManagedLabels {
  revision?: number;
  updated_at?: string;
  label_configs?: Record<string, LabelConfigSettings>;
}

export interface ManagedLabelsAndAgents {
  revision?: number;
  updated_at?: string;
  label_configs?: Record<string, LabelConfigSettings>;
  agents: ManagedAgent[];
}

export interface LabelConfigSettings {
  runtime?: RuntimeSettings | null;
  scheduler?: SchedulerSettings | null;
  tool_policies?: Partial<Record<Tool, PolicySettings>>;
}

export interface TLSSettings {
  enabled?: boolean;
  ca_files?: string[];
  cert_file?: string;
  key_file?: string;
}

export interface HTTPAgentConfig {
  id: string;
  transport?: "http";
  enabled: boolean;
  base_url: string;
  http_token?: string;
  labels?: string[];
  tls?: TLSSettings;
  created_at?: string;
  updated_at?: string;
}

export interface AgentConfig {
  id: string;
  transport?: "grpc";
  disabled?: boolean;
  labels?: string[];
  created_at?: string;
  updated_at?: string;
}

export type ManagedAgent = Omit<Agent, "tools"> & {
  tools?: Partial<Record<Tool, PermissionTool>>;
  capabilities?: Tool[];
  type?: "grpc" | "http";
  transport: "grpc" | "http";
  config?: AgentConfig;
  http?: HTTPAgentConfig;
};

export interface ManagedAgentLabelUpdate {
  id: string;
  labels: string[];
}

export interface ManagedAgentLabelsRequest {
  agents: ManagedAgentLabelUpdate[];
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
  resolve_on_agent?: boolean;
  interval_seconds: number;
  next_run_at: string;
  last_run_at?: string;
  schedule_targets?: ScheduleTarget[];
  created_at: string;
  updated_at: string;
}

export interface ScheduleTarget {
  id?: string;
  label: string;
  allowed_agent_ids?: string[];
  interval_seconds: number;
  next_run_at: string;
  last_run_at?: string;
}

export interface CreateScheduledJobRequest {
  name?: string;
  enabled?: boolean;
  tool: Tool;
  target: string;
  args?: Record<string, string>;
  ip_version?: IPVersion;
  resolve_on_agent?: boolean;
  schedule_targets?: ScheduleTargetRequest[];
}

export interface ScheduleTargetRequest {
  label: string;
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
  labels?: string[];
  tools: Partial<Record<Tool, PermissionTool>>;
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
