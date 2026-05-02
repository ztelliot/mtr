import { describe, expect, it } from "vitest";
import { defaultFormState } from "./jobForm";
import { dnsTypeOptions, filterAgentsByPermissions, httpMethodOptions, permissionFormError, protocolOptions, toolAllowed } from "./permissions";
import type { Agent, Permissions } from "./types";

const fullPermissions: Permissions = {
  tools: {
    dns: { allowed_args: { type: "A,AAAA,CNAME,MX,TXT,NS" }, ip_versions: [0, 4, 6], requires_agent: false },
    http: { allowed_args: { method: "GET,HEAD" }, ip_versions: [0, 4, 6], requires_agent: false },
    mtr: { allowed_args: { port: "1-65535", protocol: "icmp,tcp" }, ip_versions: [0, 4, 6], requires_agent: true },
    ping: { allowed_args: { port: "1-65535", protocol: "icmp,tcp" }, ip_versions: [0, 4, 6], requires_agent: false },
    port: { allowed_args: { port: "1-65535" }, ip_versions: [0, 4, 6], requires_agent: false },
    traceroute: { allowed_args: { port: "1-65535", protocol: "icmp,tcp" }, ip_versions: [0, 4, 6], requires_agent: true }
  },
  agents: ["*"],
  schedule_access: "write",
  manage_access: "write"
};

describe("permission helpers", () => {
  const agents: Agent[] = [
    testAgent("edge-1", ["blue"]),
    testAgent("edge-2", ["green"])
  ];

  it("treats allowed args as backend CSV rules", () => {
    expect(protocolOptions(fullPermissions, "ping").map((option) => option.value)).toEqual(["icmp", "tcp"]);
    expect(httpMethodOptions(fullPermissions).map((option) => option.value)).toEqual(["HEAD", "GET"]);
    expect(dnsTypeOptions(fullPermissions)).toEqual(["A", "AAAA", "CNAME", "MX", "TXT", "NS"]);
  });

  it("treats missing allowed_args as inherited policy permissions", () => {
    const inherited = {
      ...fullPermissions,
      tools: {
        ...fullPermissions.tools,
        http: { ip_versions: [0, 4, 6], requires_agent: false }
      }
    } satisfies Permissions;

    expect(httpMethodOptions(inherited).map((option) => option.value)).toEqual(["HEAD", "GET"]);
    expect(permissionFormError({ ...defaultFormState, tool: "http", target: "https://example.com", method: "GET" }, inherited, agents, testTranslate)).toBeNull();
  });

  it("allows a ping form with CSV protocol permissions", () => {
    expect(
      permissionFormError(
        { ...defaultFormState, tool: "ping", target: "1.1.1.1", protocol: "icmp", ipVersion: 4 },
        fullPermissions,
        agents,
        testTranslate
      )
    ).toBeNull();
  });

  it("treats port allowed args as backend port range rules", () => {
    expect(
      permissionFormError(
        { ...defaultFormState, tool: "port", target: "example.com:443" },
        { ...fullPermissions, tools: { ...fullPermissions.tools, port: { allowed_args: { port: "1-1024" }, ip_versions: [0, 4, 6], requires_agent: false } } },
        agents,
        testTranslate
      )
    ).toBeNull();
    expect(
      permissionFormError(
        { ...defaultFormState, tool: "port", target: "example.com:8443" },
        { ...fullPermissions, tools: { ...fullPermissions.tools, port: { allowed_args: { port: "1-1024" }, ip_versions: [0, 4, 6], requires_agent: false } } },
        agents,
        testTranslate
      )
    ).toBe("errors.optionNotAllowed");
  });

  it("does not allow tools before permissions load", () => {
    expect(toolAllowed(null, "ping")).toBe(false);
  });

  it("filters agents with tag and deny permission scopes", () => {
    const agents: Agent[] = [
      testAgent("edge-1", ["blue"]),
      testAgent("edge-2", ["blue", "blocked"]),
      testAgent("edge-3", ["green"])
    ];
    expect(
      filterAgentsByPermissions(agents, {
        ...fullPermissions,
        agents: ["tag:blue", "!tag:blocked", "!edge-3"]
      }).map((agent) => agent.id)
    ).toEqual(["edge-1"]);
  });

  it("treats deny-only agent scopes as all except denied", () => {
    const agents: Agent[] = [
      testAgent("edge-1", ["blue"]),
      testAgent("edge-2", ["blocked"])
    ];
    expect(
      filterAgentsByPermissions(agents, {
        ...fullPermissions,
        agents: ["!tag:blocked"]
      }).map((agent) => agent.id)
    ).toEqual(["edge-1"]);
  });

  it("validates manually typed agent IDs against tag scopes using loaded agent labels", () => {
    const permissions = {
      ...fullPermissions,
      agents: ["tag:blue"]
    };
    expect(permissionFormError({ ...defaultFormState, tool: "ping", target: "1.1.1.1", agentId: "edge-1" }, permissions, agents, testTranslate)).toBeNull();
    expect(permissionFormError({ ...defaultFormState, tool: "ping", target: "1.1.1.1", agentId: "edge-2" }, permissions, agents, testTranslate)).toBe("errors.agentNotAllowed");
    expect(permissionFormError({ ...defaultFormState, tool: "ping", target: "1.1.1.1", agentId: "edge-3" }, permissions, agents, testTranslate)).toBe("errors.agentNotAllowed");
  });
});

function testTranslate(key: string): string {
  return key;
}

function testAgent(id: string, labels: string[]): Agent {
  return {
    id,
    labels,
    capabilities: ["ping"],
    protocols: 3,
    status: "online",
    last_seen_at: "",
    created_at: ""
  };
}
