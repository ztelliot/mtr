import { describe, expect, it } from "vitest";
import { __managePageTest } from "./ManagePage";
import type { APITokenPermission, ManagedAgent, OutboundAgentConfig } from "./types";

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
    expect(__managePageTest.displayTokenToolScope("http", editable.tools?.http).allowed_args).toEqual({ method: "GET,HEAD", http_version: "auto,3,2,1.1" });
    expect(__managePageTest.normalizeToken(editable).tools?.http?.allowed_args).toBeUndefined();
  });

  it("does not materialize defaults when selecting a new token tool", () => {
    const selected = __managePageTest.selectTokenTools({}, ["dns"]);

    expect(selected.dns).toEqual({});
    expect(__managePageTest.displayTokenToolScope("dns", selected.dns).allowed_args).toEqual({ type: "A,AAAA,CNAME,MX,TXT,NS" });
  });

  it("does not invent an HTTP version for method-only access", () => {
    const raw = { allowed_args: { method: "HEAD" } };

    expect(__managePageTest.displayTokenToolScope("http", raw).allowed_args).toEqual({ method: "HEAD" });
    expect(__managePageTest.normalizeTokenToolScope("http", raw).allowed_args).toEqual({ method: "HEAD" });
    expect(__managePageTest.updateAllowedArg("http", raw.allowed_args, "method", "GET")).toEqual({ method: "GET" });
  });

  it("keeps raw policy args separate from editable effective defaults", () => {
    expect(__managePageTest.withPolicyDefaults("http", { enabled: true, allowed_args: { method: "HEAD" } })).toEqual({
      enabled: true,
      allowed_args: { method: "HEAD" }
    });
    expect(__managePageTest.effectiveAllowedArgs("http", { method: "HEAD" })).toEqual({ method: "HEAD" });
    expect(__managePageTest.updateAllowedArg("http", undefined, "method", "HEAD")).toEqual({
      method: "HEAD",
      http_version: "auto,3,2,1.1"
    });
  });

  it("normalizes and de-duplicates register token Agent IDs", () => {
    expect(__managePageTest.normalizeRegisterAgentIDs([" edge-1 ", "", "edge-1", "edge-2"])).toEqual(["edge-1", "edge-2"]);
  });

  it("tracks all register token fields behind one row-level save", () => {
    const token = { token: "secret", name: "edge", remaining_uses: 1, allowed_agent_ids: ["edge-1"] };
    expect(__managePageTest.registerTokenDraftChanged(token, { name: "edge", remainingUses: 1, allowedAgentIDs: ["edge-1"] })).toBe(false);
    expect(__managePageTest.registerTokenDraftChanged(token, { name: "edge", remainingUses: 2, allowedAgentIDs: ["edge-1"] })).toBe(true);
    expect(__managePageTest.registerTokenDraftChanged(token, { name: "edge", remainingUses: 1, allowedAgentIDs: ["edge-1", "edge-2"] })).toBe(true);
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

  it("requires outbound authentication fields according to their type", () => {
    const bearer: OutboundAgentConfig = {
      id: "edge-http",
      transport: "http",
      enabled: true,
      url: "https://edge.example.com",
      auth: { type: "bearer", secret: "" },
      labels: []
    };
    expect(__managePageTest.outboundAgentRequiredFieldsMissing(bearer)).toEqual({ id: false, url: false, header: false, accessKey: false, projectID: false, secret: true, eventResponse: false });
    expect(__managePageTest.outboundAgentRequiredFieldsMissing(bearer, "edge-http", true, bearer).secret).toBe(false);
    expect(__managePageTest.outboundAgentRequiredFieldsMissing({
      id: "edge-event",
      transport: "event",
      enabled: true,
      url: "https://functions.example.com/invoke",
      auth: { type: "header", header: "X-Auth-Token", secret: " secret " },
      event_response: "result_field",
      labels: []
    })).toEqual({ id: false, url: false, header: false, accessKey: false, projectID: false, secret: false, eventResponse: false });
    expect(__managePageTest.outboundAgentRequiredFieldsMissing({
      id: "edge-function",
      transport: "event",
      enabled: true,
      url: "https://functiongraph.example.com/invoke",
      auth: { type: "huaweicloud_aksk", access_key: "", project_id: "", secret: "" },
      event_response: "result_field",
      labels: []
    })).toEqual({ id: false, url: false, header: false, accessKey: true, projectID: true, secret: true, eventResponse: false });
    const huaweiCloud: OutboundAgentConfig = {
      id: "edge-function",
      transport: "event",
      enabled: true,
      url: "https://functiongraph.example.com/invoke",
      auth: { type: "huaweicloud_aksk", access_key: "AK", project_id: "project", secret: "" },
      event_response: "result_field",
      labels: []
    };
    expect(__managePageTest.outboundAgentRequiredFieldsMissing(huaweiCloud, "edge-function", true, huaweiCloud).secret).toBe(false);
    expect(__managePageTest.outboundAgentRequiredFieldsMissing(huaweiCloud, "edge-function", true, bearer).secret).toBe(true);
  });

  it("retains a redacted outbound secret only for the backend credential scope", () => {
    const original: OutboundAgentConfig = {
      id: "edge-function",
      transport: "event",
      enabled: true,
      url: "https://functiongraph.example.com/invoke",
      auth: { type: "huaweicloud_aksk", access_key: "AK", project_id: "project", secret: "" },
      headers: { "X-CFF-Request-Version": "v1" },
      event_response: "result_field",
      labels: ["old"],
      tls: { enabled: true, ca_files: ["ca.pem"], cert_file: "client.pem", key_file: "client-key.pem", server_name: "functiongraph.example.com" }
    };
    const clone = (): OutboundAgentConfig => ({
      ...original,
      auth: { ...original.auth },
      headers: { ...original.headers },
      labels: [...(original.labels ?? [])],
      tls: { ...original.tls, ca_files: [...(original.tls?.ca_files ?? [])] }
    });

    const nonScopeChange = clone();
    nonScopeChange.enabled = false;
    nonScopeChange.event_response = "direct";
    nonScopeChange.labels = ["new"];
    expect(__managePageTest.outboundCredentialScopeEqual(original, nonScopeChange)).toBe(true);
    expect(__managePageTest.outboundAgentRequiredFieldsMissing(nonScopeChange, original.id, true, original).secret).toBe(false);

    const scopeChanges: Record<string, (node: OutboundAgentConfig) => void> = {
      transport: (node) => { node.transport = "http"; },
      url: (node) => { node.url = "https://other.example.com/invoke"; },
      "auth type": (node) => { node.auth = { type: "bearer", secret: "" }; },
      "access key": (node) => { node.auth.access_key = "OTHER-AK"; },
      "project id": (node) => { node.auth.project_id = "other-project"; },
      headers: (node) => { node.headers = { "X-CFF-Request-Version": "v2" }; },
      "tls enabled": (node) => { node.tls = { ...node.tls, enabled: false }; },
      "tls ca files": (node) => { node.tls = { ...node.tls, ca_files: ["other-ca.pem"] }; },
      "tls cert file": (node) => { node.tls = { ...node.tls, cert_file: "other-client.pem" }; },
      "tls key file": (node) => { node.tls = { ...node.tls, key_file: "other-key.pem" }; },
      "tls server name": (node) => { node.tls = { ...node.tls, server_name: "other.example.com" }; }
    };
    for (const [name, mutate] of Object.entries(scopeChanges)) {
      const next = clone();
      mutate(next);
      expect(__managePageTest.outboundCredentialScopeEqual(original, next), name).toBe(false);
      expect(__managePageTest.outboundAgentRequiredFieldsMissing(next, original.id, true, original).secret, name).toBe(true);
    }

    const headerOriginal = clone();
    headerOriginal.auth = { type: "header", header: "X-Agent-Token", secret: "" };
    const headerChanged = { ...headerOriginal, auth: { ...headerOriginal.auth, header: "X-Other-Token" } };
    expect(__managePageTest.outboundCredentialScopeEqual(headerOriginal, headerChanged)).toBe(false);
    expect(__managePageTest.outboundAgentRequiredFieldsMissing(headerChanged, original.id, true, headerOriginal).secret).toBe(true);
  });

  it("serializes only fields used by the selected outbound authentication type", () => {
    expect(__managePageTest.outboundAgentAuthForSave({
      type: "huaweicloud_aksk",
      access_key: " AK ",
      project_id: " project ",
      secret: " SK ",
      header: "X-Stale"
    })).toEqual({ type: "huaweicloud_aksk", access_key: "AK", project_id: "project", secret: "SK" });
    expect(__managePageTest.outboundAgentAuthForSave({
      type: "header",
      header: " X-Agent-Token ",
      secret: " token ",
      access_key: "stale-ak",
      project_id: "stale-project"
    })).toEqual({ type: "header", header: "X-Agent-Token", secret: "token" });
    expect(__managePageTest.outboundAgentAuthForSave({
      type: "none",
      secret: "stale-secret",
      access_key: "stale-ak",
      project_id: "stale-project"
    })).toEqual({ type: "none" });
  });

  it("accepts only string-valued JSON objects for outbound request headers", () => {
    expect(__managePageTest.parseOutboundHeaders('{"X-CFF-Request-Version":"v1"}')).toEqual({ "X-CFF-Request-Version": "v1" });
    expect(__managePageTest.parseOutboundHeaders("[]")).toBeNull();
    expect(__managePageTest.parseOutboundHeaders('{"X-Test":1}')).toBeNull();
    expect(__managePageTest.parseOutboundHeaders("{")).toBeNull();
  });

  it("does not mark saved outbound agent configs online until backend reports live state", () => {
    expect(__managePageTest.managedOutboundAgentFromConfig({
      id: "edge-http",
      transport: "http",
      enabled: true,
      url: "https://edge.example.com",
      auth: { type: "bearer", secret: "secret" },
      labels: []
    }).status).toBe("offline");
    expect(__managePageTest.managedOutboundAgentFromConfig({
      id: "edge-http",
      transport: "http",
      enabled: true,
      url: "https://edge.example.com",
      auth: { type: "bearer", secret: "secret" },
      labels: []
    }, { ...managedAgent("edge-http", ["agent", "agent:http", "id:edge-http"]), status: "online", transport: "http" }).status).toBe("online");
  });

  it("lets manual disablement override the node connectivity status", () => {
    expect(__managePageTest.effectiveAgentStatus("offline", true)).toBe("disabled");
    expect(__managePageTest.effectiveAgentStatus("online", true)).toBe("disabled");
    expect(__managePageTest.effectiveAgentStatus("offline", false)).toBe("offline");
    expect(__managePageTest.effectiveAgentStatus("online", false)).toBe("online");
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
      { ...managedAgent("edge-http-1", ["agent", "agent:http", "id:edge-http-1", "local"]), transport: "http" }
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
    transport: "grpc",
    config: { id, labels }
  };
}
