import { describe, expect, it } from "vitest";
import { __managePageTest } from "./ManagePage";
import type { APITokenPermission, ManagedAgent } from "./types";

describe("manage token helpers", () => {
  it("preserves inherited token allowed_args while showing editable defaults", () => {
    const token: APITokenPermission = {
      secret: "secret",
      tools: {
        http: {}
      }
    };

    const editable = __managePageTest.withTokenDefaults(token);
    expect(editable.tools?.http?.allowed_args).toBeUndefined();
    expect(__managePageTest.displayTokenToolScope("http", editable.tools?.http).allowed_args).toEqual({ method: "GET,HEAD" });
    expect(__managePageTest.normalizeToken(editable).tools?.http?.allowed_args).toBeUndefined();
  });

  it("does not materialize defaults when selecting a new token tool", () => {
    const selected = __managePageTest.selectTokenTools({}, ["dns"]);

    expect(selected.dns).toEqual({});
    expect(__managePageTest.displayTokenToolScope("dns", selected.dns).allowed_args).toEqual({ type: "A,AAAA,CNAME,MX,TXT,NS" });
  });

  it("does not materialize policy defaults when toggling enabled state", () => {
    expect(__managePageTest.setPolicyEnabled(undefined, false)).toEqual({ enabled: false });
    expect(__managePageTest.setPolicyEnabled({ enabled: true }, false)).toEqual({ enabled: false });
  });

  it("builds one label update per changed node id without transport", () => {
    const before: ManagedAgent[] = [
      managedAgent("edge-1", ["agent", "id:edge-1", "old"]),
      managedAgent("edge-2", ["agent", "id:edge-2", "same"])
    ];
    const after: ManagedAgent[] = [
      managedAgent("edge-1", ["agent", "id:edge-1", "new", "agent"]),
      managedAgent("edge-2", ["agent", "id:edge-2", "same"])
    ];

    const updates = __managePageTest.changedLabelAgents(before, after).map((agent) => ({
      id: agent.id,
      labels: __managePageTest.customLabels(agent.labels)
    }));

    expect(updates).toEqual([{ id: "edge-1", labels: ["new"] }]);
  });
});

function managedAgent(id: string, labels: string[]): ManagedAgent {
  return {
    id,
    labels,
    capabilities: [],
    protocols: 0,
    status: "online",
    last_seen_at: "",
    created_at: "",
    type: "grpc",
    transport: "grpc",
    config: { id, labels }
  };
}
