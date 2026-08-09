import { describe, expect, it } from "vitest";
import { buildCreateJobRequest, defaultFormState, formStateFromJob, formStateFromLocation, formStatePath, jobResultPath, locationHasExplicitTarget, locationHasExplicitTool, navTools, normalizeFormTarget, normalizeTargetForTool, parseProbePort, supportsAgentTags, validateForm } from "./jobForm";

describe("job form", () => {
  it("defaults new HTTP diagnostics and schedules to automatic version negotiation", () => {
    expect(defaultFormState.httpVersion).toBe("auto");
    expect(buildCreateJobRequest({ ...defaultFormState, tool: "http", target: "https://example.com" })).toMatchObject({
      args: { method: "HEAD", http_version: "auto" }
    });
  });

  it("accepts only canonical probe ports", () => {
    expect(parseProbePort("80")).toBe("80");
    expect(parseProbePort("080")).toBeUndefined();
    expect(parseProbePort("+80")).toBeUndefined();
  });

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
        protocol: "tcp",
        port: "443"
      }
    });
  });

  it("does not submit a stale port for ICMP", () => {
    expect(buildCreateJobRequest({ ...defaultFormState, target: "1.1.1.1", protocol: "icmp", port: "443" })).toEqual({
      tool: "ping",
      target: "1.1.1.1",
      resolve_on_agent: true,
      args: { protocol: "icmp" }
    });
  });

  it("builds fanout payloads with normalized agent tags", () => {
    expect(buildCreateJobRequest({
      ...defaultFormState,
      target: "1.1.1.1",
      agentTags: [" blue ", "green", "blue"]
    })).toMatchObject({
      tool: "ping",
      target: "1.1.1.1",
      agent_tags: ["blue", "green"]
    });
    expect(buildCreateJobRequest({
      ...defaultFormState,
      tool: "mtr",
      target: "1.1.1.1",
      agentId: "edge-1",
      agentTags: ["blue"]
    })).not.toHaveProperty("agent_tags");
    expect(supportsAgentTags("http")).toBe(true);
    expect(supportsAgentTags("traceroute")).toBe(false);
  });

  it("does not expose internal transport tags through the workbench request", () => {
    const form = {
      ...defaultFormState,
      target: "1.1.1.1",
      agentTags: ["blue", "agent:grpc", "africa-grpc", "asia-http"]
    };
    expect(buildCreateJobRequest(form).agent_tags).toEqual(["blue"]);
    expect(formStatePath(form)).not.toContain("grpc");
    expect(formStateFromLocation(new URL("https://mtr.test/ping?agent_tag=blue&agent_tag=agent%3Agrpc&agent_tag=asia-http")).agentTags).toEqual(["blue"]);
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
      buildCreateJobRequest({ ...defaultFormState, tool: "http", target: "https://example.com", method: "GET", httpVersion: "auto", resolveOnAgent: false })
    ).toEqual({
      tool: "http",
      target: "https://example.com",
      args: { method: "GET", http_version: "auto" },
      resolve_on_agent: false
    });
  });

  it("always submits the selected HTTP version", () => {
    expect(
      buildCreateJobRequest({ ...defaultFormState, tool: "http", target: "https://example.com", httpVersion: "1.1" })
    ).toEqual({
      tool: "http",
      target: "https://example.com",
      args: { method: "HEAD", http_version: "1.1" },
      resolve_on_agent: true
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

  it("defaults the port for port checks", () => {
    expect(validateForm({ ...defaultFormState, tool: "port", target: "example.com" })).toBeNull();
    expect(buildCreateJobRequest({ ...defaultFormState, tool: "port", target: "https://example.com" })).toEqual({
      tool: "port",
      target: "example.com",
      args: { port: "443" },
      resolve_on_agent: true
    });
    expect(buildCreateJobRequest({ ...defaultFormState, tool: "port", target: "http://example.com" })).toEqual({
      tool: "port",
      target: "example.com",
      args: { port: "80" },
      resolve_on_agent: true
    });
  });

  it("normalizes target input when switching tools", () => {
    expect(normalizeTargetForTool("", "http")).toBe("");
    expect(normalizeTargetForTool("example.com", "http")).toBe("https://example.com");
    expect(normalizeTargetForTool("example.com/status?q=1", "http")).toBe("https://example.com/status?q=1");
    expect(normalizeTargetForTool("example.com", "port")).toBe("example.com:443");
    expect(normalizeTargetForTool("example.com:8443", "ping")).toBe("example.com");
    expect(normalizeTargetForTool("https://example.com:8443/path?q=1", "ping")).toBe("example.com");
    expect(normalizeTargetForTool("example.com:8080", "mtr")).toBe("example.com");
    expect(normalizeTargetForTool("https://example.com/path?q=1", "dns")).toBe("example.com");
    expect(normalizeTargetForTool("//example.com/path", "dns")).toBe("example.com");
    expect(normalizeTargetForTool("ping -c 4 example.com", "ping")).toBe("example.com");
    expect(normalizeTargetForTool("curl -I https://example.com/status", "http")).toBe("https://example.com/status");
    expect(normalizeTargetForTool("http://example.com:8080/path", "port")).toBe("example.com:8080");
    expect(normalizeTargetForTool("http://example.com", "port")).toBe("example.com:80");
    expect(normalizeTargetForTool("https://example.com", "port")).toBe("example.com:443");
    expect(normalizeTargetForTool("2001:db8::1", "port")).toBe("[2001:db8::1]:443");
    expect(normalizeTargetForTool("[2001:db8::1]:8443", "ping")).toBe("2001:db8::1");
    expect(normalizeTargetForTool("[2001:db8::1]", "port")).toBe("[2001:db8::1]:443");
  });

  it("normalizes form target for target input blur", () => {
    expect(normalizeFormTarget({ ...defaultFormState, tool: "ping", target: "https://example.com:8443/path?q=1" }).target).toBe("example.com");
    expect(normalizeFormTarget({ ...defaultFormState, tool: "port", target: "example.com" }).target).toBe("example.com:443");
    expect(normalizeFormTarget({ ...defaultFormState, tool: "http", target: "example.com/status?q=1" }).target).toBe("https://example.com/status?q=1");
  });

  it("submits cleaned target values if a raw target reaches the request builder", () => {
    expect(buildCreateJobRequest({ ...defaultFormState, tool: "ping", target: "https://example.com:8443/path?q=1" })).toMatchObject({
      tool: "ping",
      target: "example.com"
    });
  });

  it("builds form state from path and query parameters", () => {
    const url = new URL("https://mtr.test/mtr?target=2606:4700:20::681a:c1f&agent_id=edge-1&ip_version=6&protocol=tcp&port=8443&resolve_on_agent=0");

    expect(formStateFromLocation(url)).toMatchObject({
      tool: "mtr",
      target: "2606:4700:20::681a:c1f",
      agentId: "edge-1",
      ipVersion: 6,
      protocol: "tcp",
      port: "8443",
      resolveOnAgent: false
    });
  });

  it("round-trips fanout agent tags through shareable URLs", () => {
    const path = formStatePath({
      ...defaultFormState,
      target: "1.1.1.1",
      agentTags: ["blue", "green"]
    });
    expect(path).toBe("/ping?target=1.1.1.1&agent_tag=blue&agent_tag=green&protocol=icmp&resolve_on_agent=1");
    expect(formStateFromLocation(new URL(`https://mtr.test${path}`)).agentTags).toEqual(["blue", "green"]);
  });

  it("detects explicit tool URLs for readonly shared views", () => {
    expect(locationHasExplicitTool(new URL("https://mtr.test/traceroute?target=ipv6.ip.sb&job_id=job-1"))).toBe(true);
    expect(locationHasExplicitTool(new URL("https://mtr.test/jobs/job-1"))).toBe(false);
    expect(locationHasExplicitTool(new URL("https://mtr.test/?tool=mtr&target=1.1.1.1"))).toBe(false);
  });

  it("ignores removed URL aliases", () => {
    expect(formStateFromLocation(new URL("https://mtr.test/ping?host=example.com&ipVersion=6&agentId=edge-1&httpVersion=3&dns_type=AAAA&remote_dns=0"))).toMatchObject({
      tool: "ping",
      target: "",
      ipVersion: 0,
      agentId: "",
      httpVersion: "auto",
      dnsType: "A",
      resolveOnAgent: true
    });
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
        port: "8443",
        resolveOnAgent: false
      })
    ).toBe("/mtr?target=1.1.1.1&ip_version=4&agent_id=edge-1&protocol=tcp&port=8443&resolve_on_agent=0");

    expect(
      formStatePath({
        ...defaultFormState,
        tool: "http",
        target: "https://example.com",
        method: "GET",
        httpVersion: "auto"
      })
    ).toBe("/http?target=https%3A%2F%2Fexample.com&method=GET&http_version=auto&resolve_on_agent=1");

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

    expect(
      formStateFromJob({
        id: "job-2",
        tool: "traceroute",
        target: "example.com",
        args: { protocol: "udp", port: "33440" },
        agent_id: "edge-1",
        status: "succeeded",
        created_at: "2026-04-28T00:00:00Z",
        updated_at: "2026-04-28T00:00:01Z"
      })
    ).toMatchObject({
      tool: "traceroute",
      protocol: "udp",
      port: "33440"
    });
  });

  it("uses execution defaults for missing arguments instead of the current form", () => {
    const current = {
      ...defaultFormState,
      protocol: "tcp" as const,
      method: "HEAD" as const,
      httpVersion: "2" as const,
      dnsType: "AAAA" as const
    };

    expect(
      formStateFromJob({
        id: "missing-http-args",
        tool: "http",
        target: "https://example.com",
        status: "succeeded",
        created_at: "2026-04-28T00:00:00Z",
        updated_at: "2026-04-28T00:00:01Z"
      }, current)
    ).toMatchObject({ method: "GET", httpVersion: "1.1" });

    expect(
      formStateFromJob({
        id: "missing-route-args",
        tool: "traceroute",
        target: "1.1.1.1",
        agent_id: "edge-1",
        status: "succeeded",
        created_at: "2026-04-28T00:00:00Z",
        updated_at: "2026-04-28T00:00:01Z"
      }, current)
    ).toMatchObject({ protocol: "icmp" });

    expect(
      formStateFromJob({
        id: "missing-dns-args",
        tool: "dns",
        target: "example.com",
        status: "succeeded",
        created_at: "2026-04-28T00:00:00Z",
        updated_at: "2026-04-28T00:00:01Z"
      }, current)
    ).toMatchObject({ dnsType: "A" });
  });
});
