import type { Agent, CreateJobRequest, CreateScheduledJobRequest, GeoIPInfo, Job, JobEvent, Permissions, RuntimeConfig, ScheduledJob, StreamEvent, VersionInfo } from "./types";

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
