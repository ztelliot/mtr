import { describe, expect, it } from "vitest";
import { agentRegionLabel, agentSelectLabel, flagCountryCode, ispProtocolLabel } from "./agentDisplay";
import i18n from "./i18n";
import type { Agent } from "./types";

const zhT = (key: string, options?: Record<string, unknown>) => i18n.t(key, { ...options, lng: "zh-CN" });
const enT = (key: string, options?: Record<string, unknown>) => i18n.t(key, { ...options, lng: "en-US" });

describe("agent display helpers", () => {
  it("uses HK, MO, and TW flag assets directly", () => {
    expect(flagCountryCode("HK")).toBe("HK");
    expect(flagCountryCode("MO")).toBe("MO");
    expect(flagCountryCode("TW")).toBe("TW");
  });

  it("localizes known agent regions and ISP labels", () => {
    expect(agentRegionLabel("Hong Kong", zhT)).toBe("香港");
    expect(agentRegionLabel("unknown-place", zhT)).toBe("unknown-place");
    expect(ispProtocolLabel({ isp: "QCloud", protocols: 1, t: zhT })).toBe("腾讯云 [v4]");
    expect(ispProtocolLabel({ isp: "Aliyun", protocols: 3, t: enT })).toBe("Aliyun");
  });

  it("uses localized labels in node select text", () => {
    expect(agentSelectLabel({
      id: "edge-1",
      country: "HK",
      region: "Hong Kong",
      isp: "Aliyun",
      tools: {},
      protocols: 2,
      status: "online",
      last_seen_at: "",
      created_at: ""
    } satisfies Agent, zhT)).toBe("香港 · 阿里云 [v6]");
  });
});
