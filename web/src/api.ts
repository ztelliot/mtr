import type { Agent, AgentConfig, APITokenPermission, CreateJobRequest, CreateScheduledJobRequest, GeoIPInfo, HTTPAgentConfig, Job, JobEvent, ManagedAgent, ManagedAgentLabelsRequest, ManagedLabels, ManagedLabelsAndAgents, ManagedRateLimit, ManagedRegisterTokenListResponse, ManagedRegisterTokenRequest, ManagedRegisterTokenResponse, ManagedTokenListResponse, ManagedTokenRequest, ManagedTokenResponse, Permissions, RateLimitConfig, RegisterToken, RuntimeConfig, ScheduledJob, StreamEvent, VersionInfo } from "./types";

type EventPayload = StreamEvent & { agent_id?: string };

export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

export class ApiClient {
  private readonly baseUrl: string;
  private readonly token: string;

  constructor(config: RuntimeConfig) {
    this.baseUrl = config.apiBaseUrl;
    this.token = config.apiToken;
  }

  async listAgents(): Promise<Agent[]> {
    const agents = await this.request<Agent[] | null>("/v1/agents");
    return Array.isArray(agents) ? agents : [];
  }

  async getPermissions(): Promise<Permissions> {
    return this.request<Permissions>("/v1/permissions");
  }

  async getVersion(): Promise<VersionInfo> {
    return this.request<VersionInfo>("/v1/version");
  }

  async createJob(payload: CreateJobRequest): Promise<Job> {
    return this.request<Job>("/v1/jobs", {
      method: "POST",
      body: JSON.stringify(payload)
    });
  }

  async getJob(id: string): Promise<Job> {
    return this.request<Job>(`/v1/jobs/${encodeURIComponent(id)}`);
  }

  async listJobEvents(id: string): Promise<JobEvent[]> {
    const events = await this.request<EventPayload[] | null>(`/v1/jobs/${encodeURIComponent(id)}/events`);
    return Array.isArray(events) ? events.map((event, index) => jobEventFromPayload(id, event, index)) : [];
  }

  async listSchedules(): Promise<ScheduledJob[]> {
    const schedules = await this.request<ScheduledJob[] | null>("/v1/schedules");
    return Array.isArray(schedules) ? schedules : [];
  }

  async createSchedule(payload: CreateScheduledJobRequest): Promise<ScheduledJob> {
    return this.request<ScheduledJob>("/v1/schedules", {
      method: "POST",
      body: JSON.stringify(payload)
    });
  }

