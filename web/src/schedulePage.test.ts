import { describe, expect, it } from "vitest";
import { defaultFormState } from "./jobForm";
import { __schedulePageTest } from "./SchedulePage";
import type { Agent, Permissions } from "./types";

const permissions: Permissions = {
  tools: {
    http: { allowed_args: { method: "GET,HEAD" }, ip_versions: [0, 4, 6], requires_agent: false }
  },
  schedule_access: "write",
  manage_access: "read"
};

describe("schedule page permission helpers", () => {
  it("allows a schedule label when any matching agent can run the form", () => {
    const agents = [
      testAgent("edge-head", ["agent", "id:edge-head", "blue"], {
        http: { allowed_args: { method: "HEAD" }, ip_versions: [0, 4, 6], requires_agent: false }
      }),
      testAgent("edge-get", ["agent", "id:edge-get", "blue"], {
        http: { allowed_args: { method: "GET" }, ip_versions: [0, 4, 6], requires_agent: false }
      })
    ];

    const error = __schedulePageTest.schedulePermissionFormError(
      { ...defaultFormState, tool: "http", target: "https://example.com", method: "GET" },
      permissions,
      agents,
      ["blue"],
      testTranslate
    );

    expect(error).toBeNull();
  });

  it("rejects a selected label when none of its matching agents can run the form", () => {
    const agents = [
      testAgent("edge-head", ["agent", "id:edge-head", "blue"], {
        http: { allowed_args: { method: "HEAD" }, ip_versions: [0, 4, 6], requires_agent: false }
      }),
      testAgent("edge-get", ["agent", "id:edge-get", "green"], {
        http: { allowed_args: { method: "GET" }, ip_versions: [0, 4, 6], requires_agent: false }
      })
    ];

    const error = __schedulePageTest.schedulePermissionFormError(
      { ...defaultFormState, tool: "http", target: "https://example.com", method: "GET" },
      permissions,
      agents,
      ["blue", "green"],
      testTranslate
    );

    expect(error).toBe("errors.optionNotAllowed");
  });

  it("filters IP version options to versions runnable for every selected label", () => {
    const agents = [
      testAgent("edge-v4", ["agent", "id:edge-v4", "blue"], {
        http: { allowed_args: { method: "GET" }, ip_versions: [0, 4], requires_agent: false }
      }),
      testAgent("edge-v6", ["agent", "id:edge-v6", "green"], {
        http: { allowed_args: { method: "GET" }, ip_versions: [0, 6], requires_agent: false }
      })
    ];

    const options = __schedulePageTest.scheduleIPVersionOptions(
      permissions,
      { ...defaultFormState, tool: "http", target: "https://example.com", method: "GET" },
      agents,
      ["blue", "green"],
      testTranslate
    );

    expect(options.map((option) => option.value)).toEqual(["0"]);
  });

  it("filters IP version options by agent protocol support even when permissions use defaults", () => {
    const agents = [
      testAgent("edge-v4", ["agent", "id:edge-v4", "blue"], {
        http: { allowed_args: { method: "GET" }, requires_agent: false }
      }, 1)
    ];

    const options = __schedulePageTest.scheduleIPVersionOptions(
      permissions,
      { ...defaultFormState, tool: "http", target: "https://example.com", method: "GET" },
      agents,
      ["blue"],
      testTranslate
    );

    expect(options.map((option) => option.value)).toEqual(["0", "4"]);
  });

  it("uses literal target IP version when validating auto IP schedule labels", () => {
    const agents = [
      testAgent("edge-v4", ["agent", "id:edge-v4", "blue"], {
        http: { allowed_args: { method: "GET" }, ip_versions: [0, 4], requires_agent: false }
      }, 1),
      testAgent("edge-v6", ["agent", "id:edge-v6", "green"], {
        http: { allowed_args: { method: "GET" }, ip_versions: [0, 6], requires_agent: false }
      }, 2)
    ];

    expect(__schedulePageTest.schedulePermissionFormError(
      { ...defaultFormState, tool: "http", target: "https://[2606:4700:4700::1111]", method: "GET", ipVersion: 0 },
      permissions,
      agents,
      ["blue"],
      testTranslate
    )).toBe("errors.ipVersionNotAllowed");
    expect(__schedulePageTest.schedulePermissionFormError(
      { ...defaultFormState, tool: "http", target: "https://[2606:4700:4700::1111]", method: "GET", ipVersion: 0 },
      permissions,
      agents,
      ["green"],
      testTranslate
    )).toBeNull();
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
