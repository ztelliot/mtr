import { describe, expect, it, vi } from "vitest";
import { ApiClient } from "./api";
import { loadConfig } from "./config";

describe("api client", () => {
  it("uses the deployment token loaded from config.json", async () => {
    const configFetcher = async () => new Response(JSON.stringify({ apiBaseUrl: "/api", apiToken: "public-diagnostics-token" }));
    const config = await loadConfig(configFetcher as typeof fetch, null);
    const fetcher = vi.fn(async () => new Response(JSON.stringify({ version: "v1.2.3" }), { status: 200 }));
    vi.stubGlobal("fetch", fetcher);

    await new ApiClient(config).getVersion();

    expect(fetcher).toHaveBeenCalledWith(
      "/api/v1/version",
      expect.objectContaining({
        headers: expect.objectContaining({ Authorization: "Bearer public-diagnostics-token" })
      })
    );
  });

  it("rejects a malformed agent list instead of silently normalizing it", async () => {
    const fetcher = vi.fn(async () => new Response("null", { status: 200 }));
    vi.stubGlobal("fetch", fetcher);

    const client = new ApiClient({ apiBaseUrl: "", apiToken: "token" });
    await expect(client.listAgents()).rejects.toThrow("agents response must be an array");
  });

  it("fetches agents with node-level tool permissions", async () => {
    const fetcher = vi.fn(async () => new Response(JSON.stringify([{
      id: "edge-1",
      tools: { ping: { requires_agent: false } },
      protocols: 3,
      status: "online",
      last_seen_at: "2026-04-25T00:00:00Z",
      created_at: "2026-04-25T00:00:00Z"
    }]), { status: 200 }));
    vi.stubGlobal("fetch", fetcher);

    const client = new ApiClient({ apiBaseUrl: "", apiToken: "token" });
    await expect(client.listAgents()).resolves.toMatchObject([
      { id: "edge-1", tools: { ping: { requires_agent: false } } }
    ]);
  });

  it("fetches geoip data through the API proxy", async () => {
    const fetcher = vi.fn(async () => new Response(JSON.stringify({ asn: 13335, org: "Cloudflare, Inc." }), { status: 200 }));
    vi.stubGlobal("fetch", fetcher);

    const client = new ApiClient({ apiBaseUrl: "/api", apiToken: "token" });
    await expect(client.getGeoIP("2606:4700:4700::1111")).resolves.toMatchObject({
      asn: 13335,
      org: "Cloudflare, Inc."
    });
    expect(fetcher).toHaveBeenCalledWith(
      "/api/v1/geoip/2606%3A4700%3A4700%3A%3A1111",
      expect.objectContaining({
        headers: expect.objectContaining({ Authorization: "Bearer token" })
      })
    );
  });

  it("fetches token permissions", async () => {
    const fetcher = vi.fn(async () => new Response(JSON.stringify({ tools: { ping: { requires_agent: false } }, schedule_access: "read" }), { status: 200 }));
    vi.stubGlobal("fetch", fetcher);

    const client = new ApiClient({ apiBaseUrl: "/api", apiToken: "token" });
    await expect(client.getPermissions()).resolves.toMatchObject({
      tools: { ping: { requires_agent: false } },
      schedule_access: "read"
    });
  });

  it("fetches server version", async () => {
    const fetcher = vi.fn(async () => new Response(JSON.stringify({ version: "v1.2.3", commit: "abc123" }), { status: 200 }));
    vi.stubGlobal("fetch", fetcher);

    const client = new ApiClient({ apiBaseUrl: "/api", apiToken: "token" });
    await expect(client.getVersion()).resolves.toMatchObject({
      version: "v1.2.3",
      commit: "abc123"
    });
    expect(fetcher).toHaveBeenCalledWith(
      "/api/v1/version",
      expect.objectContaining({
        headers: expect.objectContaining({ Authorization: "Bearer token" })
      })
    );
  });

  it("creates schedules and fetches their history", async () => {
    const fetcher = vi.fn(async (url: string, init?: RequestInit) => {
      if (url === "/api/v1/schedules" && init?.method === "POST") {
        return new Response(JSON.stringify({ id: "sched-1", tool: "ping", target: "1.1.1.1", enabled: true, cron_expression: "*/5 * * * *" }), { status: 201 });
      }
      if (url === "/api/v1/schedules/sched-1" && init?.method === "PUT") {
        return new Response(JSON.stringify({ id: "sched-1", tool: "ping", target: "8.8.8.8", enabled: false, cron_expression: "0 * * * *" }), { status: 200 });
      }
      if (url === "/api/v1/schedules/sched-1" && init?.method === "DELETE") {
        return new Response(null, { status: 204 });
      }
      if (url === "/api/v1/jobs/job-1/events") {
        return new Response(JSON.stringify([{ type: "summary", agent_id: "edge-1", metric: { rtt_avg_ms: 4.5 } }]), { status: 200 });
      }
      if (url === "/api/v1/schedules/sched-1/history?range=24h") {
        return new Response(JSON.stringify([{ id: "job-1", scheduled_id: "sched-1", tool: "ping", target: "1.1.1.1", status: "succeeded" }]), { status: 200 });
      }
      if (url === "/api/v1/schedules/sched-1/summary?range=24h") {
        return new Response(JSON.stringify([{ id: "event-summary", job_id: "job-1", event: { type: "summary" }, created_at: "2026-04-28T00:00:00Z" }]), { status: 200 });
      }
      return new Response("[]", { status: 200 });
    });
    vi.stubGlobal("fetch", fetcher);

    const client = new ApiClient({ apiBaseUrl: "/api", apiToken: "token" });
    await expect(client.createSchedule({ tool: "ping", target: "1.1.1.1", cron_expression: "*/5 * * * *", schedule_targets: [{ label: "agent" }] })).resolves.toMatchObject({ id: "sched-1" });
    await expect(client.updateSchedule("sched-1", { tool: "ping", target: "8.8.8.8", enabled: false, cron_expression: "0 * * * *", schedule_targets: [{ label: "agent" }] })).resolves.toMatchObject({ target: "8.8.8.8", enabled: false });
    await expect(client.deleteSchedule("sched-1")).resolves.toBeUndefined();
    await expect(client.listJobEvents("job-1")).resolves.toMatchObject([{ job_id: "job-1", agent_id: "edge-1", event: { type: "summary" } }]);
    await expect(client.listScheduleHistory("sched-1", "24h")).resolves.toMatchObject([{ id: "job-1" }]);
    await expect(client.listScheduleHistorySummary("sched-1", "24h")).resolves.toMatchObject([{ id: "event-summary", job_id: "job-1" }]);
  });

  it("streams events with a bearer header and no query token", async () => {
    let requestedURL = "";
    let authorization = "";
    const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requestedURL = String(input);
      authorization = new Headers(init?.headers).get("Authorization") ?? "";
      const encoded = new TextEncoder().encode('id: event-1\nevent: metric\ndata: {"type":"metric"}\n\n');
      return new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(encoded);
        }
      }), { status: 200, headers: { "Content-Type": "text/event-stream" } });
    });
    vi.stubGlobal("fetch", fetcher);
    const client = new ApiClient({ apiBaseUrl: "/api", apiToken: "token value" });
    let stream: ReturnType<ApiClient["openJobStream"]>;
    const message = await new Promise<{ data: string; type: string; lastEventId: string }>((resolve, reject) => {
      stream = client.openJobStream("job/1", (event) => {
        resolve(event);
        stream.close();
      }, reject);
    });

    expect(requestedURL).toBe("/api/v1/jobs/job%2F1/stream");
    expect(authorization).toBe("Bearer token value");
    expect(message).toEqual({ data: '{"type":"metric"}', type: "metric", lastEventId: "event-1" });
  });

  it("reconnects an unexpectedly closed authenticated stream", async () => {
    vi.useFakeTimers();
    try {
      let calls = 0;
      const fetcher = vi.fn(async () => {
        calls += 1;
        if (calls <= 3) {
          return new Response("", { status: 200, headers: { "Content-Type": "text/event-stream" } });
        }
        const encoded = new TextEncoder().encode('event: summary\ndata: {"type":"summary"}\n\n');
        return new Response(new ReadableStream({
          start(controller) {
            controller.enqueue(encoded);
          }
        }), { status: 200, headers: { "Content-Type": "text/event-stream" } });
      });
      vi.stubGlobal("fetch", fetcher);
      const client = new ApiClient({ apiBaseUrl: "/api", apiToken: "token" });
      let stream: ReturnType<ApiClient["openJobStream"]>;
      const messagePromise = new Promise<{ data: string; type: string }>((resolve, reject) => {
        stream = client.openJobStream("job-1", (event) => {
          resolve(event);
          stream.close();
        }, reject);
      });

      await vi.advanceTimersByTimeAsync(999);
      expect(fetcher).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(1);
      expect(fetcher).toHaveBeenCalledTimes(2);
      await vi.advanceTimersByTimeAsync(1999);
      expect(fetcher).toHaveBeenCalledTimes(2);
      await vi.advanceTimersByTimeAsync(1);
      expect(fetcher).toHaveBeenCalledTimes(3);
      await vi.advanceTimersByTimeAsync(3999);
      expect(fetcher).toHaveBeenCalledTimes(3);
      await vi.advanceTimersByTimeAsync(1);
      await expect(messagePromise).resolves.toMatchObject({ data: '{"type":"summary"}', type: "summary" });
      expect(fetcher).toHaveBeenCalledTimes(4);
    } finally {
      vi.useRealTimers();
    }
  });

  it("starts status fallback once while retryable stream failures continue reconnecting", async () => {
    vi.useFakeTimers();
    try {
      const fetcher = vi.fn(async () => new Response("unavailable", { status: 503 }));
      vi.stubGlobal("fetch", fetcher);
      const client = new ApiClient({ apiBaseUrl: "/api", apiToken: "token" });
      const onError = vi.fn();
      const onRetry = vi.fn();
      const stream = client.openJobStream("job-1", vi.fn(), onError, onRetry);

      await vi.advanceTimersByTimeAsync(0);
      expect(onRetry).toHaveBeenCalledTimes(1);
      expect(onError).not.toHaveBeenCalled();
      expect(stream.closed).toBe(false);

      await vi.advanceTimersByTimeAsync(1_000);
      await vi.advanceTimersByTimeAsync(2_000);
      expect(fetcher).toHaveBeenCalledTimes(3);
      expect(onRetry).toHaveBeenCalledTimes(1);
      expect(onError).not.toHaveBeenCalled();
      stream.close();
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps exponential backoff when a short connection only replays history", async () => {
    vi.useFakeTimers();
    try {
      let calls = 0;
      const history = new TextEncoder().encode('id: event-1\nevent: metric\ndata: {"type":"metric"}\n\n');
      const summary = new TextEncoder().encode('id: event-2\nevent: summary\ndata: {"type":"summary"}\n\n');
      const fetcher = vi.fn(async () => {
        calls += 1;
        const encoded = calls <= 3 ? history : summary;
        return new Response(new ReadableStream({
          start(controller) {
            controller.enqueue(encoded);
            controller.close();
          }
        }), { status: 200, headers: { "Content-Type": "text/event-stream" } });
      });
      vi.stubGlobal("fetch", fetcher);
      const client = new ApiClient({ apiBaseUrl: "/api", apiToken: "token" });
      let stream: ReturnType<ApiClient["openJobStream"]>;
      const summaryPromise = new Promise<{ type: string }>((resolve, reject) => {
        stream = client.openJobStream("job-1", (event) => {
          if (event.type === "summary") {
            resolve(event);
            stream.close();
          }
        }, reject);
      });

      await vi.advanceTimersByTimeAsync(1000);
      expect(fetcher).toHaveBeenCalledTimes(2);
      await vi.advanceTimersByTimeAsync(2000);
      expect(fetcher).toHaveBeenCalledTimes(3);
      await vi.advanceTimersByTimeAsync(4000);
      await expect(summaryPromise).resolves.toMatchObject({ type: "summary" });
      expect(fetcher).toHaveBeenCalledTimes(4);
    } finally {
      vi.useRealTimers();
    }
  });
});
