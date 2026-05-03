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

  it("treats an empty token IP version selection as backend default access", () => {
    expect(__managePageTest.normalizeIPVersions([])).toBeUndefined();
    expect(__managePageTest.normalizeIPVersions(["0", "4", "6"])).toBeUndefined();
    expect(__managePageTest.normalizeIPVersions(["4"])).toEqual([4]);
  });

  it("drops DNS token IP version restrictions because DNS jobs do not submit ip_version", () => {
    expect(__managePageTest.normalizeTokenToolScope("dns", { ip_versions: [4] }).ip_versions).toBeUndefined();
    expect(__managePageTest.normalizeTokenToolScope("ping", { ip_versions: [4] }).ip_versions).toEqual([4]);
  });

  it("requires HTTP agent token before submitting to match backend validation", () => {
    expect(__managePageTest.httpAgentRequiredFieldsMissing({
      id: "edge-http",
      enabled: true,
      base_url: "https://edge.example.com",
      http_token: "",
      labels: []
    })).toEqual({ id: false, baseURL: false, token: true });
    expect(__managePageTest.httpAgentRequiredFieldsMissing({
      id: "edge-http",
      enabled: true,
      base_url: "https://edge.example.com",
      http_token: " secret ",
      labels: []
    })).toEqual({ id: false, baseURL: false, token: false });
  });

  it("does not mark saved HTTP agent configs online until backend reports live state", () => {
    expect(__managePageTest.managedHTTPAgentFromConfig({
      id: "edge-http",
      enabled: true,
      base_url: "https://edge.example.com",
      http_token: "secret",
      labels: []
    }).status).toBe("offline");
    expect(__managePageTest.managedHTTPAgentFromConfig({
      id: "edge-http",
      enabled: true,
      base_url: "https://edge.example.com",
      http_token: "secret",
      labels: []
    }, { ...managedAgent("edge-http", ["agent", "agent:http", "id:edge-http"]), status: "online", transport: "http", type: "http" }).status).toBe("online");
  });

  it("does not materialize policy defaults when toggling enabled state", () => {
    expect(__managePageTest.setPolicyEnabled(undefined, false)).toEqual({ enabled: false });
    expect(__managePageTest.setPolicyEnabled({ enabled: true }, false)).toEqual({ enabled: false });
  });

  it("builds one label update per changed node id without transport", () => {
    const before: ManagedAgent[] = [
      managedAgent("edge-1", ["agent", "agent:grpc", "id:edge-1", "old"]),
      managedAgent("edge-2", ["agent", "agent:grpc", "id:edge-2", "same"])
    ];
    const after: ManagedAgent[] = [
      managedAgent("edge-1", ["agent", "agent:grpc", "id:edge-1", "new", "agent", "agent:http"]),
      managedAgent("edge-2", ["agent", "agent:grpc", "id:edge-2", "same"])
    ];

    const updates = __managePageTest.changedLabelAgents(before, after).map((agent) => ({
      id: agent.id,
      labels: __managePageTest.customLabels(agent.labels)
    }));

    expect(updates).toEqual([{ id: "edge-1", labels: ["new"] }]);
  });

  it("sorts system labels before custom labels and id labels", () => {
    const summaries = __managePageTest.labelSummaries([
      managedAgent("edge-1", ["agent", "agent:grpc", "id:edge-1", "local"]),
      { ...managedAgent("edge-http-1", ["agent", "agent:http", "id:edge-http-1", "local"]), type: "http", transport: "http" }
    ], {});

    expect(summaries.map((summary) => summary.label)).toEqual([
      "agent",
      "agent:grpc",
      "agent:http",
      "local",
      "id:edge-1",
      "id:edge-http-1"
    ]);
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
