import { describe, expect, it } from "vitest";
import { buildCreateJobRequest, defaultFormState, formStateFromJob, formStateFromLocation, formStatePath, jobResultPath, locationHasExplicitTarget, locationHasExplicitTool, navTools, normalizeTargetForTool, validateForm } from "./jobForm";

describe("job form", () => {
  it("builds ping payload with optional network args", () => {
    expect(
      buildCreateJobRequest({
        ...defaultFormState,
        target: "  1.1.1.1 ",
        protocol: "tcp",
        port: "443",
        ipVersion: 4,
        agentId: ""
      })
    ).toEqual({
      tool: "ping",
      target: "1.1.1.1",
      ip_version: 4,
      resolve_on_agent: true,
      args: {
        protocol: "tcp"
      }
    });
  });

  it("requires a pinned agent for route tools", () => {
    expect(validateForm({ ...defaultFormState, tool: "ping", target: "1.1.1.1", agentId: "" })).toBeNull();
    expect(validateForm({ ...defaultFormState, tool: "traceroute", target: "1.1.1.1", agentId: "" })).toBe("traceroute requires an agent.");
    expect(validateForm({ ...defaultFormState, tool: "traceroute", target: "1.1.1.1", agentId: "edge-1" })).toBeNull();
    expect(validateForm({ ...defaultFormState, tool: "mtr", target: "1.1.1.1", agentId: "" })).toBe("mtr requires an agent.");
    expect(validateForm({ ...defaultFormState, tool: "mtr", target: "1.1.1.1", agentId: "edge-1" })).toBeNull();
  });

  it("does not submit max_hops for route tools", () => {
    expect(
      buildCreateJobRequest({
        ...defaultFormState,
        tool: "traceroute",
        target: "1.1.1.1",
        agentId: "edge-1",
      })
    ).toEqual({
      tool: "traceroute",
      target: "1.1.1.1",
      agent_id: "edge-1",
      resolve_on_agent: true,
      args: { protocol: "icmp" }
    });
  });

  it("builds DNS payload with agent-side resolution and without IP version", () => {
    expect(buildCreateJobRequest({ ...defaultFormState, tool: "dns", target: "1.1.1.1", dnsType: "AAAA", ipVersion: 6 })).toEqual({
      tool: "dns",
      target: "1.1.1.1",
      args: { type: "AAAA" },
      resolve_on_agent: true
    });
  });

  it("can disable agent-side DNS resolution for non-DNS tools", () => {
    expect(
      buildCreateJobRequest({ ...defaultFormState, tool: "http", target: "https://example.com", method: "GET", resolveOnAgent: false })
    ).toEqual({
      tool: "http",
      target: "https://example.com",
      args: { method: "GET" },
      resolve_on_agent: false
    });
  });

  it("builds port payload and exposes port navigation", () => {
    expect(navTools).toEqual(["ping", "dns", "port", "http", "traceroute", "mtr"]);
    expect(
      buildCreateJobRequest({ ...defaultFormState, tool: "port", target: "example.com:443" })
    ).toEqual({
      tool: "port",
      target: "example.com",
      args: { port: "443" },
      resolve_on_agent: true
    });
  });

  it("requires port for port checks", () => {
    expect(validateForm({ ...defaultFormState, tool: "port", target: "example.com" })).toBe(
      "port requires host:port."
    );
  });

  it("normalizes target input when switching tools", () => {
    expect(normalizeTargetForTool("", "http")).toBe("");
    expect(normalizeTargetForTool("example.com", "http")).toBe("https://example.com");
    expect(normalizeTargetForTool("example.com", "port")).toBe("example.com:443");
    expect(normalizeTargetForTool("example.com:8443", "ping")).toBe("example.com");
    expect(normalizeTargetForTool("https://example.com/path?q=1", "dns")).toBe("example.com");
    expect(normalizeTargetForTool("http://example.com:8080/path", "port")).toBe("example.com:8080");
    expect(normalizeTargetForTool("https://example.com", "port")).toBe("example.com:443");
    expect(normalizeTargetForTool("2001:db8::1", "port")).toBe("[2001:db8::1]:443");
  });

  it("builds form state from path and query parameters", () => {
    const url = new URL("https://mtr.test/mtr?target=2606:4700:20::681a:c1f&agent_id=edge-1&ip_version=6&protocol=tcp&resolve_on_agent=0");

    expect(formStateFromLocation(url)).toMatchObject({
      tool: "mtr",
      target: "2606:4700:20::681a:c1f",
      agentId: "edge-1",
      ipVersion: 6,
      protocol: "tcp",
      resolveOnAgent: false
    });
  });

  it("detects explicit tool URLs for readonly shared views", () => {
    expect(locationHasExplicitTool(new URL("https://mtr.test/traceroute?target=ipv6.ip.sb&job_id=job-1"))).toBe(true);
    expect(locationHasExplicitTool(new URL("https://mtr.test/jobs/job-1"))).toBe(false);
    expect(locationHasExplicitTool(new URL("https://mtr.test/?tool=mtr&target=1.1.1.1"))).toBe(true);
  });

  it("detects whether a share URL pins the target", () => {
    expect(locationHasExplicitTarget(new URL("https://mtr.test/traceroute?job_id=job-1"))).toBe(false);
    expect(locationHasExplicitTarget(new URL("https://mtr.test/traceroute?target=ipv6.ip.sb&job_id=job-1"))).toBe(true);
    expect(locationHasExplicitTarget(new URL("https://mtr.test/http/https%3A%2F%2Fexample.com?job_id=job-1"))).toBe(true);
  });

  it("uses the first path segment as the tool and remaining encoded path as target", () => {
    const url = new URL("https://mtr.test/http/https%3A%2F%2Fexample.com%2Fstatus%3Fq%3D1?method=get");

    expect(formStateFromLocation(url)).toMatchObject({
      tool: "http",
      target: "https://example.com/status?q=1",
      method: "GET"
    });
  });

  it("serializes shareable form paths", () => {
    expect(
      formStatePath({
        ...defaultFormState,
        tool: "mtr",
        target: "1.1.1.1",
        agentId: "edge-1",
        ipVersion: 4,
        protocol: "tcp",
        resolveOnAgent: false
      })
    ).toBe("/mtr?target=1.1.1.1&ip_version=4&agent_id=edge-1&protocol=tcp&resolve_on_agent=0");

    expect(
      formStatePath({
        ...defaultFormState,
        tool: "http",
        target: "https://example.com",
        method: "GET"
      })
    ).toBe("/http?target=https%3A%2F%2Fexample.com&method=GET&resolve_on_agent=1");

    expect(
      formStatePath({
        ...defaultFormState,
        tool: "dns",
        target: "example.com",
        dnsType: "AAAA"
      })
    ).toBe("/dns?target=example.com&type=AAAA");
  });

  it("serializes job result paths without exposing credentials", () => {
    expect(
      jobResultPath({
        ...defaultFormState,
        tool: "ping",
        target: "1.1.1.1"
      }, "job/1")
    ).toBe("/ping?target=1.1.1.1&protocol=icmp&resolve_on_agent=1&job_id=job%2F1");
  });

  it("restores form state from an existing job", () => {
    expect(
      formStateFromJob({
        id: "job-1",
        tool: "port",
        target: "example.com",
        args: { port: "8443" },
        ip_version: 4,
        status: "succeeded",
        created_at: "2026-04-28T00:00:00Z",
        updated_at: "2026-04-28T00:00:01Z"
      })
    ).toMatchObject({
      tool: "port",
      target: "example.com:8443",
      ipVersion: 4
    });
  });
});
