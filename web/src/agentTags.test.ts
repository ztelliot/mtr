import { describe, expect, it } from "vitest";
import { agentTagCounts, agentTagLabel, filterAgentsByTags, isPolicyLabel, normalizeAgentTags, normalizeSelectableAgentTags } from "./agentTags";
import type { Agent } from "./types";

const agents: Agent[] = [
  testAgent("edge-blue", ["agent", "agent:grpc", "id:edge-blue", "africa-grpc", "blue", "shared", "shared"]),
  testAgent("edge-green", ["agent", "agent:http", "id:edge-green", "asia-http", "green", "shared"]),
  testAgent("edge-red", ["agent", "agent:grpc", "id:edge-red", "red"])
];

describe("agent tag helpers", () => {
  it("allows policies only for agent and id namespaces", () => {
    for (const label of ["agent", "agent:grpc", "agent:custom", "id:edge-blue"]) {
      expect(isPolicyLabel(label)).toBe(true);
    }
    for (const label of ["blue", "agent-blue", "agents", "region:asia"]) {
      expect(isPolicyLabel(label)).toBe(false);
    }
    expect(normalizeSelectableAgentTags(["agent", "agent:custom", "agent:grpc", "blue"])).toEqual(["blue"]);
  });

  it("normalizes tag selections without changing their order", () => {
    expect(normalizeAgentTags([" blue ", "", "green", "blue"])).toEqual(["blue", "green"]);
    expect(normalizeSelectableAgentTags([
      "blue",
      "agent",
      "agent:grpc",
      "id:edge-blue",
      "africa-grpc",
      "friendly-tag",
      "north_america",
      "green"
    ])).toEqual(["blue", "north_america", "green"]);
  });

  it("counts selectable node-set tags and excludes all-node and id labels", () => {
    expect(agentTagCounts(agents)).toEqual([
      { tag: "blue", count: 1 },
      { tag: "green", count: 1 },
      { tag: "red", count: 1 },
      { tag: "shared", count: 2 }
    ]);
  });

  it("uses OR semantics for multiple tags and defaults to every agent", () => {
    expect(filterAgentsByTags(agents, ["blue", "green"]).map((agent) => agent.id)).toEqual(["edge-blue", "edge-green"]);
    expect(filterAgentsByTags(agents, []).map((agent) => agent.id)).toEqual(["edge-blue", "edge-green", "edge-red"]);
  });

  it("localizes known tags and keeps unknown API values unchanged", () => {
    const t = (key: string) => key === "agentTags.asia" ? "亚洲" : key;
    expect(agentTagLabel(" asia ", t)).toBe("亚洲");
    expect(agentTagLabel("custom_group", t)).toBe("custom_group");
    expect(agentTagLabel("中文标签", t)).toBe("中文标签");
  });
});

function testAgent(id: string, labels: string[]): Agent {
  return {
    id,
    labels,
    tools: {},
    protocols: 3,
    status: "online",
    last_seen_at: "",
    created_at: ""
  };
}
