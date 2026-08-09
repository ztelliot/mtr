import type { Agent, AgentConfig, CreateJobRequest, CreateScheduledJobRequest, GeoIPInfo, OutboundAgentConfig, Job, JobEvent, ManagedAgent, ManagedAgentLabelsRequest, ManagedLabels, ManagedLabelsAndAgents, ManagedRateLimit, ManagedRegisterTokenListResponse, ManagedRegisterTokenRequest, ManagedRegisterTokenResponse, ManagedTokenListResponse, ManagedTokenRequest, ManagedTokenResponse, Permissions, RuntimeConfig, ScheduledJob, StreamEvent, VersionInfo } from "./types";

type EventPayload = StreamEvent & { agent_id?: string };

export interface JobStreamMessage {
  data: string;
  type: string;
  lastEventId: string;
}

export interface JobStream {
  readonly closed: boolean;
  close(): void;
}

const maxSSEBufferBytes = 4 * 1024 * 1024;
const initialJobStreamRetryMS = 1000;
const maxJobStreamRetryMS = 8000;
const stableJobStreamMS = 15_000;

class RetryableJobStreamError extends Error {}

function requireArray<T>(value: unknown, responseName: string): T[] {
  if (!Array.isArray(value)) {
    throw new Error(`${responseName} response must be an array`);
  }
  return value as T[];
}

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
    return requireArray<Agent>(await this.request<unknown>("/v1/agents"), "agents");
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
    const events = requireArray<EventPayload>(
      await this.request<unknown>(`/v1/jobs/${encodeURIComponent(id)}/events`),
      "job events"
    );
    return events.map((event, index) => jobEventFromPayload(id, event, index));
  }

  async listSchedules(): Promise<ScheduledJob[]> {
    return requireArray<ScheduledJob>(await this.request<unknown>("/v1/schedules"), "schedules");
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
    return requireArray<Job>(
      await this.request<unknown>(`/v1/schedules/${encodeURIComponent(id)}/history${query ? `?${query}` : ""}`),
      "schedule history"
    );
  }

  async listScheduleHistorySummary(id: string, range?: string): Promise<JobEvent[]> {
    const params = new URLSearchParams();
    if (range && range !== "all") {
      params.set("range", range);
    }
    const query = params.toString();
    return requireArray<JobEvent>(
      await this.request<unknown>(`/v1/schedules/${encodeURIComponent(id)}/summary${query ? `?${query}` : ""}`),
      "schedule summary"
    );
  }

  async getGeoIP(ip: string): Promise<GeoIPInfo> {
    return this.request<GeoIPInfo>(`/v1/geoip/${encodeURIComponent(ip)}`);
  }

  async getManagedRateLimit(): Promise<ManagedRateLimit> {
    return this.request<ManagedRateLimit>("/v1/manage/rate-limit");
  }

  async updateManagedRateLimit(payload: ManagedRateLimit): Promise<ManagedRateLimit> {
    return this.request<ManagedRateLimit>("/v1/manage/rate-limit", {
      method: "PUT",
      body: JSON.stringify(payload)
    });
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
    return this.request<ManagedLabelsAndAgents>("/v1/manage/labels/state", {
      method: "PUT",
      body: JSON.stringify(payload)
    });
  }

  async listManagedTokens(): Promise<ManagedTokenListResponse> {
    return this.request<ManagedTokenListResponse>("/v1/manage/tokens");
  }

  async createManagedToken(payload: ManagedTokenRequest): Promise<ManagedTokenResponse> {
    return this.request<ManagedTokenResponse>("/v1/manage/tokens", {
      method: "POST",
      body: JSON.stringify(payload)
    });
  }

  async updateManagedToken(id: string, payload: ManagedTokenRequest): Promise<ManagedTokenResponse> {
    return this.request<ManagedTokenResponse>(`/v1/manage/tokens/${encodeURIComponent(id)}`, {
      method: "PATCH",
      body: JSON.stringify(payload)
    });
  }

  async rotateManagedToken(id: string): Promise<ManagedTokenResponse> {
    return this.request<ManagedTokenResponse>(`/v1/manage/tokens/${encodeURIComponent(id)}/rotate`, {
      method: "POST"
    });
  }

  async deleteManagedToken(id: string): Promise<ManagedTokenListResponse> {
    return this.request<ManagedTokenListResponse>(`/v1/manage/tokens/${encodeURIComponent(id)}`, {
      method: "DELETE"
    });
  }

  async listManagedRegisterTokens(): Promise<ManagedRegisterTokenListResponse> {
    return this.request<ManagedRegisterTokenListResponse>("/v1/manage/register-tokens");
  }

  async createManagedRegisterToken(payload: ManagedRegisterTokenRequest): Promise<ManagedRegisterTokenResponse> {
    return this.request<ManagedRegisterTokenResponse>("/v1/manage/register-tokens", {
      method: "POST",
      body: JSON.stringify(payload)
    });
  }

  async updateManagedRegisterToken(id: string, payload: ManagedRegisterTokenRequest): Promise<ManagedRegisterTokenResponse> {
    return this.request<ManagedRegisterTokenResponse>(`/v1/manage/register-tokens/${encodeURIComponent(id)}`, {
      method: "PATCH",
      body: JSON.stringify(payload)
    });
  }

  async rotateManagedRegisterToken(id: string): Promise<ManagedRegisterTokenResponse> {
    return this.request<ManagedRegisterTokenResponse>(`/v1/manage/register-tokens/${encodeURIComponent(id)}/rotate`, {
      method: "POST"
    });
  }

  async deleteManagedRegisterToken(id: string): Promise<ManagedRegisterTokenListResponse> {
    return this.request<ManagedRegisterTokenListResponse>(`/v1/manage/register-tokens/${encodeURIComponent(id)}`, {
      method: "DELETE"
    });
  }

  async createOutboundAgent(payload: OutboundAgentConfig): Promise<OutboundAgentConfig> {
    return this.request<OutboundAgentConfig>("/v1/manage/agents", {
      method: "POST",
      body: JSON.stringify(payload)
    });
  }

  async updateOutboundAgent(id: string, payload: OutboundAgentConfig): Promise<OutboundAgentConfig> {
    return this.request<OutboundAgentConfig>(`/v1/manage/agents/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify(payload)
    });
  }

  async listManagedAgents(): Promise<ManagedAgent[]> {
    return requireArray<ManagedAgent>(await this.request<unknown>("/v1/manage/agents"), "managed agents");
  }

  async updateAgentConfig(id: string, payload: AgentConfig): Promise<AgentConfig> {
    return this.request<AgentConfig>(`/v1/manage/agents/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify({ ...payload, transport: "grpc" })
    });
  }

  async updateManagedAgentLabels(payload: ManagedAgentLabelsRequest): Promise<ManagedAgent[]> {
    const agents = await this.request<unknown>("/v1/manage/agents/labels", {
      method: "PUT",
      body: JSON.stringify(payload)
    });
    return requireArray<ManagedAgent>(agents, "managed agents");
  }

  async deleteManagedAgent(id: string): Promise<void> {
    return this.request<void>(`/v1/manage/agents/${encodeURIComponent(id)}`, {
      method: "DELETE"
    });
  }

  openJobStream(
    id: string,
    onMessage: (message: JobStreamMessage) => void,
    onError: (error: unknown) => void,
    onRetry?: (error: unknown) => void
  ): JobStream {
    const controller = new AbortController();
    let closed = false;
    let reader: ReadableStreamDefaultReader<Uint8Array> | null = null;
    const stream: JobStream = {
      get closed() {
        return closed;
      },
      close() {
        if (closed) {
          return;
        }
        closed = true;
        controller.abort();
        if (reader) {
          void reader.cancel().catch(() => undefined);
        }
      }
    };

    void (async () => {
      let retryMS = initialJobStreamRetryMS;
      let retryNotified = false;
      while (!closed) {
        try {
          const response = await fetch(`${this.baseUrl}/v1/jobs/${encodeURIComponent(id)}/stream`, {
            cache: "no-store",
            credentials: "omit",
            headers: {
              Accept: "text/event-stream",
              ...this.authHeaders()
            },
            redirect: "error",
            signal: controller.signal
          });
          if (!response.ok) {
            const error = new ApiError(response.status, await responseErrorMessage(response));
            if (response.status >= 500) {
              throw new RetryableJobStreamError(error.message);
            }
            throw error;
          }
          if (!response.body) {
            throw new Error("Streaming response body is unavailable");
          }
          const contentType = response.headers.get("Content-Type")?.toLowerCase() ?? "";
          if (!contentType.startsWith("text/event-stream")) {
            throw new Error("Streaming response has an invalid content type");
          }
          const connectedAt = Date.now();
          reader = response.body.getReader();
          await consumeSSE(reader, (message) => {
            retryNotified = false;
            onMessage(message);
          }, controller.signal);
          if (Date.now() - connectedAt >= stableJobStreamMS) {
            retryMS = initialJobStreamRetryMS;
          }
          if (!closed) {
            throw new RetryableJobStreamError("Job event stream ended unexpectedly");
          }
        } catch (error) {
          if (closed || isAbortError(error)) {
            return;
          }
          if (!isRetryableJobStreamError(error)) {
            closed = true;
            onError(error);
            return;
          }
          if (!retryNotified) {
            retryNotified = true;
            onRetry?.(error);
          }
          await waitForJobStreamRetry(controller.signal, retryMS);
          retryMS = Math.min(retryMS * 2, maxJobStreamRetryMS);
        } finally {
          reader?.releaseLock();
          reader = null;
        }
      }
      closed = true;
    })();
    return stream;
  }

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const response = await fetch(`${this.baseUrl}${path}`, {
      ...init,
      cache: "no-store",
      credentials: "omit",
      headers: {
        "Content-Type": "application/json",
        ...this.authHeaders(),
        ...init.headers
      },
      redirect: "error"
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

async function responseErrorMessage(response: Response): Promise<string> {
  let message = response.statusText || "Request failed";
  try {
    const body = (await response.json()) as { error?: unknown };
    if (typeof body.error === "string" && body.error) {
      message = body.error;
    }
  } catch {
    // Keep the HTTP status text when the body is not JSON.
  }
  return message;
}

async function consumeSSE(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  onMessage: (message: JobStreamMessage) => void,
  signal: AbortSignal
): Promise<void> {
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    if (signal.aborted) {
      return;
    }
    const { done, value } = await reader.read();
    buffer += decoder.decode(value, { stream: !done });
    if (new TextEncoder().encode(buffer).byteLength > maxSSEBufferBytes) {
      throw new Error("Job event stream exceeded its buffer limit");
    }
    let boundary = sseBoundary(buffer);
    while (boundary) {
      const block = buffer.slice(0, boundary.index);
      buffer = buffer.slice(boundary.index + boundary.length);
      emitSSEBlock(block, onMessage);
      boundary = sseBoundary(buffer);
    }
    if (done) {
      return;
    }
  }
}

function sseBoundary(buffer: string): { index: number; length: number } | null {
  const match = /\r?\n\r?\n/.exec(buffer);
  return match ? { index: match.index, length: match[0].length } : null;
}

function emitSSEBlock(block: string, onMessage: (message: JobStreamMessage) => void): void {
  let type = "message";
  let lastEventId = "";
  const data: string[] = [];
  for (const line of block.split(/\r?\n/)) {
    if (!line || line.startsWith(":")) {
      continue;
    }
    const separator = line.indexOf(":");
    const field = separator < 0 ? line : line.slice(0, separator);
    let value = separator < 0 ? "" : line.slice(separator + 1);
    if (value.startsWith(" ")) {
      value = value.slice(1);
    }
    if (field === "event") {
      type = value || "message";
    } else if (field === "id" && !value.includes("\0")) {
      lastEventId = value;
    } else if (field === "data") {
      data.push(value);
    }
  }
  if (data.length > 0) {
    onMessage({ data: data.join("\n"), type, lastEventId });
  }
}

function isAbortError(error: unknown): boolean {
  return error instanceof Error && error.name === "AbortError";
}

function isRetryableJobStreamError(error: unknown): boolean {
  return error instanceof RetryableJobStreamError || error instanceof TypeError || (error instanceof Error && error.name === "NetworkError");
}

function waitForJobStreamRetry(signal: AbortSignal, delayMS: number): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve();
      return;
    }
    const finish = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", finish);
      resolve();
    };
    const timer = setTimeout(finish, delayMS);
    signal.addEventListener("abort", finish, { once: true });
  });
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
