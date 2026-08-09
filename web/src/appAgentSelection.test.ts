import { describe, expect, it } from "vitest";
import { routeAgentIDReplacement } from "./App";

describe("route tool agent selection", () => {
  it("does not update an already-empty selection when no agents are available", () => {
    expect(routeAgentIDReplacement("", [])).toBeUndefined();
  });

  it("clears a stale selection once when the last agent becomes unavailable", () => {
    expect(routeAgentIDReplacement("edge-old", [])).toBe("");
    expect(routeAgentIDReplacement("", [])).toBeUndefined();
  });

  it("selects the first available agent and preserves a valid selection", () => {
    const agents = [{ id: "edge-1" }, { id: "edge-2" }];

    expect(routeAgentIDReplacement("", agents)).toBe("edge-1");
    expect(routeAgentIDReplacement("edge-2", agents)).toBeUndefined();
  });
});
