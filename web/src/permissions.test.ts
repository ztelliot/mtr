import { describe, expect, it } from "vitest";
import { defaultFormState } from "./jobForm";
import { dnsTypeOptions, filterAgentsByPermissions, httpMethodOptions, ipVersionOptions, localizedFormError, permissionFormError, protocolOptions, toolAllowed, toolAllowedForAgent } from "./permissions";
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
  schedule_access: "write",
  manage_access: "write"
};

describe("permission helpers", () => {
  const agents: Agent[] = [
    testAgent("edge-1", ["blue"], fullPermissions.tools),
    testAgent("edge-2", ["green"], fullPermissions.tools)
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

  it("filters agents by returned node tool permissions", () => {
    const agents: Agent[] = [
      testAgent("edge-1", ["blue"], { ping: fullPermissions.tools.ping }),
      testAgent("edge-2", ["blue", "blocked"], {}),
      testAgent("edge-3", ["green"], { http: fullPermissions.tools.http })
    ];
    expect(filterAgentsByPermissions(agents, fullPermissions).map((agent) => agent.id)).toEqual(["edge-1", "edge-3"]);
  });

  it("validates manually typed agent IDs against returned agent permissions", () => {
    const scopedAgents = [testAgent("edge-1", ["blue"], { ping: fullPermissions.tools.ping })];
    expect(permissionFormError({ ...defaultFormState, tool: "ping", target: "1.1.1.1", agentId: "edge-1" }, fullPermissions, scopedAgents, testTranslate)).toBeNull();
    expect(permissionFormError({ ...defaultFormState, tool: "ping", target: "1.1.1.1", agentId: "edge-2" }, fullPermissions, scopedAgents, testTranslate)).toBe("errors.agentNotAllowed");
  });

  it("uses per-agent effective tool permissions when an agent is selected", () => {
    const scopedAgents = [
      testAgent("edge-1", ["blue"], {
        http: { allowed_args: { method: "HEAD" }, ip_versions: [0, 4, 6], requires_agent: false }
      })
    ];
    expect(toolAllowedForAgent(fullPermissions, "http", scopedAgents[0])).toBe(true);
    expect(httpMethodOptions(fullPermissions, scopedAgents[0]).map((option) => option.value)).toEqual(["HEAD"]);
    expect(permissionFormError({ ...defaultFormState, tool: "http", target: "https://example.com", agentId: "edge-1", method: "GET" }, fullPermissions, scopedAgents, testTranslate)).toBe("errors.optionNotAllowed");
    expect(permissionFormError({ ...defaultFormState, tool: "http", target: "https://example.com", agentId: "edge-1", method: "HEAD" }, fullPermissions, scopedAgents, testTranslate)).toBeNull();
  });

  it("requires token and agent tool argument permissions to overlap", () => {
    const tokenAllowsICMPOnly = {
      ...fullPermissions,
      tools: {
        mtr: { allowed_args: { protocol: "icmp" }, ip_versions: [0, 4, 6], requires_agent: true }
      }
    } satisfies Permissions;
    const tcpOnlyAgent = testAgent("edge-tcp", ["blue"], {
      mtr: { allowed_args: { protocol: "tcp" }, ip_versions: [0, 4, 6], requires_agent: true }
    });
    const mixedAgent = testAgent("edge-mixed", ["blue"], {
      mtr: { allowed_args: { protocol: "icmp,tcp" }, ip_versions: [0, 4, 6], requires_agent: true }
    });

    expect(toolAllowedForAgent(tokenAllowsICMPOnly, "mtr", tcpOnlyAgent)).toBe(false);
    expect(protocolOptions(tokenAllowsICMPOnly, "mtr", tcpOnlyAgent)).toEqual([]);
    expect(filterAgentsByPermissions([tcpOnlyAgent], tokenAllowsICMPOnly)).toEqual([]);
    expect(protocolOptions(tokenAllowsICMPOnly, "mtr", mixedAgent).map((option) => option.value)).toEqual(["icmp"]);
    expect(permissionFormError({ ...defaultFormState, tool: "mtr", target: "1.1.1.1", agentId: "edge-tcp", protocol: "icmp" }, tokenAllowsICMPOnly, [tcpOnlyAgent], testTranslate)).toBe("errors.toolNotAllowed");
    expect(permissionFormError({ ...defaultFormState, tool: "mtr", target: "1.1.1.1", agentId: "edge-tcp", protocol: "tcp" }, tokenAllowsICMPOnly, [tcpOnlyAgent], testTranslate)).toBe("errors.toolNotAllowed");
  });

  it("treats empty IP version permissions as no allowed versions", () => {
    const scoped = {
      ...fullPermissions,
      tools: {
        ...fullPermissions.tools,
        ping: { allowed_args: { protocol: "icmp,tcp" }, ip_versions: [], requires_agent: false }
      }
    } satisfies Permissions;

    expect(permissionFormError({ ...defaultFormState, tool: "ping", target: "1.1.1.1", ipVersion: 4 }, scoped, agents, testTranslate)).toBe("errors.ipVersionNotAllowed");
  });

  it("checks auto IP literal targets against agent protocol support", () => {
    const v4OnlyAgents = [testAgent("edge-v4", ["blue"], fullPermissions.tools, 1)];

    expect(permissionFormError({ ...defaultFormState, tool: "ping", target: "2606:4700:4700::1111", ipVersion: 0, agentId: "edge-v4" }, fullPermissions, v4OnlyAgents, testTranslate)).toBe("errors.ipVersionNotAllowed");
    expect(permissionFormError({ ...defaultFormState, tool: "ping", target: "2606:4700:4700::1111", ipVersion: 6, agentId: "edge-v4" }, fullPermissions, v4OnlyAgents, testTranslate)).toBe("errors.ipVersionNotAllowed");
    expect(permissionFormError({ ...defaultFormState, tool: "http", target: "https://[2606:4700:4700::1111]", ipVersion: 0, agentId: "edge-v4" }, fullPermissions, v4OnlyAgents, testTranslate)).toBe("errors.ipVersionNotAllowed");
    expect(permissionFormError({ ...defaultFormState, tool: "ping", target: "1.1.1.1", ipVersion: 0, agentId: "edge-v4" }, fullPermissions, v4OnlyAgents, testTranslate)).toBeNull();
    expect(permissionFormError({ ...defaultFormState, tool: "ping", target: "::ffff:1.1.1.1", ipVersion: 0, agentId: "edge-v4" }, fullPermissions, v4OnlyAgents, testTranslate)).toBeNull();
    expect(permissionFormError({ ...defaultFormState, tool: "ping", target: "0:0:0:0:0:ffff:101:101", ipVersion: 0, agentId: "edge-v4" }, fullPermissions, v4OnlyAgents, testTranslate)).toBeNull();
    expect(permissionFormError({ ...defaultFormState, tool: "http", target: "https://[::ffff:1.1.1.1]", ipVersion: 0, agentId: "edge-v4" }, fullPermissions, v4OnlyAgents, testTranslate)).toBeNull();
  });

  it("filters default IP version options by agent protocol support", () => {
    const v4OnlyAgent = testAgent("edge-v4", ["blue"], {
      ping: { allowed_args: { protocol: "icmp,tcp" }, requires_agent: false }
    }, 1);

    expect(ipVersionOptions(fullPermissions, "ping", v4OnlyAgent).map((option) => option.value)).toEqual(["0", "4"]);
  });

  it("aligns target validation with backend policy rules", () => {
    expect(localizedFormError({ ...defaultFormState, tool: "http", target: "example.com" }, fullPermissions, testTranslate)).toBe("errors.httpTargetRequired");
    expect(localizedFormError({ ...defaultFormState, tool: "ping", target: "example.com;id" }, fullPermissions, testTranslate)).toBe("errors.targetForbiddenChars");
    expect(localizedFormError({ ...defaultFormState, tool: "ping", target: "example..com" }, fullPermissions, testTranslate)).toBe("errors.targetHostRequired");
    expect(localizedFormError({ ...defaultFormState, tool: "ping", target: "[2606:4700:4700::1111]" }, fullPermissions, testTranslate)).toBe("errors.targetHostRequired");
    expect(localizedFormError({ ...defaultFormState, tool: "ping", target: "127.0.0.1" }, fullPermissions, testTranslate)).toBe("errors.targetAddressNotAllowed");
    expect(localizedFormError({ ...defaultFormState, tool: "ping", target: "2606:4700:4700::1111", ipVersion: 4 }, fullPermissions, testTranslate)).toBe("errors.targetAddressNotIPv4");
    expect(localizedFormError({ ...defaultFormState, tool: "http", target: "https://[::ffff:127.0.0.1]" }, fullPermissions, testTranslate)).toBe("errors.targetAddressNotAllowed");
    expect(localizedFormError({ ...defaultFormState, tool: "ping", target: "0:0:0:0:0:ffff:127.0.0.1" }, fullPermissions, testTranslate)).toBe("errors.targetAddressNotAllowed");
  });
});

function testTranslate(key: string): string {
  return key;
}

function testAgent(id: string, labels: string[], tools: Agent["tools"], protocols = 3): Agent {
  return {
    id,
    labels,
    tools,
    protocols,
    status: "online",
    last_seen_at: "",
    created_at: ""
  };
}