  async updateSchedule(id: string, payload: CreateScheduledJobRequest): Promise<ScheduledJob> {
    return this.request<ScheduledJob>(`/v1/schedules/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify(payload)
    });
  }

  async deleteSchedule(id: string): Promise<void> {
    return this.request<void>(`/v1/schedules/${encodeURIComponent(id)}`, {
      method: "DELETE"
    });
  }

  async listScheduleHistory(id: string, range?: string): Promise<Job[]> {
    const params = new URLSearchParams();
    if (range && range !== "all") {
      params.set("range", range);
    }
    const query = params.toString();
    const history = await this.request<Job[] | null>(`/v1/schedules/${encodeURIComponent(id)}/history${query ? `?${query}` : ""}`);
    return Array.isArray(history) ? history : [];
  }

  async listScheduleHistorySummary(id: string, range?: string): Promise<JobEvent[]> {
    const params = new URLSearchParams();
    if (range && range !== "all") {
      params.set("range", range);
    }
    const query = params.toString();
    const history = await this.request<JobEvent[] | null>(`/v1/schedules/${encodeURIComponent(id)}/summary${query ? `?${query}` : ""}`);
    return Array.isArray(history) ? history : [];
  }

  async getGeoIP(ip: string): Promise<GeoIPInfo> {
    return this.request<GeoIPInfo>(`/v1/geoip/${encodeURIComponent(ip)}`);
  }

  async getManagedRateLimit(): Promise<ManagedRateLimit> {
    const response = await this.request<ManagedRateLimit | RateLimitConfig>("/v1/manage/rate-limit");
    if ("rate_limit" in response) {
      return response;
    }
    return { rate_limit: response };
  }

  async updateManagedRateLimit(payload: ManagedRateLimit): Promise<ManagedRateLimit> {
    const response = await this.request<ManagedRateLimit | RateLimitConfig>("/v1/manage/rate-limit", {
      method: "PUT",
      body: JSON.stringify(payload)
    });
    if ("rate_limit" in response) {
      return response;
    }
    return { rate_limit: response };
  }

  async getManagedLabels(): Promise<ManagedLabels> {
    return this.request<ManagedLabels>("/v1/manage/labels");
  }

  async updateManagedLabels(payload: ManagedLabels): Promise<ManagedLabels> {
    return this.request<ManagedLabels>("/v1/manage/labels", {
      method: "PUT",
      body: JSON.stringify(payload)
    });
  }

  async updateManagedLabelsAndAgents(payload: ManagedLabels & ManagedAgentLabelsRequest): Promise<ManagedLabelsAndAgents> {
    const response = await this.request<ManagedLabelsAndAgents | null>("/v1/manage/labels/state", {
      method: "PUT",
      body: JSON.stringify(payload)
    });
    return { revision: response?.revision, updated_at: response?.updated_at, label_configs: response?.label_configs ?? {}, agents: Array.isArray(response?.agents) ? response.agents : [] };
  }

  async listManagedTokens(): Promise<ManagedTokenListResponse> {
    const response = await this.request<ManagedTokenListResponse | APITokenPermission[] | null>("/v1/manage/tokens");
    if (Array.isArray(response)) {
      return { tokens: response };
    }
    return { revision: response?.revision, updated_at: response?.updated_at, tokens: Array.isArray(response?.tokens) ? response.tokens : [] };
  }

  async createManagedToken(payload: ManagedTokenRequest): Promise<ManagedTokenResponse> {
    const response = await this.request<ManagedTokenResponse | APITokenPermission>("/v1/manage/tokens", {
      method: "POST",
      body: JSON.stringify(payload)
    });
    return tokenResponse(response);
  }

  async updateManagedToken(id: string, payload: ManagedTokenRequest): Promise<ManagedTokenResponse> {
    const response = await this.request<ManagedTokenResponse | APITokenPermission>(`/v1/manage/tokens/${encodeURIComponent(id)}`, {
      method: "PATCH",
      body: JSON.stringify(payload)
    });
    return tokenResponse(response);
  }

  async rotateManagedToken(id: string): Promise<ManagedTokenResponse> {
    const response = await this.request<ManagedTokenResponse | APITokenPermission>(`/v1/manage/tokens/${encodeURIComponent(id)}/rotate`, {
      method: "POST"
    });
    return tokenResponse(response);
  }

  async deleteManagedToken(id: string): Promise<ManagedTokenListResponse> {
    const response = await this.request<ManagedTokenListResponse | void>(`/v1/manage/tokens/${encodeURIComponent(id)}`, {
      method: "DELETE"
    });
    return response ?? { tokens: [] };
  }

  async listManagedRegisterTokens(): Promise<ManagedRegisterTokenListResponse> {
    const response = await this.request<ManagedRegisterTokenListResponse | RegisterToken[] | null>("/v1/manage/register-tokens");
    if (Array.isArray(response)) {
      return { tokens: response };
    }
    return { revision: response?.revision, updated_at: response?.updated_at, tokens: Array.isArray(response?.tokens) ? response.tokens : [] };
  }

  async createManagedRegisterToken(payload: ManagedRegisterTokenRequest): Promise<ManagedRegisterTokenResponse> {
    const response = await this.request<ManagedRegisterTokenResponse | RegisterToken>("/v1/manage/register-tokens", {
      method: "POST",
      body: JSON.stringify(payload)
    });
    return registerTokenResponse(response);
  }

  async updateManagedRegisterToken(id: string, payload: ManagedRegisterTokenRequest): Promise<ManagedRegisterTokenResponse> {
    const response = await this.request<ManagedRegisterTokenResponse | RegisterToken>(`/v1/manage/register-tokens/${encodeURIComponent(id)}`, {
      method: "PATCH",
      body: JSON.stringify(payload)
    });
    return registerTokenResponse(response);
  }

  async rotateManagedRegisterToken(id: string): Promise<ManagedRegisterTokenResponse> {
    const response = await this.request<ManagedRegisterTokenResponse | RegisterToken>(`/v1/manage/register-tokens/${encodeURIComponent(id)}/rotate`, {
      method: "POST"
    });
    return registerTokenResponse(response);
  }

  async deleteManagedRegisterToken(id: string): Promise<ManagedRegisterTokenListResponse> {
    const response = await this.request<ManagedRegisterTokenListResponse | void>(`/v1/manage/register-tokens/${encodeURIComponent(id)}`, {
      method: "DELETE"
    });
    return response ?? { tokens: [] };
  }

  async createHTTPAgent(payload: HTTPAgentConfig): Promise<HTTPAgentConfig> {
    return this.request<HTTPAgentConfig>("/v1/manage/agents", {
      method: "POST",
      body: JSON.stringify({ ...payload, transport: "http" })
    });
  }

  async updateHTTPAgent(id: string, payload: HTTPAgentConfig): Promise<HTTPAgentConfig> {
    return this.request<HTTPAgentConfig>(`/v1/manage/agents/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify({ ...payload, transport: "http" })
    });
  }

  async listManagedAgents(): Promise<ManagedAgent[]> {
    const agents = await this.request<ManagedAgent[] | null>("/v1/manage/agents");
    return Array.isArray(agents) ? agents : [];
  }

  async updateAgentConfig(id: string, payload: AgentConfig): Promise<AgentConfig> {
    return this.request<AgentConfig>(`/v1/manage/agents/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify({ ...payload, transport: "grpc" })
    });
  }

  async updateManagedAgentLabels(payload: ManagedAgentLabelsRequest): Promise<ManagedAgent[]> {
    const agents = await this.request<ManagedAgent[] | null>("/v1/manage/agents/labels", {
      method: "PUT",
      body: JSON.stringify(payload)
    });
    return Array.isArray(agents) ? agents : [];
  }

  async deleteManagedAgent(id: string): Promise<void> {
    return this.request<void>(`/v1/manage/agents/${encodeURIComponent(id)}`, {
      method: "DELETE"
    });
  }

  jobStreamUrl(id: string): string {
    const url = new URL(
      `${this.baseUrl}/v1/jobs/${encodeURIComponent(id)}/stream`,
      window.location.origin
    );
    if (this.token) {
      url.searchParams.set("access_token", this.token);
    }
    return url.toString();
  }

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const response = await fetch(`${this.baseUrl}${path}`, {
      ...init,
      headers: {
        "Content-Type": "application/json",
        ...this.authHeaders(),
        ...init.headers
      }
    });
    if (!response.ok) {
      let message = response.statusText || "Request failed";
      try {
        const body = (await response.json()) as { error?: string };
        message = body.error ?? message;
      } catch {
        // Keep the HTTP status text when the body is not JSON.
      }
      throw new ApiError(response.status, message);
    }
    if (response.status === 204) {
      return undefined as T;
    }
    const text = await response.text();
    return (text ? JSON.parse(text) : undefined) as T;
  }

  private authHeaders(): Record<string, string> {
    if (!this.token) {
      return {};
    }
    return { Authorization: `Bearer ${this.token}` };
  }
}

function jobEventFromPayload(jobID: string, payload: EventPayload, index: number): JobEvent {
  const { agent_id: agentID, ...event } = payload;
  return {
    id: `${jobID}:${index}`,
    job_id: jobID,
    agent_id: typeof agentID === "string" ? agentID : undefined,
    event,
    created_at: ""
  };
}

function tokenResponse(response: ManagedTokenResponse | APITokenPermission): ManagedTokenResponse {
  if ("token" in response) {
    return response;
  }
  return { token: response };
}

function registerTokenResponse(response: ManagedRegisterTokenResponse | RegisterToken): ManagedRegisterTokenResponse {
  if ("revision" in response || "updated_at" in response) {
    return response as ManagedRegisterTokenResponse;
  }
  return { token: response as RegisterToken };
}
