import { describe, expect, it, vi } from "vitest";
import { ApiClient } from "./api";

describe("api client", () => {
  it("normalizes null agent lists to an empty array", async () => {
    const fetcher = vi.fn(async () => new Response("null", { status: 200 }));
    vi.stubGlobal("fetch", fetcher);

    const client = new ApiClient({ apiBaseUrl: "", apiToken: "token" });
    await expect(client.listAgents()).resolves.toEqual([]);
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
    const fetcher = vi.fn(async () => new Response(JSON.stringify({ tools: { ping: { requires_agent: false } }, agents: ["*"], schedule_access: "read" }), { status: 200 }));
    vi.stubGlobal("fetch", fetcher);

    const client = new ApiClient({ apiBaseUrl: "/api", apiToken: "token" });
    await expect(client.getPermissions()).resolves.toMatchObject({
      tools: { ping: { requires_agent: false } },
      agents: ["*"],
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
        return new Response(JSON.stringify({ id: "sched-1", tool: "ping", target: "1.1.1.1", enabled: true, interval_seconds: 60 }), { status: 201 });
      }
      if (url === "/api/v1/schedules/sched-1" && init?.method === "PUT") {
        return new Response(JSON.stringify({ id: "sched-1", tool: "ping", target: "8.8.8.8", enabled: false, interval_seconds: 120 }), { status: 200 });
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
    await expect(client.createSchedule({ tool: "ping", target: "1.1.1.1", interval_seconds: 60 })).resolves.toMatchObject({ id: "sched-1" });
    await expect(client.updateSchedule("sched-1", { tool: "ping", target: "8.8.8.8", enabled: false, interval_seconds: 120 })).resolves.toMatchObject({ target: "8.8.8.8", enabled: false });
    await expect(client.deleteSchedule("sched-1")).resolves.toBeUndefined();
    await expect(client.listJobEvents("job-1")).resolves.toMatchObject([{ job_id: "job-1", agent_id: "edge-1", event: { type: "summary" } }]);
    await expect(client.listScheduleHistory("sched-1", "24h")).resolves.toMatchObject([{ id: "job-1" }]);
    await expect(client.listScheduleHistorySummary("sched-1", "24h")).resolves.toMatchObject([{ id: "event-summary", job_id: "job-1" }]);
  });

  it("builds event stream URLs with the token in the query string", () => {
    vi.stubGlobal("window", { location: { origin: "https://mtr.test" } });
    const client = new ApiClient({ apiBaseUrl: "/api", apiToken: "token value" });

    expect(client.jobStreamUrl("job/1")).toBe("https://mtr.test/api/v1/jobs/job%2F1/stream?access_token=token+value");
  });
});
